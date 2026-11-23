package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Owner-facing texts (Vietnamese).
const (
	btnExtendBudget = "Thêm 1 vòng"
	btnApprove      = "Làm"
	btnCancel       = "Huỷ"

	gatePrefix    = "gate:"
	clarifyPrefix = "clarify:"
	jobPrefix     = "job:"
	// jobOwnerPrefix: the step's worker job failed, needs input or was
	// interrupted; the step waits for the owner's Retry or Cancel.
	jobOwnerPrefix = "job_owner:"
	// taskWaitPrefix: an in-process executor's task waits; the executor's
	// own reference follows.
	taskWaitPrefix = "task:"
	stepMarker     = "step:"
	budgetPurpose  = "budget_"
	approvalGate   = "approval"

	maxRefBytes        = 200
	maxStoredRefs      = 16
	maxJobInstruction  = 8 << 10
	maxReportRunes     = 3500
	maxQuestionRunes   = 600
	maxResultTextBytes = 500
	maxWaitRefBytes    = 128
	maxTaskInputs      = 8
)

var reasonCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// stepRun is one attempt of one orchestrator step and the durable state it
// read.
type stepRun struct {
	s   *Steps
	ctx context.Context
	sc  workflow.StepContext
	ws  string
	id  int64
	key string

	budget    *Budget
	route     Route
	plan      *PlanRecord
	tasks     []PlanTaskRecord // of the latest plan
	decisions []DecisionRecord
	wait      string // the wait marker of the step's own checkpoint
	// prevWait keeps the wait marker the step woke from (resumeWait clears
	// wait).
	prevWait string

	taskRef string // CheckpointTask of a run slot
	jobRef  string // CheckpointWorkerJob of a run slot
	usage   workflow.Usage
}

func storageFail() error {
	return &workflow.StepError{Code: ReasonStorageUnavailable, Retryable: true}
}

func refValue(c *workflow.Checkpoint, kind string) string {
	if c == nil {
		return ""
	}
	for _, r := range c.References {
		if r.Kind == kind {
			return r.ReferenceID
		}
	}
	return ""
}

