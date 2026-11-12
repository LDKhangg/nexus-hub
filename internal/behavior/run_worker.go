package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is the run executor: it claims pending runs with a lease and
// runs each one: pinned version, pre-checks, context through the allowed
// reads, one model call with the generic item schema, citation checks,
// classification by the engine, stored items, then the Dispatcher.

// Run failure and skip codes (behavior_runs.error_code).
const (
	CodeVersionMissing     = "version_missing"
	CodeVersionInvalid     = "version_invalid"
	CodeTriggerInvalid     = "trigger_invalid"
	CodePolicyUnavailable  = "policy_unavailable"
	CodePrecheckFailed     = "precheck_failed"
	CodeAssigneeMismatch   = "assignee_mismatch"
	CodeListMismatch       = "list_mismatch"
	CodeReadTransient      = "read_transient"
	CodeLLMUnconfigured    = "llm_unconfigured"
	CodeLLMError           = "llm_error"
	CodeLLMRefused         = "llm_refused"
	CodeInvalidOutput      = "invalid_output"
	CodeTokenBudget        = "token_budget"
	CodeStepBudget         = "step_budget"
	CodeDispatchFailed     = "dispatch_failed"
	CodeRunTimeout         = "run_timeout"
	RunToolKeyScope        = "behavior_run" // tools.Executor.KeyScope of behavior runs
	maxRetainedOutputBytes = 64 << 10
)

// MaxRunAttempts bounds how often one run is tried.
const MaxRunAttempts = 5

var runBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// RunExecStore is what the executor needs from storage
// (storage.BehaviorStore). Writes are fenced by the claim token and return
// ErrStaleClaim when another worker owns the run.
type RunExecStore interface {
	ClaimRun(ctx context.Context, now time.Time, lease time.Duration, maxAttempts int) (*Run, error)
	AddUsage(ctx context.Context, runID int64, claimToken string, u Usage) error
	FinishRun(ctx context.Context, runID int64, claimToken string, status RunStatus, errorCode string) error
	RetryRun(ctx context.Context, runID int64, claimToken string, next time.Time, errorCode string) error
	ListItems(ctx context.Context, workspaceID string, runID int64) ([]Item, error)
	// SetRunContext stores the run's context snapshot.
	SetRunContext(ctx context.Context, runID int64, claimToken string, snapshot json.RawMessage) error
	// AddRunItems stores all items of the run in one transaction.
	AddRunItems(ctx context.Context, workspaceID string, runID int64, claimToken string, items []NewItem) ([]Item, error)
}

// PolicySource returns the tool policy (grants) and ClickUp credential
// reference a run uses: the agent configuration of the workspace's ClickUp
// source. integrationID is the integration the trigger came through.
type PolicySource interface {
	RunPolicy(ctx context.Context, workspaceID, integrationID string) (tenant.AgentConfig, error)
}

// ModelSource returns the workspace's LLM provider.
type ModelSource func(ctx context.Context, workspaceID string) (domain.LLMProvider, error)

// CallerFactory builds the tool caller of one run attempt. policy holds
// only the tools the run may call; outputCap and maxCalls bound it.
type CallerFactory func(run Run, policy tenant.ToolPolicyConfig, clickUpTokenRef string, outputCap, maxCalls int) ToolCaller

// ExecutorFactory returns a CallerFactory over the Phase 2 executor: the
// run's workspace, policy, audit (store) and caps; reads only, no writes,
// nothing dangerous. Keys are scoped to behavior runs (RunToolKeyScope).
func ExecutorFactory(reg *tools.Registry, store tools.ToolRunStore, san tools.Sanitizer, base tools.Limits) CallerFactory {
	return func(run Run, policy tenant.ToolPolicyConfig, ref string, outputCap, maxCalls int) ToolCaller {
		limits := base
		limits.OutputCap = outputCap
		limits.MaxCalls = maxCalls
		limits.MaxDangerous = 0
		e := tools.NewExecutor(reg, policy, limits, false, store, san, run.ID, run.WorkspaceID)
		e.ClickUpTokenRef = ref
		e.KeyScope = RunToolKeyScope
		return e
	}
}

// RunWorkerDeps are the executor's collaborators.
type RunWorkerDeps struct {
	Runs       RunExecStore
	Defs       DefinitionStore
	Profiles   ProfileStore
	Policies   PolicySource
	Models     ModelSource
	Callers    CallerFactory
	Dispatcher Dispatcher // nil: NopDispatcher
	Notifier   Notifier   // nil: LogNotifier
	Redactor   *agent.Redactor
}

// RunWorkerSettings tune the executor.
type RunWorkerSettings struct {
	Workers          int           // concurrent runs; default 1
	PollInterval     time.Duration // idle wait; default 1s
	Lease            time.Duration // claim lease; default 15m, longer than RunTimeout
	RunTimeout       time.Duration // one attempt; default 10m
	DefaultModel     string        // when neither behavior nor profile sets one
	DefaultMaxTokens int           // run token budget when neither sets one
	MaxOutputTokens  int           // cap of one model reply
}

// RunWorker executes behavior runs.
type RunWorker struct {
	d   RunWorkerDeps
	s   RunWorkerSettings
	now func() time.Time
}

// NewRunWorker applies safe defaults.
func NewRunWorker(d RunWorkerDeps, s RunWorkerSettings) *RunWorker {
	if d.Dispatcher == nil {
		d.Dispatcher = NopDispatcher{}
	}
	if d.Notifier == nil {
		d.Notifier = LogNotifier{}
	}
	if d.Redactor == nil {
		d.Redactor = agent.NewRedactor(nil)
	}
	if s.Workers < 1 {
		s.Workers = 1
	}
	if s.PollInterval <= 0 {
		s.PollInterval = time.Second
	}
	if s.RunTimeout <= 0 {
		s.RunTimeout = 10 * time.Minute
	}
	if s.Lease <= s.RunTimeout {
		s.Lease = s.RunTimeout + 5*time.Minute
	}
	if s.DefaultMaxTokens <= 0 {
		s.DefaultMaxTokens = 100000
	}
	if s.MaxOutputTokens <= 0 {
		s.MaxOutputTokens = 6000
	}
	return &RunWorker{d: d, s: s, now: time.Now}
}