func encodeSnapshot(s BudgetSnapshot) string {
	b, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSnapshot(v string) (BudgetSnapshot, bool) {
	if v == "" {
		return BudgetSnapshot{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return BudgetSnapshot{}, false
	}
	var s BudgetSnapshot
	if json.Unmarshal(b, &s) != nil {
		return BudgetSnapshot{}, false
	}
	return s, true
}

// LatestBudgetSnapshot returns the budget snapshot of the newest step that
// has one (read-only; /status shows it). ok is false when no step has one.
func LatestBudgetSnapshot(steps []workflow.Step) (BudgetSnapshot, bool) {
	var (
		snap BudgetSnapshot
		ok   bool
		best = -1
	)
	for _, st := range steps {
		if st.Position <= best {
			continue
		}
		if v, found := decodeSnapshot(refValue(st.Checkpoint, CheckpointBudget)); found {
			snap, ok, best = v, true, st.Position
		}
	}
	return snap, ok
}

// load reads the durable state of the execution: the budget (the step's own
// checkpoint, else the newest earlier step's, else new), the route and the
// plan rows.
func (s *Steps) load(ctx context.Context, sc workflow.StepContext) (*stepRun, error) {
	r := &stepRun{s: s, ctx: ctx, sc: sc, ws: sc.Execution.WorkspaceID, id: sc.Execution.ID, key: sc.Step.Key}
	started := s.now()
	if sc.Execution.StartedAt != nil {
		started = *sc.Execution.StartedAt
	} else if !sc.Execution.CreatedAt.IsZero() {
		started = sc.Execution.CreatedAt
	}
	r.budget = NewBudget(s.Settings.Caps, started)
	r.budget.Clock = s.now
	snap, ok := decodeSnapshot(refValue(sc.Checkpoint, CheckpointBudget))
	if !ok && sc.Step.Key != StepKeyPlan {
		steps, err := s.Steps.ListSteps(ctx, r.ws, r.id)
		if err != nil {
			return nil, storageFail()
		}
		best := -1
		for _, st := range steps {
			if st.Position >= sc.Step.Position || st.Position <= best {
				continue
			}
			if v, found := decodeSnapshot(refValue(st.Checkpoint, CheckpointBudget)); found {
				snap, ok, best = v, true, st.Position
			}
		}
	}
	if ok {
		if err := r.budget.Restore(snap); err != nil {
			log.Printf("orchestrator execution %d step %s: budget snapshot not restored; using caps", r.id, r.key)
		}
	}
	r.wait = refValue(sc.Checkpoint, CheckpointWait)
	r.prevWait = r.wait
	r.taskRef = refValue(sc.Checkpoint, CheckpointTask)
	route, err := s.Routes.GetRoute(ctx, r.ws, r.id)
	if errors.Is(err, ErrRouteNotFound) {
		return nil, &workflow.StepError{Code: ReasonRouteMissing}
	}
	if err != nil {
		return nil, storageFail()
	}
	r.route = route
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// reload re-reads plans, the latest plan's tasks and the decisions.
func (r *stepRun) reload() error {
	plans, err := r.s.Plans.ListPlans(r.ctx, r.ws, r.id)
	if err != nil {
		return storageFail()
	}
	r.plan, r.tasks = nil, nil
	for i := range plans {
		if r.plan == nil || plans[i].Version > r.plan.Version {
			p := plans[i]
			r.plan = &p
		}
	}
	if r.plan != nil {
		tasks, err := r.s.Plans.ListTasks(r.ctx, r.ws, r.id)
		if err != nil {
			return storageFail()
		}
		for _, t := range tasks {
			if t.PlanID == r.plan.ID {
				r.tasks = append(r.tasks, t)
			}
		}
	}
	decisions, err := r.s.Plans.ListDecisions(r.ctx, r.ws, r.id)
	if err != nil {
		return storageFail()
	}
	r.decisions = decisions
	return nil
}

func (r *stepRun) fence() Fence {
	return Fence{JobID: r.sc.JobID, WorkspaceID: r.ws, ExecutionID: r.id, StepKey: r.key,
		Attempt: r.sc.Attempt, ClaimToken: r.sc.ClaimToken, At: r.s.now()}
}

func (r *stepRun) refs() []workflow.CheckpointReference {
	refs := []workflow.CheckpointReference{{Kind: CheckpointBudget, ReferenceID: encodeSnapshot(r.budget.Snapshot())}}
	if r.taskRef != "" {
		refs = append(refs, workflow.CheckpointReference{Kind: CheckpointTask, ReferenceID: r.taskRef})
	}
	if r.jobRef != "" {
		refs = append(refs, workflow.CheckpointReference{Kind: CheckpointWorkerJob, ReferenceID: r.jobRef})
	}
	return refs
}

func (r *stepRun) totalUsage() workflow.Usage {
	u := r.sc.Step.Usage
	u.InputTokens += r.usage.InputTokens
	u.OutputTokens += r.usage.OutputTokens
	u.CacheReadTokens += r.usage.CacheReadTokens
	u.CacheWriteTokens += r.usage.CacheWriteTokens
	return u
}

func (r *stepRun) success() (workflow.StepResult, error) {
	return workflow.StepResult{Checkpoint: &workflow.Checkpoint{References: r.refs()}, Usage: r.totalUsage()}, nil
}

// waitWorker waits for the step's worker job (or for the owner's Retry or
// Cancel of it) with a durable wake time at the end of the wall-time budget
// (Phase 10 REQ-11): the step then runs again, finds the wall time spent and
// takes the O4 outcome, so a worker that never returns cannot hold the
// execution until its 24 h deadline.
func (r *stepRun) waitWorker(state workflow.State, marker string) (workflow.StepResult, error) {
	snap := r.budget.Snapshot()
	wake := snap.StartedAt.Add(max(snap.Caps.Wall, 0))
	secs := wake.Unix()
	if wake.Nanosecond() > 0 {
		secs++ // never before the budget's end
	}
	refs := append(r.refs(),
		workflow.CheckpointReference{Kind: CheckpointWait, ReferenceID: marker},
		workflow.CheckpointReference{Kind: CheckpointWakeAt, ReferenceID: strconv.FormatInt(secs, 10)})
	return workflow.StepResult{
		Checkpoint: &workflow.Checkpoint{References: refs}, Usage: r.totalUsage(),
		WaitState: state, WaitReferences: refs,
	}, nil
}

func (r *stepRun) waitFor(state workflow.State, marker string) (workflow.StepResult, error) {
	refs := append(r.refs(), workflow.CheckpointReference{Kind: CheckpointWait, ReferenceID: marker})
	return workflow.StepResult{
		Checkpoint: &workflow.Checkpoint{References: refs}, Usage: r.totalUsage(),
		WaitState: state, WaitReferences: refs,
	}, nil
}

// failStep ends the attempt with a step error; the checkpoint keeps the
// budget so a retry resumes with the same counters.
func (r *stepRun) failStep(code string, retryable bool) (workflow.StepResult, error) {
	return workflow.StepResult{Checkpoint: &workflow.Checkpoint{References: r.refs()}, Usage: r.totalUsage()},
		&workflow.StepError{Code: workflow.ReasonCode(code), Retryable: retryable}
}

func (r *stepRun) recordUsage(u Usage) {
	r.budget.RecordUsage(u)
	r.usage.InputTokens += max(u.Tokens.Input, 0)
	r.usage.OutputTokens += max(u.Tokens.Output, 0)
	r.usage.CacheReadTokens += max(u.Tokens.CacheRead, 0)
	r.usage.CacheWriteTokens += max(u.Tokens.CacheWrite, 0)
}

// orch returns the orchestrator of this attempt: the workspace runtime, the
// repair allowance of the budget and a guard that stops repair attempts
// once the budget is spent.
func (r *stepRun) orch() (*Orchestrator, error) {
	rt, err := r.s.Runtime(r.ctx, r.ws)
	if err != nil || rt == nil {
		return nil, errors.New("orchestrator: runtime unavailable")
	}
	o := r.s.Template
	o.Runtime = rt
	o.MaxRepair = r.budget.RepairAllowance(r.s.Settings.MaxRepair)
	b := r.budget
	o.BeforeAttempt = func(spent Usage) error {
		t := spent.Tokens
		return b.CanCall(t.Input + t.Output + t.CacheRead + t.CacheWrite)
	}
	return &o, nil
}

// --- decisions ---

func (r *stepRun) marker() string { return stepMarker + r.key }

func (r *stepRun) planVersion() int {
	if r.plan == nil {
		return 1
	}
	return r.plan.Version
}

// lastOwnDecision is the newest decision this step recorded.
func (r *stepRun) lastOwnDecision() *DecisionRecord {
	for i := len(r.decisions) - 1; i >= 0; i-- {
		if slices.Contains(r.decisions[i].EvidenceRefs, r.marker()) {
			d := r.decisions[i]
			return &d
		}
	}
	return nil
}

// terminal is the newest decision when it is COMPLETE or FAIL.
func (r *stepRun) terminal() (DecisionRecord, bool) {
	if n := len(r.decisions); n > 0 {
		d := r.decisions[n-1]
		if d.Decision == DecisionComplete || d.Decision == DecisionFail {
			return d, true
		}
	}
	return DecisionRecord{}, false
}

func boundRefs(refs []string) []string {
	out := []string{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || len(ref) > maxRefBytes || !utf8.ValidString(ref) || slices.Contains(out, ref) {
			continue
		}
		if len(out) == maxStoredRefs {
			break
		}
		out = append(out, ref)
	}
	return out
}

// record stores one decision (REQ-6, REQ-7 conversions) with the step's
// marker reference.
func (r *stepRun) record(decision, reason, taskKey, convertedFrom string, evidence []string) (DecisionRecord, error) {
	if !reasonCodeRe.MatchString(reason) {
		reason = ""
	}
	if !contains(Decisions, convertedFrom) {
		convertedFrom = ""
	}
	if taskKey != "" {
		if _, ok := r.task(taskKey); !ok {
			taskKey = ""
		}
	}
	refs := boundRefs(evidence)
	if len(refs) == maxStoredRefs {
		refs = refs[:maxStoredRefs-1]
	}
	refs = append(refs, r.marker())
	rec, err := r.s.Plans.InsertDecision(r.ctx, r.ws, DecisionRecord{
		ExecutionID: r.id, PlanVersion: r.planVersion(), TaskKey: taskKey, Decision: decision,
		ReasonCode: reason, ConvertedFrom: convertedFrom, EvidenceRefs: refs,
	})
	if err != nil {
		return DecisionRecord{}, storageFail()
	}
	r.decisions = append(r.decisions, rec)
	log.Printf("orchestrator execution %d step %s: decision %s recorded", r.id, r.key, decision)
	return rec, nil
}

func (r *stepRun) setPlanStatus(status string) error {
	if r.plan == nil || r.plan.Status == status {
		return nil
	}
	if err := r.s.Plans.SetPlanStatus(r.ctx, r.ws, r.id, r.plan.Version, status); err != nil {
		return storageFail()
	}
	r.plan.Status = status
	return nil
}

// failDecision records FAIL and finishes the step; the report step ends
// the execution with the reason.
func (r *stepRun) failDecision(reason string) (workflow.StepResult, error) {
	if _, ok := r.terminal(); !ok {
		if _, err := r.record(DecisionFail, reason, "", "", nil); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
	}
	if err := r.setPlanStatus(PlanFailed); err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	return r.success()
}

// apply carries out a recorded decision. It is idempotent: a retried step
// applies its last decision again.
func (r *stepRun) apply(d DecisionRecord, text string) (workflow.StepResult, error) {
	switch d.Decision {
	case DecisionComplete:
		if err := r.setPlanStatus(PlanCompleted); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	case DecisionFail:
		if err := r.setPlanStatus(PlanFailed); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	case DecisionReplan:
		return r.replan(d.ReasonCode, d.PlanVersion)
	case DecisionRequestClarification:
		if text == "" {
			text = "kế hoạch cần thêm thông tin trước khi chạy tiếp."
		}
		return r.askClarification("plan", text, "")
	case DecisionRequestApproval:
		if text == "" {
			text = "cần bạn duyệt để chạy tiếp kế hoạch."
		}
		return r.askGate(approvalGate, fmt.Sprintf("%s %s", workflow.FormatRef(r.id), capText(text, maxQuestionRunes)),
			btnApprove, btnCancel, workflow.StateWaitingForApproval)
	}
	return r.success() // EXECUTE, DELEGATE: the next run slot continues.
}

// planError maps a failed Plan or Evaluate call.
func (r *stepRun) planError(err error) (workflow.StepResult, error) {
	errorCode, retryable := llmCallError(err)
	obs.Logger(r.ctx).Error("orchestrator llm call failed",
		"component", "orchestrator",
		"execution_id", r.id,
		"step_key", r.key,
		"attempt", r.sc.Attempt,
		"error_code", errorCode,
		"retryable", retryable,
	)

	var be *BudgetExceededError
	switch {
	case errors.As(err, &be):
		return r.budgetGate(err)
	case errors.Is(err, ErrInvalidOutput):
		return r.failDecision(ReasonInvalidOutput)
	case errors.Is(err, ErrInvalidInput):
		return r.failDecision(ReasonInvalidInputCode)
	case isBudgetExceeded(err):
		// The daily model budget is spent: a retry would be refused too.
		return r.failDecision(ReasonLLMBudgetExceeded)
	case r.ctx.Err() == nil && r.sc.Attempt >= r.sc.Definition.Retry.MaxAttempts:
		// The last attempt: end in a reported FAIL instead of a silent one.
		return r.failDecision(ReasonLLMError)
	}
	return r.failStep(ReasonLLMError, true)
}

type llmHTTPError interface {
	HTTPStatusCode() int
	IsRetryable() bool
}

func llmCallError(err error) (string, bool) {
	var httpErr llmHTTPError
	if errors.As(err, &httpErr) {
		if status := httpErr.HTTPStatusCode(); status > 0 {
			return "llm_status_" + strconv.Itoa(status), httpErr.IsRetryable()
		}
		return "llm_unknown", httpErr.IsRetryable()
	}
	if errors.Is(err, ErrInvalidOutput) {
		return "llm_invalid_output", false
	}
	var budgetErr *BudgetExceededError
	if errors.As(err, &budgetErr) || isBudgetExceeded(err) {
		return "llm_budget", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "llm_timeout", true
	}
	if llmErr, ok := domain.AsLLMError(err); ok {
		if llmErr.Code == domain.LLMCodeTimeout {
			return "llm_timeout", llmErr.Retryable
		}
		return "llm_unknown", llmErr.Retryable
	}
	return "llm_unknown", true
}

// --- owner waits ---

// budgetGate is the O4a outcome: the owner may add one round ("Thêm 1
// vòng") or cancel; no model call is made meanwhile. Without extensions
// left the execution fails at once.
func (r *stepRun) budgetGate(err error) (workflow.StepResult, error) {
	kind := BudgetTokens
	var be *BudgetExceededError
	if errors.As(err, &be) {
		kind = be.Kind
	}
	if r.budget.ExtensionsLeft() == 0 {
		return r.failDecision(ReasonPlanningBudgetExceeded)
	}
	text := fmt.Sprintf("%s đã dùng hết ngân sách (%s). Thêm 1 vòng để chạy tiếp, hoặc Huỷ.", workflow.FormatRef(r.id), kind)
	return r.askGate(budgetPurpose+string(kind), text, btnExtendBudget, btnCancel, workflow.StateWaitingForUser)
}

// newestGate is the newest gate of the step with purpose.
func (r *stepRun) newestGate(purpose string) (GateWait, bool, error) {
	waits, err := r.s.Owner.GateWaits(r.ctx, r.ws, r.id, r.key)
	if err != nil {
		return GateWait{}, false, storageFail()
	}
	var best GateWait
	found := false
	for _, w := range waits {
		if w.Purpose == purpose && (!found || w.ApprovalID > best.ApprovalID) {
			best, found = w, true
		}
	}
	return best, found, nil
}

// askGate binds a gate to the step (once) and waits for it.
func (r *stepRun) askGate(purpose, text, approve, reject string, state workflow.State) (workflow.StepResult, error) {
	g, found, err := r.newestGate(purpose)
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	if !found || (g.Decision != "" && g.Consumed) {
		ttl := r.s.Settings.GateTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		if err := r.s.Owner.AskGate(r.ctx, r.fence(), r.route, Gate{Purpose: purpose, Text: text, Approve: approve, Reject: reject, TTL: ttl}); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
	}
	return r.waitFor(state, gatePrefix+purpose)
}

// gateDecision returns the decision of the step's newest gate with purpose
// and marks its signal applied: "" while open, gateNone when there is none.
const gateNone = "none"

func (r *stepRun) gateDecision(purpose string) (string, error) {
	g, found, err := r.newestGate(purpose)
	if err != nil {
		return "", err
	}
	if !found {
		return gateNone, nil
	}
	if g.Decision == "" {
		return "", nil
	}
	if !g.Consumed && g.SignalID > 0 {
		if err := r.s.Owner.ConsumeGate(r.ctx, r.fence(), g.SignalID); err != nil {
			return "", storageFail()
		}
	}
	return g.Decision, nil
}

type clarify struct {
	version     int64
	what        string
	planVersion int
	taskKey     string
}

func (c clarify) marker() string {
	return fmt.Sprintf("%s%d:%s:%d:%s", clarifyPrefix, c.version, c.what, c.planVersion, c.taskKey)
}

func parseClarify(m string) (clarify, bool) {
	parts := strings.SplitN(strings.TrimPrefix(m, clarifyPrefix), ":", 4)
	if len(parts) != 4 {
		return clarify{}, false
	}
	v, err1 := strconv.ParseInt(parts[0], 10, 64)
	pv, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return clarify{}, false
	}
	return clarify{version: v, what: parts[1], planVersion: pv, taskKey: parts[3]}, true
}

func (r *stepRun) instructions() ([]workflow.Instruction, error) {
	list, err := r.s.Instructions.ListInstructions(r.ctx, r.ws, r.id)
	if err != nil {
		return nil, storageFail()
	}
	slices.SortStableFunc(list, func(a, b workflow.Instruction) int {
		switch {
		case a.Version < b.Version:
			return -1
		case a.Version > b.Version:
			return 1
		}
		return 0
	})
	return list, nil
}

// askClarification sends a question bound to WF-n and waits for the
// owner's answer, which arrives as a newer versioned instruction (REQ-9).
func (r *stepRun) askClarification(what, text, taskKey string) (workflow.StepResult, error) {
	list, err := r.instructions()
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	var version int64
	if n := len(list); n > 0 {
		version = list[n-1].Version
	}
	c := clarify{version: version, what: what, planVersion: r.planVersion(), taskKey: taskKey}
	ref := workflow.FormatRef(r.id)
	msg := fmt.Sprintf("%s cần thêm thông tin: %s\nHãy trả lời bằng một tin nhắn bắt đầu bằng %s trong topic này.", ref, capText(text, maxQuestionRunes), ref)
	if err := r.s.Owner.Say(r.ctx, r.route, msg); err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	return r.waitFor(workflow.StateWaitingForUser, c.marker())
}

// resumeWait continues the wait the step's own checkpoint names. handled is
// false when the step should run its normal flow (no wait, a budget
// extension granted, a lost gate).
func (r *stepRun) resumeWait(onApproved func() (workflow.StepResult, error), onAnswer func(clarify, workflow.Instruction) (workflow.StepResult, error)) (workflow.StepResult, bool, error) {
	m := r.wait
	r.wait = ""
	switch {
	case strings.HasPrefix(m, gatePrefix):
		purpose := strings.TrimPrefix(m, gatePrefix)
		d, err := r.gateDecision(purpose)
		if err != nil {
			res, err := r.failStep(ReasonStorageUnavailable, true)
			return res, true, err
		}
		switch d {
		case "":
			state := workflow.StateWaitingForUser
			if purpose == approvalGate {
				state = workflow.StateWaitingForApproval
			}
			res, err := r.waitFor(state, m)
			return res, true, err
		case gateNone:
			return workflow.StepResult{}, false, nil
		}
		if strings.HasPrefix(purpose, budgetPurpose) {
			if d == GateApproved {
				if err := r.budget.Extend(BudgetKind(strings.TrimPrefix(purpose, budgetPurpose))); err == nil {
					log.Printf("orchestrator execution %d step %s: budget extended", r.id, r.key)
					return workflow.StepResult{}, false, nil
				}
			}
			res, err := r.failDecision(ReasonPlanningBudgetExceeded)
			return res, true, err
		}
		if d == GateApproved {
			if onApproved == nil {
				res, err := r.success()
				return res, true, err
			}
			res, err := onApproved()
			return res, true, err
		}
		reason := ReasonApprovalRejected
		if d == GateExpired {
			reason = ReasonApprovalExpired
		}
		res, err := r.failDecision(reason)
		return res, true, err
	case strings.HasPrefix(m, clarifyPrefix):
		c, ok := parseClarify(m)
		if !ok {
			return workflow.StepResult{}, false, nil
		}
		list, err := r.instructions()
		if err != nil {
			res, err := r.failStep(ReasonStorageUnavailable, true)
			return res, true, err
		}
		if n := len(list); n == 0 || list[n-1].Version <= c.version {
			res, err := r.waitFor(workflow.StateWaitingForUser, m)
			return res, true, err
		}
		res, err := onAnswer(c, list[len(list)-1])
		return res, true, err
	}
	return workflow.StepResult{}, false, nil
}

// answeredReplan turns a clarification answer of the plan into a new plan
// version that sees the answer (REPLAN, charged to the replan budget).
func (r *stepRun) answeredReplan(c clarify, _ workflow.Instruction) (workflow.StepResult, error) {
	if r.plan != nil && r.plan.Version > c.planVersion {
		return r.success()
	}
	if d := r.lastOwnDecision(); d == nil || d.Decision != DecisionReplan || d.ReasonCode != ReasonClarified || d.PlanVersion != c.planVersion {
		if _, err := r.record(DecisionReplan, ReasonClarified, "", "", nil); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
	}
	return r.replan(ReasonClarified, c.planVersion)
}

// --- plans ---

func planFromRecords(tasks []PlanTaskRecord) Plan {
	p := Plan{}
	for _, t := range tasks {
		p.Tasks = append(p.Tasks, PlanTask{
			Key: t.Key, Title: t.Title, Capabilities: slices.Clone(t.Capabilities), DependsOn: slices.Clone(t.DependsOn),
			Priority: t.Priority, Complexity: t.Complexity, Executor: t.Executor, Merge: cloneMerge(t.Merge),
		})
	}
	if order, ok := topoOrder(p.Tasks); ok {
		p.Order = order
	} else {
		p.Order = taskKeys(p)
	}
	return p
}

// request is the owner's request: every instruction of the execution in
// version order (the first is the request, later ones are answers).
func (r *stepRun) request() (string, error) {
	list, err := r.instructions()
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(list))
	for _, in := range list {
		if t := strings.TrimSpace(in.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "", &workflow.StepError{Code: ReasonInstructionPending, Retryable: true}
	}
	return strings.Join(parts, "\n\n"), nil
}

// planInput offers the capabilities of wired executors; clickup.write only
// when the execution works on a ClickUp task (a ClickUp write never names
// another task, Phase 10 REQ-10).
func (r *stepRun) planInput(request string) PlanInput {
	caps := r.s.offeredCapabilities()
	if r.route.ClickUpTaskID == "" {
		caps = slices.DeleteFunc(caps, func(c string) bool { return c == CapClickUpWrite })
	}
	return PlanInput{
		Request: request, Capabilities: caps, MaxTasks: r.s.Settings.MaxTasks,
		Caps: r.budget.Snapshot().Caps.PlanCaps(),
	}
}

func stepErr(err error) (string, bool) {
	var se *workflow.StepError
	if errors.As(err, &se) {
		return string(se.Code), se.Retryable
	}
	return ReasonStorageUnavailable, true
}

func (r *stepRun) planStep() (workflow.StepResult, error) {
	if res, handled, err := r.resumeWait(nil, r.answeredReplan); handled {
		return res, err
	}
	if _, ok := r.terminal(); ok {
		return r.success()
	}
	if r.plan != nil {
		if d := r.lastOwnDecision(); d != nil {
			return r.apply(*d, "")
		}
		// The plan was stored but its decision was not: run it.
		d, err := r.record(DecisionExecute, "", "", "", nil)
		if err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.apply(d, "")
	}
	request, err := r.request()
	if err != nil {
		code, retry := stepErr(err)
		return r.failStep(code, retry)
	}
	if err := r.budget.StartPlanIteration(false, r.s.Settings.PlanEstimate); err != nil {
		return r.budgetGate(err)
	}
	o, err := r.orch()
	if err != nil {
		return r.failDecision(ReasonLLMError)
	}
	p, u, err := o.Plan(r.ctx, r.planInput(request))
	r.recordUsage(u)
	if err != nil {
		return r.planError(err)
	}
	return r.storePlan(1, p, nil, u, 0)
}

// replan makes plan version fromVersion+1 (REQ-8): finished tasks are
// carried with their results, the replan is charged before the call.
func (r *stepRun) replan(reason string, fromVersion int) (workflow.StepResult, error) {
	if r.plan == nil || r.plan.Version > fromVersion {
		return r.success()
	}
	request, err := r.request()
	if err != nil {
		code, retry := stepErr(err)
		return r.failStep(code, retry)
	}
	if err := r.budget.StartPlanIteration(true, r.s.Settings.PlanEstimate); err != nil {
		return r.budgetGate(err)
	}
	o, err := r.orch()
	if err != nil {
		return r.failDecision(ReasonLLMError)
	}
	prev := planFromRecords(r.tasks)
	in := r.planInput(request)
	in.Previous, in.ReplanReason = &prev, reason
	next, u, err := o.Plan(r.ctx, in)
	r.recordUsage(u)
	if err != nil {
		return r.planError(err)
	}
	version, merged, carried := NextPlan(prev, r.plan.Version, r.tasks, next)
	return r.storePlan(version, merged, carried, u, r.plan.Version)
}

// storePlan assigns executors (REQ-5), stores the plan version with its
// tasks (carried tasks keep their results), supersedes the previous
// version, records the plan's initial decision and applies it.
func (r *stepRun) storePlan(version int, p Plan, carried []PlanTaskRecord, u Usage, supersede int) (workflow.StepResult, error) {
	assigned, blocked := r.s.Registry.AssignExecutors(p)
	byKey := map[string]PlanTaskRecord{}
	for _, c := range carried {
		byKey[c.Key] = c
	}
	records := make([]PlanTaskRecord, 0, len(assigned.Tasks))
	for _, t := range assigned.Tasks {
		if c, ok := byKey[t.Key]; ok {
			if c.Executor == "" {
				c.Executor = t.Executor
			}
			records = append(records, c)
			continue
		}
		state := TaskPending
		if t.Executor == "" {
			state = TaskBlocked
		}
		records = append(records, PlanTaskRecord{
			Key: t.Key, Title: t.Title, Capabilities: slices.Clone(t.Capabilities), DependsOn: slices.Clone(t.DependsOn),
			Priority: t.Priority, Complexity: t.Complexity, Executor: t.Executor, Merge: cloneMerge(t.Merge), State: state,
		})
	}
	budget, _ := json.Marshal(p.Budget)
	if _, err := r.s.Plans.InsertPlan(r.ctx, r.ws, PlanRecord{
		ExecutionID: r.id, Version: version, SchemaVersion: SchemaPlan.Version, Budget: budget,
		TokensIn: max(u.Tokens.Input, 0), TokensOut: max(u.Tokens.Output, 0), CostUSD: max(u.CostUSD, 0),
	}, records); err != nil {
		// The step retry calls Plan again: this plan is lost, but the
		// iteration was charged to the budget before the call, so the
		// re-call is bounded by the planning budget like any other.
		return r.failStep(ReasonStorageUnavailable, true)
	}
	if supersede > 0 {
		if err := r.s.Plans.SetPlanStatus(r.ctx, r.ws, r.id, supersede, PlanSuperseded); err != nil {
			log.Printf("orchestrator execution %d: plan v%d not marked superseded", r.id, supersede)
		}
	}
	if err := r.reload(); err != nil {
		code, retry := stepErr(err)
		return r.failStep(code, retry)
	}
	decision, reason, converted := p.Decision, "", ""
	if decision == "" || decision == DecisionReplan || decision == DecisionComplete {
		decision = DecisionExecute
	}
	if len(blocked) > 0 {
		decision, reason, converted = DecisionFail, ReasonNoExecutor, p.Decision
	}
	d, err := r.record(decision, reason, "", converted, nil)
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	r.sayPlan()
	return r.apply(d, "")
}

func (r *stepRun) sayPlan() {
	if r.plan == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: kế hoạch v%d (%d việc)", workflow.FormatRef(r.id), r.plan.Version, len(r.tasks))
	caps := r.budget.Snapshot().Caps
	fmt.Fprintf(&b, "\nGiới hạn: %d vòng lập kế hoạch · %d lần lập lại · %d token · %d phút",
		caps.PlanIterations, caps.Replans, caps.Tokens, int64(caps.Wall/time.Minute))
	order := planFromRecords(r.tasks).Order
	for i, t := range order {
		rec, _ := r.task(t)
		executor := rec.Executor
		if executor == "" {
			executor = "không có executor phù hợp"
		}
		line := fmt.Sprintf("\n%d. %s · %s [%s]", i+1, sanitizeText(rec.Key, 32), sanitizeText(rec.Title, MaxTitleRunes), executor)
		if len(rec.DependsOn) > 0 {
			deps := make([]string, 0, len(rec.DependsOn))
			for _, d := range rec.DependsOn {
				deps = append(deps, sanitizeText(d, 32))
			}
			line += " ← " + strings.Join(deps, ", ")
		}
		// Keep room for the "+N" line so the message stays under the limit.
		if rest := len(order) - i; utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > maxReportRunes-planMoreReserve && rest > 0 {
			fmt.Fprintf(&b, "\n… +%d việc nữa", rest)
			break
		}
		b.WriteString(line)
	}
	if err := r.s.Owner.Say(r.ctx, r.route, capText(b.String(), maxReportRunes)); err != nil {
		log.Printf("orchestrator execution %d: plan message not sent", r.id)
	}
}

// planMoreReserve is the room kept for the "+N more tasks" line.
const planMoreReserve = 40

// --- tasks ---

func (r *stepRun) task(key string) (PlanTaskRecord, bool) {
	for _, t := range r.tasks {
		if t.Key == key {
			return t, true
		}
	}
	return PlanTaskRecord{}, false
}

func (r *stepRun) putTask(t PlanTaskRecord) error {
	if err := r.s.Plans.UpdateTask(r.ctx, r.ws, t); err != nil {
		return storageFail()
	}
	for i := range r.tasks {
		if r.tasks[i].Key == t.Key {
			r.tasks[i] = t
		}
	}
	return nil
}

// currentTask is the task this run slot already started, else the first
// ready task in dependency order (every dependency succeeded).
func (r *stepRun) currentTask() (PlanTaskRecord, bool) {
	for _, t := range r.tasks {
		if t.ResultStepID == r.key && (t.State == TaskRunning || t.State == TaskWaiting) {
			return t, true
		}
	}
	for _, key := range planFromRecords(r.tasks).Order {
		t, _ := r.task(key)
		if t.State != TaskPending && t.State != TaskReady {
			continue
		}
		ready := true
		for _, dep := range t.DependsOn {
			if d, ok := r.task(dep); !ok || d.State != TaskSucceeded {
				ready = false
			}
		}
		if ready {
			return t, true
		}
	}
	return PlanTaskRecord{}, false
}

func (r *stepRun) execRef(kind, id string) string { return fmt.Sprintf("wf%d:%s:%s", r.id, kind, id) }

// finishTask stores a task result: fixed outcome and reason labels, ids
// and links only.
func (r *stepRun) finishTask(t PlanTaskRecord, ok bool, reason string, evidence []string, extra map[string]string) error {
	outcome, state := "failed", TaskFailed
	if ok {
		outcome, state = "succeeded", TaskSucceeded
	}
	return r.endTask(t, state, outcome, reason, evidence, extra)
}

// blockTask stores a BLOCKED result: the task did not fail, but cannot go
// on (the owner cancelled it, or it expired); Evaluate decides.
func (r *stepRun) blockTask(t PlanTaskRecord, reason string, evidence []string, extra map[string]string) error {
	return r.endTask(t, TaskBlocked, "blocked", reason, evidence, extra)
}

func (r *stepRun) endTask(t PlanTaskRecord, state, outcome, reason string, evidence []string, extra map[string]string) error {
	t.State = state
	if !reasonCodeRe.MatchString(reason) {
		reason = ""
	}
	result := map[string]string{"outcome": outcome}
	if reason != "" {
		result["reason_code"] = reason
	}
	for k, v := range extra {
		if v = strings.TrimSpace(v); v != "" && len(v) <= maxResultTextBytes && utf8.ValidString(v) {
			result[k] = v
		}
	}
	t.Result, _ = json.Marshal(result)
	t.EvidenceRefs = boundRefs(evidence)
	t.ResultStepID = r.key
	return r.putTask(t)
}

func (r *stepRun) runStep() (workflow.StepResult, error) {
	if res, handled, err := r.resumeWait(nil, r.answeredRepo); handled {
		return res, err
	}
	if _, ok := r.terminal(); ok || r.plan == nil {
		r.taskRef = noTask
		return r.success()
	}
	t, ok := r.currentTask()
	if !ok {
		r.taskRef = noTask
		return r.success()
	}
	r.taskRef = t.Key
	return r.runTask(t, "")
}

func (r *stepRun) runTask(t PlanTaskRecord, explicitRepo string) (workflow.StepResult, error) {
	prof, ok := r.s.Registry.Profile(t.Executor)
	if !ok {
		if err := r.finishTask(t, false, ReasonNoExecutor, nil, nil); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	}
	if prof.Worker {
		return r.runWorker(t, explicitRepo)
	}
	ex := r.s.Executors[t.Executor]
	resumer, _ := ex.(TaskResumer)
	if t.ResultStepID == r.key && (t.State == TaskRunning || t.State == TaskWaiting) {
		if resumer == nil {
			// A retry of this step found its in-process task still running:
			// the earlier attempt was interrupted after it may have spent
			// tokens and made tool calls. Never run it a second time; fail
			// it and let Evaluate decide.
			if err := r.finishTask(t, false, ReasonExecutorInterrupted, nil, nil); err != nil {
				return r.failStep(ReasonStorageUnavailable, true)
			}
			return r.success()
		}
		// The task waits (or an attempt stopped before it recorded the
		// wait): the executor reads its state again and never repeats a
		// side effect (Phase 10 REQ-10).
		ref := ""
		if t.State == TaskWaiting && strings.HasPrefix(r.prevWait, taskWaitPrefix) {
			ref = strings.TrimPrefix(r.prevWait, taskWaitPrefix)
		}
		out, err := resumer.ResumeTask(r.ctx, r.taskRun(t, ""), ref)
		if err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		r.recordUsage(out.Usage)
		return r.applyOutcome(t, out, true)
	}
	if ex == nil {
		if err := r.finishTask(t, false, ReasonExecutorUnavailable, nil, nil); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	}
	if err := r.budget.CanCall(r.s.Settings.ExecuteEstimate); err != nil {
		return r.budgetGate(err)
	}
	request, err := r.request()
	if err != nil {
		code, retry := stepErr(err)
		return r.failStep(code, retry)
	}
	t.State, t.ResultStepID = TaskRunning, r.key
	if err := r.putTask(t); err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	out, err := ex.RunTask(r.ctx, r.taskRun(t, request))
	r.recordUsage(out.Usage)
	if err != nil {
		if r.ctx.Err() != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		out = TaskOutcome{ReasonCode: ReasonExecutorFailed}
	}
	return r.applyOutcome(t, out, resumer != nil)
}

// taskRun is what an in-process executor gets for task t.
func (r *stepRun) taskRun(t PlanTaskRecord, request string) TaskRun {
	snap := r.budget.Snapshot()
	return TaskRun{
		WorkspaceID: r.ws, ExecutionID: r.id, StepKey: r.key, Task: planFromRecords([]PlanTaskRecord{t}).Tasks[0], Route: r.route, Fence: r.fence(),
		Request: request, TokensLeft: max(snap.Caps.Tokens-snap.Used.Tokens, 0), Inputs: r.taskInputs(t),
		Proofs: r.taskProofs(t),
	}
}

// taskProofs are the evidence refs and stored result fields of the
// execution's succeeded tasks other than t, one entry per task.
func (r *stepRun) taskProofs(t PlanTaskRecord) []string {
	var out []string
	for _, d := range r.tasks {
		if d.Key == t.Key || d.State != TaskSucceeded {
			continue
		}
		parts := slices.Clone(d.EvidenceRefs)
		var res map[string]string
		_ = json.Unmarshal(d.Result, &res)
		for _, k := range []string{"job_id", "branch", "pr_url", "summary"} {
			if v := strings.TrimSpace(res[k]); v != "" {
				parts = append(parts, v)
			}
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "\n"))
		}
	}
	return out
}

// taskInputs are the bounded results of t's succeeded dependencies: the
// stored summary, PR URL and branch (ids, links and short texts only).
func (r *stepRun) taskInputs(t PlanTaskRecord) []string {
	var out []string
	for _, key := range t.DependsOn {
		d, ok := r.task(key)
		if !ok || d.State != TaskSucceeded || len(out) == maxTaskInputs {
			continue
		}
		var res map[string]string
		_ = json.Unmarshal(d.Result, &res)
		var parts []string
		if v := strings.TrimSpace(res["summary"]); v != "" {
			parts = append(parts, v)
		}
		if v := strings.TrimSpace(res["pr_url"]); strings.HasPrefix(v, "https://") {
			parts = append(parts, "PR: "+v)
		}
		if v := strings.TrimSpace(res["branch"]); v != "" {
			parts = append(parts, "Nhánh: "+v)
		}
		if len(parts) > 0 {
			out = append(out, capText(d.Title, 80)+": "+strings.Join(parts, " · "))
		}
	}
	return out
}

func validWaitRef(ref string) bool {
	return ref != "" && len(ref) <= maxWaitRefBytes && utf8.ValidString(ref) && !strings.ContainsAny(ref, " \t\r\n")
}

// applyOutcome stores an in-process executor's outcome: a wait (only for a
// TaskResumer) keeps the task WAITING and the step waits for the owner;
// otherwise the task succeeds, fails or is blocked with its evidence.
func (r *stepRun) applyOutcome(t PlanTaskRecord, out TaskOutcome, canWait bool) (workflow.StepResult, error) {
	if out.Wait != "" {
		if !canWait || !validWaitRef(out.Wait) {
			out = TaskOutcome{ReasonCode: ReasonExecutorFailed}
		} else {
			if t.State != TaskWaiting || t.ResultStepID != r.key {
				t.State, t.ResultStepID = TaskWaiting, r.key
				if err := r.putTask(t); err != nil {
					return r.failStep(ReasonStorageUnavailable, true)
				}
			}
			return r.waitFor(workflow.StateWaitingForApproval, taskWaitPrefix+out.Wait)
		}
	}
	evidence := []string{r.execRef("step", r.key)}
	for _, e := range out.Evidence {
		evidence = append(evidence, e.ID)
	}
	var extra map[string]string
	if out.Summary != "" {
		extra = map[string]string{"summary": out.Summary}
	}
	var err error
	if out.Blocked && !out.Succeeded {
		err = r.blockTask(t, out.ReasonCode, evidence, extra)
	} else {
		err = r.finishTask(t, out.Succeeded, out.ReasonCode, evidence, extra)
	}
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	return r.success()
}