// Run polls until ctx is done with the configured number of workers.
func (w *RunWorker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < w.s.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func (w *RunWorker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.ProcessNext(ctx)
		if err != nil && ctx.Err() == nil {
			log.Print("behavior runs: storage operation failed; run retained for lease recovery")
		}
		if worked && err == nil {
			continue
		}
		t := time.NewTimer(w.s.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// ProcessNext claims and executes one due run. worked is false when no run
// was due. An error is a storage failure: the run keeps its lease and is
// recovered after it ends.
func (w *RunWorker) ProcessNext(ctx context.Context) (worked bool, err error) {
	run, err := w.d.Runs.ClaimRun(ctx, w.now(), w.s.Lease, MaxRunAttempts)
	if err != nil || run == nil {
		return false, err
	}
	err = w.execute(ctx, run)
	if errors.Is(err, ErrStaleClaim) {
		log.Printf("behavior run %d corr=%s: claim is stale; dropped", run.ID, run.CorrelationID)
		return true, nil
	}
	return true, err
}

// runOutcome ends an attempt: finish (done, skipped or failed) or retry.
type runOutcome struct {
	status RunStatus // RunDone, RunSkipped, RunFailed; "" with retry
	retry  bool
	code   string
}

// maxSnapshotJSONBytes keeps the encoded snapshot under the storage limit
// (1 MiB) even when escaping inflates the text.
const maxSnapshotJSONBytes = 900 << 10

// fitSnapshot encodes the snapshot, dropping trailing lines until it fits.
func fitSnapshot(snap *ContextSnapshot, limit int) ([]byte, error) {
	for {
		raw, err := json.Marshal(snap)
		if err != nil || len(raw) <= limit {
			return raw, err
		}
		snap.Truncated = true
		last := len(snap.Fragments) - 1
		if last < 0 {
			return raw, nil
		}
		f := &snap.Fragments[last]
		if len(f.Lines) <= 1 {
			snap.Fragments = snap.Fragments[:last]
		} else {
			f.Lines = f.Lines[:len(f.Lines)/2]
		}
	}
}

func runFail(code string) *runOutcome  { return &runOutcome{status: RunFailed, code: code} }
func runSkip(code string) *runOutcome  { return &runOutcome{status: RunSkipped, code: code} }
func runRetry(code string) *runOutcome { return &runOutcome{retry: true, code: code} }

func (w *RunWorker) execute(ctx context.Context, run *Run) error {
	rctx, cancel := context.WithTimeout(ctx, w.s.RunTimeout)
	defer cancel()
	rc := RunContext{Run: *run}
	log.Printf("behavior run %d corr=%s: attempt %d", run.ID, run.CorrelationID, run.Attempts)
	out, err := w.attempt(rctx, &rc)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrStaleClaim) {
			return err
		}
		if rctx.Err() != nil {
			out = runRetry(CodeRunTimeout)
		} else {
			return err
		}
	}
	return w.end(ctx, rc, out)
}

// end records the attempt's outcome; a retry past the attempt limit fails.
func (w *RunWorker) end(ctx context.Context, rc RunContext, out *runOutcome) error {
	run := rc.Run
	if out.retry {
		if run.Attempts < MaxRunAttempts {
			delay := runBackoff[min(max(run.Attempts-1, 0), len(runBackoff)-1)]
			log.Printf("behavior run %d corr=%s: retry (%s)", run.ID, run.CorrelationID, out.code)
			return w.d.Runs.RetryRun(ctx, run.ID, run.ClaimToken, w.now().Add(delay), out.code)
		}
		out = runFail(out.code)
	}
	if err := w.d.Runs.FinishRun(ctx, run.ID, run.ClaimToken, out.status, out.code); err != nil {
		return err
	}
	if out.code != "" {
		log.Printf("behavior run %d corr=%s: %s (%s)", run.ID, run.CorrelationID, out.status, out.code)
	} else {
		log.Printf("behavior run %d corr=%s: %s", run.ID, run.CorrelationID, out.status)
	}
	if out.status == RunFailed {
		if err := w.d.Notifier.RunFailed(ctx, rc, out.code); err != nil && ctx.Err() == nil {
			log.Printf("behavior run %d corr=%s: failure notice not sent", run.ID, run.CorrelationID)
		}
	}
	return nil
}

// attempt runs one attempt. A returned error is a storage failure or a
// cancellation; every other end is an outcome.
func (w *RunWorker) attempt(ctx context.Context, rc *RunContext) (*runOutcome, error) {
	run := rc.Run
	v, err := w.d.Defs.GetVersionByID(ctx, run.WorkspaceID, run.VersionID)
	if errors.Is(err, ErrNotFound) {
		return runFail(CodeVersionMissing), nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := ResolveVersion(v)
	if err != nil {
		return runFail(CodeVersionInvalid), nil
	}
	profile, err := w.d.Profiles.GetProfile(ctx, run.WorkspaceID)
	if err != nil {
		return nil, err
	}
	rc.Profile = profile
	rc.Doc = w.effectiveDoc(doc, v, profile)
	trig, err := DecodeTriggerData(run.Trigger)
	if err != nil {
		return runFail(CodeTriggerInvalid), nil
	}
	rc.Trigger = trig
	if snap, err := DecodeContextSnapshot(run.Context); err == nil {
		rc.Context = snap
	}

	// Items stored by an earlier attempt: only the hand-off is left.
	items, err := w.d.Runs.ListItems(ctx, run.WorkspaceID, run.ID)
	if err != nil {
		return nil, err
	}
	if len(items) > 0 {
		return w.dispatch(ctx, *rc, items), nil
	}

	cfg, err := w.d.Policies.RunPolicy(ctx, run.WorkspaceID, trig.IntegrationID)
	if err != nil {
		return runFail(CodePolicyUnavailable), nil
	}
	caller := w.caller(run, rc.Doc, cfg, trig.NeedsAssigneeCheck || trig.NeedsListCheck)
	col := newCollector(caller, run.Subject, grantedRepos(cfg.Tools))
	if out := w.precheck(ctx, col, rc.Doc, trig); out != nil {
		return out, ctx.Err()
	}
	snap, err := col.collect(ctx, rc.Doc.Reads)
	var re *readError
	if errors.As(err, &re) {
		return runRetry(CodeReadTransient), nil
	}
	if err != nil {
		return nil, err
	}
	// The executor already redacts tool output; redact again so nothing
	// secret-shaped is stored or sent even if a tool skipped it.
	for i := range snap.Fragments {
		for j, l := range snap.Fragments[i].Lines {
			snap.Fragments[i].Lines[j] = w.d.Redactor.RedactSecrets(l)
		}
	}
	raw, err := fitSnapshot(&snap, maxSnapshotJSONBytes)
	if err != nil {
		return nil, err
	}
	rc.Context = snap
	if err := w.d.Runs.SetRunContext(ctx, run.ID, run.ClaimToken, raw); err != nil {
		return nil, err
	}

	modelItems, out, err := w.askModel(ctx, rc)
	if err != nil || out != nil {
		return out, err
	}
	stored, err := w.d.Runs.AddRunItems(ctx, run.WorkspaceID, run.ID, run.ClaimToken, w.classify(rc, modelItems))
	if err != nil {
		return nil, err
	}
	return w.dispatch(ctx, *rc, stored), nil
}

func (w *RunWorker) dispatch(ctx context.Context, rc RunContext, items []Item) *runOutcome {
	if err := w.d.Dispatcher.Dispatch(ctx, rc, items); err != nil {
		return runRetry(CodeDispatchFailed)
	}
	return &runOutcome{status: RunDone}
}

// effectiveDoc applies the profile defaults: a field the behavior sets
// wins, then the profile, then the engine or settings default.
func (w *RunWorker) effectiveDoc(doc ResolvedDoc, v Version, p Profile) ResolvedDoc {
	var explicit BudgetsDoc
	if d, err := DecodeDocument(v.Document); err == nil && d.Budgets != nil {
		explicit = *d.Budgets
	}
	var prof BudgetsDoc
	if len(p.Budgets) > 0 {
		_ = json.Unmarshal(p.Budgets, &prof)
	}
	pick := func(set *int, profile *int, lo, hi, fallback int) int {
		if set != nil {
			return *set
		}
		if profile != nil && *profile >= lo && *profile <= hi {
			return *profile
		}
		return fallback
	}
	doc.Budgets.MaxTokens = pick(explicit.MaxTokens, prof.MaxTokens, 1, MaxRunTokens, w.s.DefaultMaxTokens)
	doc.Budgets.MaxItems = pick(explicit.MaxItems, prof.MaxItems, 1, 100, doc.Budgets.MaxItems)
	doc.Budgets.MaxSteps = pick(explicit.MaxSteps, prof.MaxSteps, 1, 20, doc.Budgets.MaxSteps)
	if doc.Model == "" {
		doc.Model = p.DefaultModel
	}
	if doc.Model == "" {
		doc.Model = w.s.DefaultModel
	}
	if doc.Language == "" {
		doc.Language = p.Language
	}
	if doc.Language == "" {
		doc.Language = DefaultProfile().Language
	}
	if doc.Tone == "" {
		doc.Tone = p.Tone
	}
	return doc
}

// caller builds the run's tool caller. The policy keeps the workspace's
// grants but enables only the behavior's reads (plus clickup_get_task when
// the trigger needs a pre-check) that the workspace itself enables, and
// allows no write. memory_search is enabled by the behavior's reads alone:
// it reads only the workspace's own decision memory and needs no grant or
// credential.
func (w *RunWorker) caller(run Run, doc ResolvedDoc, cfg tenant.AgentConfig, precheck bool) ToolCaller {
	want := map[string]bool{precheckTool: precheck}
	outputCap := DefaultReadBytes
	for _, r := range doc.Reads {
		want[r.Tool] = true
		outputCap = max(outputCap, readCap(r))
	}
	policy := cfg.Tools
	policy.Enabled = nil
	for _, name := range cfg.Tools.Enabled {
		if want[name] && name != MemorySearchTool {
			policy.Enabled = append(policy.Enabled, name)
		}
	}
	if want[MemorySearchTool] {
		policy.Enabled = append(policy.Enabled, MemorySearchTool)
	}
	policy.MaxWritesPerJob = 0
	policy.DryRun = true
	repos := make(map[string]tenant.RepoGrant, len(cfg.Tools.Repos))
	for k, g := range cfg.Tools.Repos {
		g.Allow = nil // no write or dangerous tool is allowed for any repo
		repos[k] = g
	}
	policy.Repos = repos
	// One pre-check, the subject reads, each read of each reference and
	// the memory search.
	maxCalls := 3 + len(doc.Reads)*(2*MaxRefsPerKind+1)
	return w.d.Callers(run, policy, cfg.ClickUpTokenRef, outputCap+64, maxCalls)
}

func grantedRepos(p tenant.ToolPolicyConfig) []string {
	out := make([]string, 0, len(p.Repos))
	for k := range p.Repos {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// precheckTool reads the subject task for pre-checks.
const precheckTool = "clickup_get_task"

// precheck verifies what the trigger could not see (assignee, list) on a
// fresh read of the subject task, before any model work. nil means go on.
func (w *RunWorker) precheck(ctx context.Context, col *collector, doc ResolvedDoc, t TriggerData) *runOutcome {
	if !t.NeedsAssigneeCheck && !t.NeedsListCheck {
		return nil
	}
	if col.taskID == "" {
		return runFail(CodePrecheckFailed)
	}
	// Use the same input as the context read so the call is made once.
	maxBytes := DefaultReadBytes
	for _, r := range doc.Reads {
		if r.Tool == precheckTool {
			maxBytes = readCap(r)
		}
	}
	res := col.call(ctx, precheckTool, taskInput(col.taskID, maxBytes))
	if ctx.Err() != nil {
		return runRetry(CodeRunTimeout)
	}
	if res.IsError {
		code := resultCode(res)
		if res.Status == tools.StatusTimeout || (res.Status != tools.StatusDenied && transientCodes[code]) {
			return runRetry(CodeReadTransient)
		}
		return runFail(CodePrecheckFailed)
	}
	list, assignees, ok := taskIdentifiers(res.Content)
	if !ok {
		return runFail(CodePrecheckFailed)
	}
	if t.NeedsListCheck && !slices.Contains(t.ListCheck, list) {
		return runSkip(CodeListMismatch)
	}
	if t.NeedsAssigneeCheck && (t.AssigneeCheck == "" || !slices.Contains(assignees, t.AssigneeCheck)) {
		return runSkip(CodeAssigneeMismatch)
	}
	return nil
}

// taskIdentifiers reads the identifier lines clickup_get_task writes first
// ("list_id: <id>" and "assignee_ids: <id>, <id>"). Only those two lines
// are read, so no task text can pass a check.
func taskIdentifiers(text string) (list string, assignees []string, ok bool) {
	ls := strings.SplitN(text, "\n", 3)
	if len(ls) < 2 {
		return "", nil, false
	}
	l, ok1 := strings.CutPrefix(ls[0], "list_id:")
	a, ok2 := strings.CutPrefix(ls[1], "assignee_ids:")
	if !ok1 || !ok2 {
		return "", nil, false
	}
	for _, id := range strings.Split(a, ",") {
		if id = strings.TrimSpace(id); id != "" {
			assignees = append(assignees, id)
		}
	}
	return strings.TrimSpace(l), assignees, true
}

// --- model ---

// llmTries is how often one model step is tried on a transient provider
// error within an attempt.
const llmTries = 2

var llmRetryDelay = 2 * time.Second

// estimateTokens is a conservative token estimate for budget checks.
func estimateTokens(s ...string) int {
	n := 0
	for _, x := range s {
		n += len(x)
	}
	return n/3 + 1
}

func totalTokens(u Usage) int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// askModel gets valid items: one call, and one more with the validation
// errors when the first output is invalid. Usage is recorded per call.
func (w *RunWorker) askModel(ctx context.Context, rc *RunContext) ([]ModelItem, *runOutcome, error) {
	run := rc.Run
	provider, err := w.d.Models(ctx, run.WorkspaceID)
	if err != nil || provider == nil {
		return nil, runFail(CodeLLMUnconfigured), nil
	}
	doc := rc.Doc
	system := systemPrompt(doc)
	msgs := []domain.Message{{Role: domain.UserRole, Text: userPrompt(doc.Instructions, rc.Context)}}
	used := run.Usage
	for try := 0; try < 2; try++ {
		if used.Steps >= doc.Budgets.MaxSteps {
			return nil, runFail(CodeStepBudget), nil
		}
		parts := []string{system}
		for _, m := range msgs {
			parts = append(parts, m.Text)
		}
		remaining := doc.Budgets.MaxTokens - totalTokens(used)
		est := estimateTokens(parts...)
		outMax := min(w.s.MaxOutputTokens, remaining-est)
		if outMax < 256 {
			return nil, runFail(CodeTokenBudget), nil
		}
		resp, err := w.generate(ctx, provider, &domain.LLMRequest{
			Model: doc.Model, System: system, MaxTokens: int64(outMax), Messages: msgs,
		})
		if err != nil {
			if errors.Is(err, domain.ErrLLMRefused) {
				return nil, runFail(CodeLLMRefused), nil
			}
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, runRetry(CodeLLMError), nil
		}
		u := Usage{
			InputTokens: int(resp.Usage.Input), OutputTokens: int(resp.Usage.Output),
			CacheReadTokens: int(resp.Usage.CacheRead), CacheWriteTokens: int(resp.Usage.CacheWrite), Steps: 1,
		}
		if err := w.d.Runs.AddUsage(ctx, run.ID, run.ClaimToken, u); err != nil {
			return nil, nil, err
		}
		used.InputTokens += u.InputTokens
		used.OutputTokens += u.OutputTokens
		used.CacheReadTokens += u.CacheReadTokens
		used.CacheWriteTokens += u.CacheWriteTokens
		used.Steps++
		rc.Run.Usage = used
		items, errs := ParseItems(resp.Text, doc.Budgets.MaxItems)
		if errs == nil {
			return items, nil, nil
		}
		log.Printf("behavior run %d corr=%s: model output invalid (%d problems, try %d)", run.ID, run.CorrelationID, len(errs), try+1)
		prev, _ := capBytes(resp.Text, maxRetainedOutputBytes)
		msgs = append(msgs, domain.Message{Role: domain.AIRole, Text: prev}, domain.Message{Role: domain.UserRole, Text: retryPrompt(errs)})
	}
	return nil, runFail(CodeInvalidOutput), nil
}

// generate calls the provider, trying again once after a short wait on a
// transient error.
func (w *RunWorker) generate(ctx context.Context, p domain.LLMProvider, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	var lastErr error
	for i := 0; i < llmTries; i++ {
		if i > 0 {
			t := time.NewTimer(llmRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		resp, err := p.Generate(ctx, req)
		if err == nil && resp != nil {
			return resp, nil
		}
		if err == nil {
			err = errors.New("behavior: empty model response")
		}
		if errors.Is(err, domain.ErrLLMRefused) || ctx.Err() != nil {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// --- classification ---

// classify checks each item's citations against the snapshot, flags
// instruction-like text and lets the engine pick the class. Item text is
// cleaned and redacted before it is stored.
func (w *RunWorker) classify(rc *RunContext, items []ModelItem) []NewItem {
	clean := func(s string) string { return w.d.Redactor.RedactSecrets(agent.CleanInput(s)) }
	out := make([]NewItem, 0, len(items))
	for _, it := range items {
		valid := make([]bool, len(it.Citations))
		texts := []string{it.Text, it.Proposal}
		for i, c := range it.Citations {
			ok, window := VerifyCitation(&rc.Context, c)
			valid[i] = ok
			texts = append(texts, window...)
			texts = append(texts, c.Quote)
		}
		cl := Classify(it, rc.Doc.Autonomy, valid, InstructionLike(texts...))
		cites := make([]Citation, len(it.Citations))
		for i, c := range it.Citations {
			cites[i] = Citation{Locator: clean(c.Locator), Quote: clean(c.Quote)}
		}
		out = append(out, NewItem{
			RunID: rc.Run.ID, Key: it.Key, Label: clean(it.Label), Text: clean(it.Text), Proposal: clean(it.Proposal),
			Citations: cites, Topic: clean(it.Topic), Confidence: it.Confidence, Informational: it.Informational,
			Flags: cl.Flags, Class: cl.Class, Rule: cl.Rule, Status: ItemDrafted,
		})
	}
	return out
}