// resolveRepo applies REQ-14: explicit repo, remembered repo of the task,
// the only registered repo, else clarification.
func (r *stepRun) resolveRepo(explicit string) (string, []string, bool, error) {
	if r.s.Repos == nil {
		return "", nil, true, nil
	}
	registered, err := r.s.Repos.RegisteredRepos(r.ctx, r.ws)
	if err != nil {
		return "", nil, false, storageFail()
	}
	if explicit == "" {
		explicit = r.route.Repo
	}
	remembered := func() (string, bool) {
		if r.route.ClickUpTaskID == "" {
			return "", false
		}
		last, err := r.s.Repos.LastTaskRepo(r.ctx, r.ws, r.route.ClickUpTaskID)
		return last, err == nil && last != ""
	}
	repo, need := ResolveRepo(explicit, remembered, registered)
	return repo, registered, need, nil
}

// workerJobPayload is the payload the laptop worker parses (the same keys
// as a 7a local task).
type workerJobPayload struct {
	Repo               string `json:"repo"`
	TaskID             string `json:"task_id"`
	ClickUpTaskID      string `json:"clickup_task_id"`
	Title              string `json:"title"`
	Instruction        string `json:"instruction"`
	InstructionVersion int64  `json:"instruction_version"`
}

// runWorker delegates a repo.write task to a 7a worker job (REQ-14; there
// is no VPS fallback) and follows it (Phase 10 REQ-11/12):
//   - waiting, leased, running or cancelling: the step waits in
//     WAITING_FOR_WORKER;
//   - failed, needs input or interrupted: the topic shows the job's
//     Retry/Cancel buttons and the step waits for the owner in
//     WAITING_FOR_USER (a retried job is followed again);
//   - succeeded: the task succeeds; cancelled: the task fails
//     (worker_cancelled) and Evaluate decides.
//
// Both waits carry a durable wake time at the end of the wall-time budget;
// the step then takes the O4 outcome (budget gate). The wait counts against
// the wall time, not tokens.
func (r *stepRun) runWorker(t PlanTaskRecord, explicitRepo string) (workflow.StepResult, error) {
	if r.s.Jobs == nil {
		if err := r.finishTask(t, false, ReasonExecutorUnavailable, nil, nil); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	}
	job, ok, err := r.s.Jobs.JobForStep(r.ctx, r.ws, r.id, r.key)
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	if !ok {
		if err := r.budget.CheckWall(); err != nil {
			return r.budgetGate(err)
		}
		repo, registered, need, err := r.resolveRepo(explicitRepo)
		if err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		if need {
			t.State, t.ResultStepID = TaskWaiting, r.key
			if err := r.putTask(t); err != nil {
				return r.failStep(ReasonStorageUnavailable, true)
			}
			if _, err := r.record(DecisionRequestClarification, ReasonRepoUnresolved, t.Key, "", nil); err != nil {
				return r.failStep(ReasonStorageUnavailable, true)
			}
			q := "chưa biết việc \"" + capText(t.Title, 80) + "\" sửa repo nào."
			if len(registered) > 0 {
				q += " Các repo đã đăng ký: " + strings.Join(registered, ", ") + "."
			}
			return r.askClarification("repo", q+" Hãy trả lời tên repo.", t.Key)
		}
		if job, err = r.createJob(t, repo); err != nil {
			code, retry := stepErr(err)
			return r.failStep(code, retry)
		}
		t.State, t.ResultStepID = TaskRunning, r.key
		if err := r.putTask(t); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
	}
	r.jobRef = job.ID
	evidence := []string{r.execRef("job", job.ID)}
	extra := map[string]string{"job_id": job.ID, "branch": job.Branch, "pr_url": job.PRURL, "test_outcome": job.TestOutcome,
		"summary": boundResultText(job.Summary)}
	switch job.State {
	case JobSucceeded:
		if strings.HasPrefix(job.PRURL, "https://") {
			evidence = append(evidence, job.PRURL)
		}
		if err := r.finishTask(t, true, "", evidence, extra); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	case JobCancelled:
		if err := r.finishTask(t, false, ReasonWorkerCancelled, evidence, extra); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		return r.success()
	}
	if err := r.budget.CheckWall(); err != nil {
		return r.budgetGate(err)
	}
	if t.State != TaskWaiting || t.ResultStepID != r.key {
		t.State, t.ResultStepID = TaskWaiting, r.key
		if err := r.putTask(t); err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
	}
	switch job.State {
	case JobFailed, JobNeedsInput, JobInterrupted:
		return r.waitWorker(workflow.StateWaitingForUser, jobOwnerPrefix+job.ID)
	}
	return r.waitWorker(workflow.StateWaitingForWorker, jobPrefix+job.ID)
}

func (r *stepRun) createJob(t PlanTaskRecord, repo string) (WorkerJob, error) {
	list, err := r.instructions()
	if err != nil {
		return WorkerJob{}, err
	}
	parts := []string{}
	var version int64
	for _, in := range list {
		if text := strings.TrimSpace(in.Text); text != "" {
			parts = append(parts, text)
			version = max(version, in.Version)
		}
	}
	parts = append(parts, fmt.Sprintf("Việc %s trong kế hoạch %s: %s", t.Key, workflow.FormatRef(r.id), t.Title))
	text := strings.Join(parts, "\n\n")
	if len(text) > maxJobInstruction {
		text = strings.ToValidUTF8(text[len(text)-maxJobInstruction:], "")
	}
	display := r.route.TaskRef
	if display == "" {
		display = r.route.ClickUpTaskID
	}
	if display == "" {
		display = workflow.FormatRef(r.id)
	}
	payload, err := json.Marshal(workerJobPayload{
		Repo: repo, TaskID: display, ClickUpTaskID: r.route.ClickUpTaskID, Title: t.Title,
		Instruction: text, InstructionVersion: version,
	})
	if err != nil {
		return WorkerJob{}, &workflow.StepError{Code: ReasonStorageUnavailable}
	}
	if p, ok := r.s.Jobs.(WorkerJobRoutes); ok {
		if err := p.PrepareJob(r.ctx, r.route, r.key, repo, version); err != nil {
			log.Printf("orchestrator execution %d step %s: worker job topic route not stored", r.id, r.key)
		}
	}
	job, err := r.s.Jobs.CreateJob(r.ctx, r.ws, r.id, r.key, []string{CapRepoWrite}, payload)
	if err != nil {
		return WorkerJob{}, storageFail()
	}
	log.Printf("orchestrator execution %d step %s: worker job created", r.id, r.key)
	return job, nil
}

// answeredRepo retries the repo resolution with the owner's answer as the
// explicit repo.
func (r *stepRun) answeredRepo(c clarify, in workflow.Instruction) (workflow.StepResult, error) {
	t, ok := r.task(c.taskKey)
	if !ok || c.planVersion != r.planVersion() {
		return r.runStep()
	}
	r.taskRef = t.Key
	explicit := ""
	if f := strings.Fields(in.Text); len(f) > 0 {
		explicit = f[0]
		if workflow.FindRefs(explicit) != nil && len(f) > 1 {
			explicit = f[1]
		}
	}
	return r.runWorker(t, strings.Trim(explicit, ".,;:\"'"))
}

// --- evaluation ---

// ranTask is the task the preceding run slot ran ("-" or "" for none).
func (r *stepRun) ranTask() (string, error) {
	runKey := "run_" + strings.TrimPrefix(r.key, "eval_")
	steps, err := r.s.Steps.ListSteps(r.ctx, r.ws, r.id)
	if err != nil {
		return "", storageFail()
	}
	for _, st := range steps {
		if st.Key == runKey {
			return refValue(st.Checkpoint, CheckpointTask), nil
		}
	}
	return "", nil
}

func taskResultOf(t PlanTaskRecord) TaskResult {
	var res struct {
		Outcome    string `json:"outcome"`
		ReasonCode string `json:"reason_code"`
	}
	_ = json.Unmarshal(t.Result, &res)
	out := TaskResult{Outcome: res.Outcome, ReasonCode: res.ReasonCode}
	for _, ref := range t.EvidenceRefs {
		kind := EvidenceStepOutput
		if strings.HasPrefix(ref, "https://") {
			kind = EvidenceArtifact
		}
		out.Evidence = append(out.Evidence, EvidenceRef{Kind: kind, ID: ref})
	}
	return out
}

// evidenceMissing applies REQ-7: every task succeeded with at least one
// evidence reference of this execution, and a worker task carries the
// worker result (branch, PR URL or test outcome).
func (r *stepRun) evidenceMissing() bool {
	if len(r.tasks) == 0 {
		return true
	}
	own := fmt.Sprintf("wf%d:", r.id)
	stepRef := r.execRef("step", "")
	for _, t := range r.tasks {
		if t.State != TaskSucceeded {
			return true
		}
		mine, executorRef := false, false
		for _, ref := range t.EvidenceRefs {
			mine = mine || strings.HasPrefix(ref, own)
			executorRef = executorRef || !strings.HasPrefix(ref, stepRef)
		}
		if !mine {
			return true
		}
		p, ok := r.s.Registry.Profile(t.Executor)
		if !(ok && p.Worker) && !executorRef {
			// An in-process task needs a ref from the executor itself (a
			// tool read); its own step ref alone proves nothing.
			return true
		}
		if ok && p.Worker {
			var res map[string]string
			_ = json.Unmarshal(t.Result, &res)
			if res["job_id"] == "" || (res["branch"] == "" && res["pr_url"] == "" && res["test_outcome"] == "") {
				return true
			}
		}
	}
	return false
}

// replanOrFail is the conversion target of an unproven COMPLETE: REPLAN
// while the replan budget allows another plan call, else FAIL.
func (r *stepRun) replanOrFail() string {
	s := r.budget.Snapshot()
	if s.Used.Replans < s.Caps.Replans && s.Used.PlanIterations < s.Caps.PlanIterations && r.budget.CanCall(r.s.Settings.PlanEstimate) == nil {
		return DecisionReplan
	}
	return DecisionFail
}

func (r *stepRun) evalStep() (workflow.StepResult, error) {
	if res, handled, err := r.resumeWait(nil, r.answeredReplan); handled {
		return res, err
	}
	if _, ok := r.terminal(); ok || r.plan == nil {
		return r.success()
	}
	if d := r.lastOwnDecision(); d != nil {
		return r.apply(*d, "")
	}
	key, err := r.ranTask()
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	t, ok := r.task(key)
	if !ok {
		return r.evaluateNoTask()
	}
	if err := r.budget.CanCall(r.s.Settings.EvaluateEstimate); err != nil {
		return r.budgetGate(err)
	}
	o, err := r.orch()
	if err != nil {
		return r.failDecision(ReasonLLMError)
	}
	d, u, err := o.Evaluate(r.ctx, EvaluateInput{Plan: planFromRecords(r.tasks), TaskKey: t.Key, Result: taskResultOf(t)})
	r.recordUsage(u)
	if err != nil {
		return r.planError(err)
	}
	decision, reason, converted := d.Decision, d.ReasonCode, ""
	if decision == DecisionComplete && r.evidenceMissing() {
		decision, reason, converted = r.replanOrFail(), ReasonMissingEvidence, DecisionComplete
	}
	taskKey := d.TaskKey
	if taskKey == "" {
		taskKey = t.Key
	}
	rec, err := r.record(decision, reason, taskKey, converted, d.EvidenceRefs)
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	return r.apply(rec, d.NextAction)
}

// evaluateNoTask decides without a model call when no task ran: COMPLETE
// when every task succeeded (with the evidence check), else FAIL.
func (r *stepRun) evaluateNoTask() (workflow.StepResult, error) {
	all := len(r.tasks) > 0
	for _, t := range r.tasks {
		all = all && t.State == TaskSucceeded
	}
	decision, reason, converted := DecisionFail, ReasonNoRunnableTask, ""
	if all {
		decision, reason = DecisionComplete, ReasonAllTasksSucceeded
		if r.evidenceMissing() {
			decision, reason, converted = r.replanOrFail(), ReasonMissingEvidence, DecisionComplete
		}
	}
	rec, err := r.record(decision, reason, "", converted, nil)
	if err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	return r.apply(rec, "")
}

// --- report ---

// fixedFailReasons are the FAIL reasons an execution may end with; any
// other (a model's own reason code) ends as orchestrator_failed.
var fixedFailReasons = []string{
	ReasonPlanningBudgetExceeded, ReasonInvalidOutput, ReasonMissingEvidence, ReasonNoExecutor,
	ReasonNoRunnableTask, ReasonTaskSlotsExhausted, ReasonApprovalRejected, ReasonApprovalExpired,
	ReasonInvalidInputCode, ReasonLLMError, ReasonLLMBudgetExceeded,
}

// isBudgetExceeded reports a provider refusal by llm_daily_budget_usd.
func isBudgetExceeded(err error) bool {
	le, ok := domain.AsLLMError(err)
	return ok && le.Code == domain.LLMCodeBudgetExceeded
}

func (r *stepRun) reportStep() (workflow.StepResult, error) {
	term, ok := r.terminal()
	if !ok {
		rec, err := r.record(DecisionFail, ReasonTaskSlotsExhausted, "", "", nil)
		if err != nil {
			return r.failStep(ReasonStorageUnavailable, true)
		}
		term = rec
	}
	status := PlanCompleted
	if term.Decision == DecisionFail {
		status = PlanFailed
	}
	if err := r.setPlanStatus(status); err != nil {
		return r.failStep(ReasonStorageUnavailable, true)
	}
	say := r.s.Owner.Say
	if fs, ok := r.s.Owner.(FinalSayer); ok {
		say = fs.SayFinal
	}
	if err := say(r.ctx, r.route, r.reportText(term)); err != nil {
		log.Printf("orchestrator execution %d: report not sent", r.id)
	}
	if term.Decision != DecisionFail {
		return r.success()
	}
	code := ReasonOrchestratorFailed
	if slices.Contains(fixedFailReasons, term.ReasonCode) {
		code = term.ReasonCode
	}
	res, _ := r.success()
	return res, &workflow.StepError{Code: workflow.ReasonCode(code)}
}

var taskStateText = map[string]string{
	TaskPending: "chưa chạy", TaskReady: "sẵn sàng", TaskRunning: "đang chạy", TaskWaiting: "đang chờ",
	TaskSucceeded: "xong", TaskFailed: "thất bại", TaskBlocked: "bị chặn", TaskSkipped: "bỏ qua",
}

// reportText is the final message: outcome, plan summary (titles only),
// evidence links, decisions and budget used. No other model text.
func (r *stepRun) reportText(term DecisionRecord) string {
	var b strings.Builder
	ref := workflow.FormatRef(r.id)
	if term.Decision == DecisionFail {
		reason := term.ReasonCode
		if reason == "" {
			reason = ReasonOrchestratorFailed
		}
		fmt.Fprintf(&b, "%s: thất bại (%s).", ref, reason)
	} else {
		fmt.Fprintf(&b, "%s: hoàn tất.", ref)
	}
	if r.plan != nil {
		fmt.Fprintf(&b, "\nKế hoạch v%d · %d việc:", r.plan.Version, len(r.tasks))
		for i, key := range planFromRecords(r.tasks).Order {
			t, _ := r.task(key)
			state := taskStateText[t.State]
			if state == "" {
				state = t.State
			}
			fmt.Fprintf(&b, "\n%d. %s — %s", i+1, capText(t.Title, MaxTitleRunes), state)
			var res map[string]string
			if json.Unmarshal(t.Result, &res) == nil {
				if pr := res["pr_url"]; strings.HasPrefix(pr, "https://") {
					b.WriteString("\n   PR: " + pr)
				} else if br := res["branch"]; br != "" {
					b.WriteString("\n   Nhánh: " + br)
				}
				if to := res["test_outcome"]; to != "" {
					b.WriteString(" · test: " + to)
				}
			}
		}
	}
	counts := map[string]int{}
	var order []string
	for _, d := range r.decisions {
		if counts[d.Decision] == 0 {
			order = append(order, d.Decision)
		}
		counts[d.Decision]++
	}
	if len(order) > 0 {
		parts := make([]string, 0, len(order))
		for _, d := range order {
			parts = append(parts, fmt.Sprintf("%s ×%d", d, counts[d]))
		}
		b.WriteString("\nQuyết định: " + strings.Join(parts, ", "))
	}
	s := r.budget.Snapshot()
	fmt.Fprintf(&b, "\nNgân sách: %d/%d token · %d/%d vòng lập kế hoạch · %d/%d lần lập lại · %d/%d phút",
		s.Used.Tokens, s.Caps.Tokens, s.Used.PlanIterations, s.Caps.PlanIterations, s.Used.Replans, s.Caps.Replans,
		int(r.budget.Elapsed()/time.Minute), int(s.Caps.Wall/time.Minute))
	return capText(b.String(), maxReportRunes)
}

// boundResultText cleans a worker's text for a stored task result and cuts
// it on a rune boundary to the size endTask keeps.
func boundResultText(s string) string {
	s = sanitizeText(s, maxResultTextBytes)
	for len(s) > maxResultTextBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}

func capText(s string, n int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}
