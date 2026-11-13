package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/descblock"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is the action library: it delivers a run's classified items
// (drafts, asks, digest, FYI), runs the behavior's allowed actions through
// the Phase 2 executor (policy, audit, idempotency per run, item and
// action), checks the target for conflicts before every ClickUp write and
// asks for a confirmation before status and assignee changes. Nothing here
// knows a particular behavior: what to write comes from the behavior's
// actions and the item fields.

// ClickUp write tools of the action library.
const (
	ToolSetDescriptionBlock = "clickup_set_description_block"
	ToolPostComment         = "clickup_post_comment"
	ToolSetStatus           = "clickup_set_status"
	ToolRemoveAssignee      = "clickup_remove_assignee"
)

// Run-level write tools (actions.run). Both are ActionOnly tools: the
// executor runs them only when they are enabled and in UnscopedWrites.
// ToolPRApprove takes {"account","repo","number"} and needs the account in
// the policy's Accounts; ToolChatReaction takes
// {"team_id","message_id","reaction"} and the ClickUp token reference.
const (
	ToolPRApprove    = "github_pr_approve"
	ToolChatReaction = "clickup_chat_reaction"
	// ToolChatReactionRemove undoes ToolChatReaction (same input); a
	// reaction already gone counts as removed.
	ToolChatReactionRemove = "clickup_chat_reaction_remove"
)

// Action outcome statuses stored in an item's action result.
const (
	OutcomeOK              = "ok"
	OutcomeDelivered       = "delivered" // telegram.message: carried by the class message
	OutcomeConflict        = "conflict"
	OutcomeError           = "error"
	OutcomeUnknown         = "unknown_outcome"
	OutcomePolicyDryRun    = "policy_dry_run"
	OutcomeAwaitingConfirm = "awaiting_confirm"
	OutcomeAlready         = "already"
	OutcomeChanged         = "changed"
	OutcomeCanceled        = "canceled"
	OutcomeExpired         = "expired"
)

// Engine codes of action outcomes.
const (
	CodeChangedSinceRead = "changed_since_read"
	CodeMarkersInvalid   = "block_markers_invalid"
	CodeNoTask           = "no_task"
	CodeOwnerUnset       = "owner_clickup_user_unset"
	CodeTaskUnreadable   = "task_unreadable"
)

// TaskState is an exact read of a ClickUp task for conflict checks.
type TaskState struct {
	ID          string
	Status      string
	AssigneeIDs []string
	Description string // ClickUp's plain-text description
	Markdown    string // the markdown description blocks are written into
}

// TaskReader reads a task exactly with the workspace's ClickUp credential.
// Errors are *tools.ToolError with safe codes (Retryable when transient).
type TaskReader interface {
	ReadTask(ctx context.Context, workspaceID, clickUpTokenRef, taskID string) (TaskState, error)
}

// ChatResolver returns where the owner is reached for a run: the chat and
// bot a Telegram command came from, or the workspace's Telegram control
// integration and the owner's chat.
type ChatResolver interface {
	OwnerChat(ctx context.Context, workspaceID string, t TriggerData) (ChatRef, error)
}

// MessageLog records the engine's Telegram messages of a run under stable
// keys so a retried dispatch never sends one twice.
type MessageLog interface {
	// ClaimMessage claims key once. A repeat returns claimed=false and the
	// stored message ID (0 when that send never finished: unknown outcome,
	// not resent).
	ClaimMessage(ctx context.Context, workspaceID string, runID int64, key string) (claimed bool, messageID int64, err error)
	SetMessageID(ctx context.Context, workspaceID string, runID int64, key string, messageID int64) error
	// ReleaseMessage drops an unsent claim after a failed send.
	ReleaseMessage(ctx context.Context, workspaceID string, runID int64, key string) error
}

// DecisionRecorder remembers the owner's decisions (REQ-23): an answer to
// an ask (SourceAskAnswer), a posted auto draft (SourceAutoPosted) and an
// undone auto write (SourceAutoUndone). Storage and search are the memory
// feature's; the engine only reports. Errors are logged, never fatal.
type DecisionRecorder interface {
	RecordDecision(ctx context.Context, workspaceID string, d NewDecision) error
}

// NopDecisionRecorder remembers nothing.
type NopDecisionRecorder struct{}

func (NopDecisionRecorder) RecordDecision(context.Context, string, NewDecision) error { return nil }

// ActionCallerFactory builds the tool caller of one action call. keyScope
// namespaces the executor's idempotency keys per (item or undo, action);
// policy enables exactly the one write tool.
type ActionCallerFactory func(run Run, keyScope string, policy tenant.ToolPolicyConfig, clickUpTokenRef string) ToolCaller

// ActionExecutorFactory returns an ActionCallerFactory over the Phase 2
// executor: the run's workspace, audit (store, rows owned by the run) and
// write budget; the enabled ClickUp write tool is allowed without a repo
// grant (UnscopedWrites), nothing dangerous.
func ActionExecutorFactory(reg *tools.Registry, store tools.ToolRunStore, san tools.Sanitizer, base tools.Limits) ActionCallerFactory {
	return func(run Run, scope string, policy tenant.ToolPolicyConfig, ref string) ToolCaller {
		limits := base
		limits.MaxCalls = 4
		limits.MaxDangerous = 0
		e := tools.NewExecutor(reg, policy, limits, false, store, san, run.ID, run.WorkspaceID)
		e.ClickUpTokenRef = ref
		e.KeyScope = scope
		e.UnscopedWrites = append([]string(nil), policy.Enabled...)
		return e
	}
}

// ActionStore is what the action library needs from storage.
type ActionStore interface {
	GetRun(ctx context.Context, workspaceID string, runID int64) (Run, error)
	SetSnapshot(ctx context.Context, workspaceID string, runID int64, snapshot json.RawMessage) (bool, error)
	MarkUndone(ctx context.Context, workspaceID string, runID int64) error
	GetItem(ctx context.Context, workspaceID string, itemID int64) (Item, error)
	ListItems(ctx context.Context, workspaceID string, runID int64) ([]Item, error)
	TransitionItem(ctx context.Context, workspaceID string, itemID int64, from []ItemStatus, to ItemStatus, result json.RawMessage) (Item, error)
	RateItem(ctx context.Context, workspaceID string, itemID int64, rating Rating, reason string) error
	ApprovalStore
	MessageLog
}

// ActionDeps are the action library's collaborators.
type ActionDeps struct {
	Store     ActionStore
	Defs      DefinitionStore
	Profiles  ProfileStore
	Policies  PolicySource
	Tasks     TaskReader
	Chats     ChatResolver
	Messenger Messenger
	Callers   ActionCallerFactory
	Decisions DecisionRecorder // nil: NopDecisionRecorder
	// Digests queues digest and FYI messages for quiet hours and digest
	// mode; nil sends them at once.
	Digests  DigestStore
	Redactor *agent.Redactor
	// Reports is the single preference-aware renderer for every owner report.
	// Nil uses owner defaults and platform/id contact fallbacks.
	Reports *reporting.Renderer
}

// ActionSettings tune the action library.
type ActionSettings struct {
	// OwnerClickUpUserID is what "me" means in clickup.unassign.
	OwnerClickUpUserID string
	SweepInterval      time.Duration // expiry sweep period; default 1m
	RequestTimeout     time.Duration // one Telegram call; default 10s
	FeedbackExpiry     time.Duration // how long a 👎 reason is awaited; default 24h
}

// Engine is the action library. It implements Dispatcher, Notifier,
// CallbackHandler and Undoer, and its Run sweeps expired approvals.
type Engine struct {
	d   ActionDeps
	s   ActionSettings
	now func() time.Time
}

var (
	_ Dispatcher      = (*Engine)(nil)
	_ Notifier        = (*Engine)(nil)
	_ CallbackHandler = (*Engine)(nil)
	_ Undoer          = (*Engine)(nil)
)

func NewEngine(d ActionDeps, s ActionSettings) (*Engine, error) {
	if d.Store == nil || d.Defs == nil || d.Profiles == nil || d.Policies == nil || d.Tasks == nil ||
		d.Chats == nil || d.Messenger == nil || d.Callers == nil {
		return nil, errors.New("action engine: missing dependency")
	}
	if d.Decisions == nil {
		d.Decisions = NopDecisionRecorder{}
	}
	if d.Redactor == nil {
		d.Redactor = agent.NewRedactor(nil)
	}
	if d.Reports == nil {
		d.Reports = reporting.NewRenderer(nil, nil)
	}
	if s.SweepInterval <= 0 {
		s.SweepInterval = time.Minute
	}
	if s.RequestTimeout <= 0 {
		s.RequestTimeout = 10 * time.Second
	}
	if s.FeedbackExpiry <= 0 {
		s.FeedbackExpiry = 24 * time.Hour
	}
	return &Engine{d: d, s: s, now: time.Now}, nil
}

// --- run snapshot and item results ---

// runSnapshot is the state of the run's task the engine saw before its
// first write (behavior_runs.snapshot): the base of conflict checks and
// undo. Drift is set when the task's description outside managed blocks
// already differed from what the model read.
type runSnapshot struct {
	TaskID            string   `json:"task_id"`
	DescriptionBefore string   `json:"description_before"`
	OutsideSHA256     string   `json:"outside_sha256,omitempty"` // "" when the markers were invalid
	Status            string   `json:"status"`
	AssigneeIDs       []string `json:"assignee_ids,omitempty"`
	Drift             bool     `json:"drift,omitempty"`
	CapturedAt        int64    `json:"captured_at"`
}

func decodeSnapshot(raw json.RawMessage) *runSnapshot {
	if len(raw) == 0 {
		return nil
	}
	var s runSnapshot
	if json.Unmarshal(raw, &s) != nil || s.CapturedAt == 0 {
		return nil
	}
	return &s
}

// actionOutcome is the result of one action of an item.
type actionOutcome struct {
	Index      int    `json:"i"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	Block      string `json:"block,omitempty"`
	DescSHA    string `json:"desc_sha256,omitempty"`
	OutsideSHA string `json:"outside_sha256,omitempty"`
	At         int64  `json:"at,omitempty"` // unix nanoseconds of a description write
}

// itemResult is an item's action_result.
type itemResult struct {
	Actions []actionOutcome `json:"actions"`
}

func decodeResult(raw json.RawMessage) itemResult {
	var r itemResult
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r)
	}
	return r
}

func (r *itemResult) set(o actionOutcome) {
	for i := range r.Actions {
		if r.Actions[i].Index == o.Index {
			r.Actions[i] = o
			return
		}
	}
	r.Actions = append(r.Actions, o)
	slices.SortFunc(r.Actions, func(a, b actionOutcome) int { return a.Index - b.Index })
}

func (r itemResult) encode() json.RawMessage {
	if r.Actions == nil {
		r.Actions = []actionOutcome{}
	}
	b, _ := json.Marshal(r)
	return b
}

// --- loading ---

// loadRun rebuilds a run's context for a later decision (a button, a reply
// or /undo): the pinned version, the profile language, trigger and context.
func (e *Engine) loadRun(ctx context.Context, workspaceID string, runID int64) (RunContext, error) {
	run, err := e.d.Store.GetRun(ctx, workspaceID, runID)
	if err != nil {
		return RunContext{}, err
	}
	v, err := e.d.Defs.GetVersionByID(ctx, workspaceID, run.VersionID)
	if err != nil {
		return RunContext{}, err
	}
	doc, err := ResolveVersion(v)
	if err != nil {
		return RunContext{}, errors.Join(ErrInvalid, err)
	}
	profile, err := e.d.Profiles.GetProfile(ctx, workspaceID)
	if err != nil {
		return RunContext{}, err
	}
	if doc.Language == "" {
		doc.Language = profile.Language
	}
	if doc.Language == "" {
		doc.Language = DefaultProfile().Language
	}
	trig, err := DecodeTriggerData(run.Trigger)
	if err != nil {
		return RunContext{}, errors.Join(ErrInvalid, err)
	}
	rc := RunContext{Run: run, Doc: doc, Profile: profile, Trigger: trig}
	if snap, err := DecodeContextSnapshot(run.Context); err == nil {
		rc.Context = snap
	}
	return rc, nil
}

// writeEnv is what ClickUp writes of a run use.
type writeEnv struct {
	taskID    string
	tokenRef  string
	dryRun    bool // the workspace's Phase 2 dry-run policy
	maxWrites int
}

func runTaskID(run Run) string {
	if id, ok := strings.CutPrefix(run.Subject, SubjectTaskPrefix); ok && taskIDRe.MatchString(id) {
		return id
	}
	return ""
}

func (e *Engine) env(ctx context.Context, rc RunContext) (writeEnv, error) {
	cfg, err := e.d.Policies.RunPolicy(ctx, rc.Run.WorkspaceID, rc.Trigger.IntegrationID)
	if err != nil {
		return writeEnv{}, err
	}
	n := 0
	for _, list := range rc.Doc.Actions {
		for _, a := range list {
			if strings.HasPrefix(a.Type, "clickup.") {
				n++
			}
		}
	}
	items := max(rc.Doc.Budgets.MaxItems, 1)
	return writeEnv{
		taskID: runTaskID(rc.Run), tokenRef: cfg.ClickUpTokenRef, dryRun: cfg.Tools.DryRun,
		// Every ClickUp action of every item, once more for a rebuilt
		// block, plus the undo writes.
		maxWrites: items*(n+1) + 10,
	}, nil
}

func needsClickUp(doc ResolvedDoc, items []Item) bool {
	classes := map[Class]bool{}
	for _, it := range items {
		classes[it.Class] = true
	}
	for c := range classes {
		for _, a := range doc.ActionsFor(c) {
			if strings.HasPrefix(a.Type, "clickup.") {
				return true
			}
		}
	}
	return false
}

func transientErr(err error) bool {
	var te *tools.ToolError
	if errors.As(err, &te) {
		return te.Retryable
	}
	return !errors.Is(err, context.Canceled)
}

func toolCode(err error) string {
	var te *tools.ToolError
	if errors.As(err, &te) && codeRe.MatchString(te.Code) {
		return te.Code
	}
	return CodeTaskUnreadable
}

// sendCode returns a safe code for a Messenger error, for logs.
func sendCode(err error) string {
	var de *relay.DeliveryError
	if errors.As(err, &de) && codeRe.MatchString(de.Code) {
		return de.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "send_failed"
}

// --- baseline and conflicts ---

// ensureBaseline stores the run's task state once, before its first write.
// Only a transient read failure is returned; a task that cannot be read
// gets no snapshot and its writes end as conflicts.
func (e *Engine) ensureBaseline(ctx context.Context, rc RunContext, env writeEnv) error {
	if env.taskID == "" || len(rc.Run.Snapshot) > 0 {
		return nil
	}
	st, err := e.d.Tasks.ReadTask(ctx, rc.Run.WorkspaceID, env.tokenRef, env.taskID)
	if err != nil {
		if transientErr(err) {
			return err
		}
		log.Printf("behavior run %d corr=%s: baseline read failed (%s)", rc.Run.ID, rc.Run.CorrelationID, toolCode(err))
		return nil
	}
	snap := runSnapshot{
		TaskID: env.taskID, DescriptionBefore: st.Markdown, Status: st.Status, AssigneeIDs: st.AssigneeIDs,
		Drift: e.contextDrift(rc, env.taskID, st), CapturedAt: e.now().UnixNano(),
	}
	if h, err := descblock.OutsideHash(st.Markdown); err == nil {
		snap.OutsideSHA256 = h
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if _, err := e.d.Store.SetSnapshot(ctx, rc.Run.WorkspaceID, rc.Run.ID, raw); err != nil {
		return err
	}
	if snap.Drift {
		log.Printf("behavior run %d corr=%s: task changed since the run read it", rc.Run.ID, rc.Run.CorrelationID)
	}
	return nil
}

// contextDrift compares the task's plain description outside managed
// blocks with what the run read (the context snapshot), rendered the same
// way the read was. It reports a change only when the comparison is
// reliable: the read was not cut and the description fits the read cap.
func (e *Engine) contextDrift(rc RunContext, taskID string, st TaskState) bool {
	snap := rc.Context
	base := "clickup:task/" + taskID
	if snap.Truncated || snap.Fragment(base+"#meta") == nil {
		return false
	}
	for _, n := range snap.Notes {
		if n.Tool == precheckTool && (n.Code == NoteTruncated || n.Code == NoteSnapshotCap) {
			return false
		}
	}
	var read []string
	if f := snap.Fragment(base + "#desc"); f != nil {
		read = f.Lines
	}
	if len(read) > 0 {
		last := read[len(read)-1]
		if strings.HasSuffix(last, "…") || strings.Contains(last, "[truncated]") {
			return false
		}
	}
	maxRunes := DefaultReadBytes
	for _, r := range rc.Doc.Reads {
		if r.Tool == precheckTool {
			maxRunes = readCap(r)
		}
	}
	cur := strings.TrimSpace(st.Description)
	if utf8.RuneCountInString(cur) > maxRunes {
		return false
	}
	curLines := lines(e.d.Redactor.CleanOutbound(cur, false))
	for i, l := range curLines {
		curLines[i] = e.d.Redactor.RedactSecrets(l)
	}
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return collapse(descblock.OutsidePlain(read)) != collapse(descblock.OutsidePlain(curLines))
}

// checkTarget re-reads the run's task before a ClickUp content write. It
// returns the current outside-block hash, or a conflict code when the
// human-written text changed since the run read it (or the markers are
// broken). err is a transient read failure or a storage error.
func (e *Engine) checkTarget(ctx context.Context, rc RunContext, env writeEnv) (outside, code string, err error) {
	if env.taskID == "" {
		return "", CodeNoTask, nil
	}
	run, err := e.d.Store.GetRun(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return "", "", err
	}
	if len(run.Snapshot) == 0 {
		rc.Run.Snapshot = nil
		if err := e.ensureBaseline(ctx, rc, env); err != nil {
			return "", "", err
		}
		if run, err = e.d.Store.GetRun(ctx, rc.Run.WorkspaceID, rc.Run.ID); err != nil {
			return "", "", err
		}
	}
	snap := decodeSnapshot(run.Snapshot)
	if snap == nil {
		return "", CodeTaskUnreadable, nil
	}
	if snap.Drift {
		return "", CodeChangedSinceRead, nil
	}
	st, err := e.d.Tasks.ReadTask(ctx, rc.Run.WorkspaceID, env.tokenRef, env.taskID)
	if err != nil {
		if transientErr(err) {
			return "", "", err
		}
		return "", toolCode(err), nil
	}
	h, herr := descblock.OutsideHash(st.Markdown)
	if herr != nil {
		return "", CodeMarkersInvalid, nil
	}
	accepted := map[string]bool{}
	if snap.OutsideSHA256 != "" {
		accepted[snap.OutsideSHA256] = true
	}
	items, err := e.d.Store.ListItems(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return "", "", err
	}
	for _, it := range items {
		for _, o := range decodeResult(it.ActionResult).Actions {
			if o.OutsideSHA != "" {
				accepted[o.OutsideSHA] = true
			}
		}
	}
	if !accepted[h] {
		return "", CodeChangedSinceRead, nil
	}
	return h, "", nil
}

// --- tool calls ---

var descHashRe = regexp.MustCompile(`desc_sha256=([0-9a-f]{64})`)
var outsideHashRe = regexp.MustCompile(`outside_sha256=([0-9a-f]{64})`)

func (e *Engine) callWrite(ctx context.Context, rc RunContext, env writeEnv, scope, tool string, input any) tools.CallResult {
	raw, err := json.Marshal(input)
	if err != nil {
		return tools.CallResult{Content: "error: invalid_input", IsError: true, Status: tools.StatusError}
	}
	policy := tenant.ToolPolicyConfig{Enabled: []string{tool}, DryRun: env.dryRun, MaxWritesPerJob: env.maxWrites}
	return e.d.Callers(rc.Run, scope, policy, env.tokenRef).Call(ctx, 1, tool, raw)
}

func actionScope(itemID int64, index int) string {
	return fmt.Sprintf("behavior_action:%d:%d", itemID, index)
}

// outcomeOf maps a tool result to an outcome; transient is a failure that
// had no effect and may be tried again.
func outcomeOf(res tools.CallResult, index int, typ string) (o actionOutcome, transient bool) {
	o = actionOutcome{Index: index, Type: typ, Status: OutcomeOK}
	switch {
	case res.Status == tools.StatusDryRun:
		o.Status = OutcomePolicyDryRun
	case res.Status == tools.StatusUnknownOutcome:
		o.Status, o.Code = OutcomeUnknown, resultCode(res)
	case res.IsError:
		code := resultCode(res)
		o.Status, o.Code = OutcomeError, code
		if code == "conflict" {
			o.Status, o.Code = OutcomeConflict, CodeChangedSinceRead
		} else if code == CodeMarkersInvalid {
			o.Status = OutcomeConflict
		}
		transient = res.Status != tools.StatusDenied && (res.Status == tools.StatusTimeout || transientCodes[code])
	}
	return o, transient
}

// errTransient asks the caller to retry later; nothing took effect.
var errTransient = errors.New("behavior: transient action failure")

// answerOf is the text an action writes for an item without an owner
// answer: its proposal, or its text when it has none.
func answerOf(it Item) string {
	if strings.TrimSpace(it.Proposal) != "" {
		return it.Proposal
	}
	return it.Text
}

// change is one confirmed-only change (set_status, unassign).
type change struct {
	Index int    `json:"i"`
	Type  string `json:"type"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	User  string `json:"user,omitempty"`
}

// confirmPayload is a confirm_action approval's payload.
type confirmPayload struct {
	TaskID  string   `json:"task_id"`
	Changes []change `json:"changes"`
}

// execute runs a class's actions for one item with answer as the action
// text. It returns the item's final status and action result. Status and
// assignee changes are not run: they become one confirmation request.
// With retry set (a dispatch), a transient failure returns errTransient so
// the run is tried again; otherwise it ends the item as failed.
func (e *Engine) execute(ctx context.Context, rc RunContext, env writeEnv, chat ChatRef, it Item, class Class, answer string, retry bool) (ItemStatus, json.RawMessage, error) {
	final := ItemDone
	if class == ClassAsk {
		final = ItemAnswered
	}
	res := decodeResult(it.ActionResult)
	checked := false
	outside := ""
	var changes []change
	for i, a := range rc.Doc.ActionsFor(class) {
		switch a.Type {
		case ActionTelegramMessage:
			res.set(actionOutcome{Index: i, Type: a.Type, Status: OutcomeDelivered})
			continue
		case ActionSetStatus, ActionUnassign:
			ch, o, err := e.planChange(ctx, rc, env, i, a)
			if err != nil {
				if retry {
					return "", nil, err
				}
				o = actionOutcome{Index: i, Type: a.Type, Status: OutcomeError, Code: toolCode(err)}
			}
			if ch != nil {
				changes = append(changes, *ch)
				o.Status = OutcomeAwaitingConfirm
			}
			res.set(o)
			continue
		}
		if !checked {
			h, code, err := e.checkTarget(ctx, rc, env)
			if err != nil {
				if retry {
					return "", nil, err
				}
				code = toolCode(err)
			}
			if code != "" {
				res.set(actionOutcome{Index: i, Type: a.Type, Status: OutcomeConflict, Code: code})
				return ItemConflict, res.encode(), nil
			}
			checked, outside = true, h
		}
		var out tools.CallResult
		switch a.Type {
		case ActionClickUpComment:
			text := capRunes(e.d.Redactor.CleanOutbound(strings.TrimSpace(answer), true), 7900)
			if text == "" {
				res.set(actionOutcome{Index: i, Type: a.Type, Status: OutcomeError, Code: "empty_text"})
				return ItemFailed, res.encode(), nil
			}
			out = e.callWrite(ctx, rc, env, actionScope(it.ID, i), ToolPostComment, map[string]any{"task_id": env.taskID, "text": text})
		case ActionDescriptionBlock:
			block := a.Params["block"]
			body, err := e.renderBlock(ctx, rc, block, it, answer)
			if err != nil {
				return "", nil, err
			}
			out = e.callWrite(ctx, rc, env, actionScope(it.ID, i), ToolSetDescriptionBlock,
				map[string]any{"task_id": env.taskID, "block": block, "body": body, "if_outside_sha256": outside})
		default:
			res.set(actionOutcome{Index: i, Type: a.Type, Status: OutcomeError, Code: "unknown_action"})
			return ItemFailed, res.encode(), nil
		}
		o, transient := outcomeOf(out, i, a.Type)
		if a.Type == ActionDescriptionBlock {
			o.Block = a.Params["block"]
			if o.Status == OutcomeOK {
				if m := descHashRe.FindStringSubmatch(out.Content); m != nil {
					o.DescSHA, o.At = m[1], e.now().UnixNano()
				}
				if m := outsideHashRe.FindStringSubmatch(out.Content); m != nil {
					o.OutsideSHA = m[1]
				}
			}
		}
		if transient && retry {
			return "", nil, errTransient
		}
		res.set(o)
		switch o.Status {
		case OutcomeConflict:
			return ItemConflict, res.encode(), nil
		case OutcomeError, OutcomeUnknown:
			return ItemFailed, res.encode(), nil
		}
	}
	if len(changes) > 0 {
		if err := e.requestConfirm(ctx, rc, env, chat, it, changes); err != nil {
			if retry {
				return "", nil, err
			}
			log.Printf("behavior run %d item %d corr=%s: confirmation not sent", rc.Run.ID, it.ID, rc.Run.CorrelationID)
		}
	}
	return final, res.encode(), nil
}

// planChange reads the task and returns the change a set_status or
// unassign action would make, or nil with an outcome when nothing is to do.
func (e *Engine) planChange(ctx context.Context, rc RunContext, env writeEnv, i int, a ActionSpec) (*change, actionOutcome, error) {
	o := actionOutcome{Index: i, Type: a.Type}
	if env.taskID == "" {
		o.Status, o.Code = OutcomeError, CodeNoTask
		return nil, o, nil
	}
	st, err := e.d.Tasks.ReadTask(ctx, rc.Run.WorkspaceID, env.tokenRef, env.taskID)
	if err != nil {
		if transientErr(err) {
			return nil, o, err
		}
		o.Status, o.Code = OutcomeError, toolCode(err)
		return nil, o, nil
	}
	switch a.Type {
	case ActionSetStatus:
		to := a.Params["status"]
		if strings.EqualFold(st.Status, to) {
			o.Status = OutcomeAlready
			return nil, o, nil
		}
		return &change{Index: i, Type: a.Type, From: st.Status, To: to}, o, nil
	default:
		user := a.Params["user"]
		if user == AssigneeMe {
			user = e.s.OwnerClickUpUserID
		}
		if user == "" {
			o.Status, o.Code = OutcomeError, CodeOwnerUnset
			return nil, o, nil
		}
		if !slices.Contains(st.AssigneeIDs, user) {
			o.Status = OutcomeAlready
			return nil, o, nil
		}
		return &change{Index: i, Type: a.Type, User: user}, o, nil
	}
}

// requestConfirm creates the item's confirm_action approval (one per item,
// listing every change) and sends it with "Xác nhận" / "Huỷ".
func (e *Engine) requestConfirm(ctx context.Context, rc RunContext, env writeEnv, chat ChatRef, it Item, changes []change) error {
	payload, _ := json.Marshal(confirmPayload{TaskID: env.taskID, Changes: changes})
	types := make([]string, 0, len(changes))
	for _, c := range changes {
		types = append(types, c.Type)
	}
	a, err := e.ensureApproval(ctx, rc.Run.WorkspaceID, NewApproval{
		Kind: ApprovalConfirmAction, RunID: rc.Run.ID, ItemID: it.ID, Action: strings.Join(types, ","),
		Resource: rc.Run.Subject, Payload: payload, ExpiresAt: e.expiry(rc.Doc),
	}, false)
	if err != nil || a.Decision != "" {
		return err
	}
	var p confirmPayload
	_ = json.Unmarshal(a.Payload, &p)
	t := textsFor(rc.Doc.Language)
	var b strings.Builder
	b.WriteString(t.confirmHead() + " (" + t.runRef(rc.Doc.Name, rc.Run.ID) + ")\n")
	for _, c := range p.Changes {
		if c.Type == ActionSetStatus {
			b.WriteString(t.changeStatus(p.TaskID, c.From, c.To) + "\n")
		} else {
			b.WriteString(t.changeUnassign(p.TaskID, c.User) + "\n")
		}
	}
	msg := OutMessage{Text: capRunes(strings.TrimSpace(b.String()), maxTelegramRunes), Buttons: [][]Button{
		e.buttons(a.Nonce, Button{Text: t.btnConfirm()}, CallbackApprove, Button{Text: t.btnCancel()}, CallbackReject),
	}}
	return e.sendApproval(ctx, rc, chat, a, msg)
}

// maxRenderedBlockBytes keeps a rendered block, once JSON-escaped, under
// the executor's 16 KiB input cap.
const maxRenderedBlockBytes = 9000

// renderBlock builds a description block body from every item of the run
// whose class writes this block and that is done or answered (plus the
// item being written, with its answer): one entry per item, its label (or
// key) and its answer. It is always rebuilt from all of them, so repeated
// writes and later answers converge on the same text.
func (e *Engine) renderBlock(ctx context.Context, rc RunContext, block string, cur Item, curAnswer string) (string, error) {
	items, err := e.d.Store.ListItems(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return "", err
	}
	approvals, err := e.d.Store.ListApprovals(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return "", err
	}
	answers := map[int64]string{}
	for _, a := range approvals {
		if a.Kind == ApprovalItemAnswer && a.Decision == DecisionAnswered {
			answers[a.ItemID] = a.Answer
		}
	}
	writes := func(c Class) bool {
		for _, a := range rc.Doc.ActionsFor(c) {
			if a.Type == ActionDescriptionBlock && a.Params["block"] == block {
				return true
			}
		}
		return false
	}
	clean := func(s string) string {
		return descblock.Neutralize(e.d.Redactor.CleanOutbound(strings.TrimSpace(s), true))
	}
	var entries []string
	size := 0
	for _, it := range items {
		var ans string
		switch {
		case it.ID == cur.ID:
			ans = curAnswer
		case !writes(it.Class):
			continue
		case it.Status == ItemAnswered:
			ans = answers[it.ID]
		case it.Status == ItemDone:
			ans = answerOf(it)
		default:
			continue
		}
		label := it.Label
		if strings.TrimSpace(label) == "" {
			label = it.Key
		}
		entry := "**" + capRunes(clean(strings.Join(strings.Fields(label), " ")), 200) + "**\n" + capRunes(clean(ans), 2000)
		if size+len(entry)+2 > maxRenderedBlockBytes {
			entries = append(entries, "…")
			break
		}
		size += len(entry) + 2
		entries = append(entries, entry)
	}
	return strings.Join(entries, "\n\n"), nil
}

// --- approvals and messages ---

func (e *Engine) expiry(doc ResolvedDoc) time.Time {
	h := doc.AskExpiryHours
	if h <= 0 {
		h = DefaultAskExpiryHours
	}
	return e.now().Add(time.Duration(h) * time.Hour)
}

// ensureApproval returns the item's approval of the kind, creating it when
// none exists. openOnly ignores decided or expired ones.
func (e *Engine) ensureApproval(ctx context.Context, ws string, na NewApproval, openOnly bool) (Approval, error) {
	find := func() (Approval, bool, error) {
		list, err := e.d.Store.ListApprovals(ctx, ws, na.RunID)
		if err != nil {
			return Approval{}, false, err
		}
		for i := len(list) - 1; i >= 0; i-- {
			a := list[i]
			if a.ItemID != na.ItemID || a.Kind != na.Kind {
				continue
			}
			if openOnly && (a.Decision != "" || !e.now().Before(a.ExpiresAt)) {
				continue
			}
			return a, true, nil
		}
		return Approval{}, false, nil
	}
	if a, ok, err := find(); err != nil || ok {
		return a, err
	}
	nonce, err := NewNonce()
	if err != nil {
		return Approval{}, err
	}
	na.Nonce = nonce
	if len(na.Payload) == 0 {
		na.Payload = json.RawMessage(`{}`)
	}
	if len(na.Options) == 0 {
		na.Options = json.RawMessage(`[]`)
	}
	a, err := e.d.Store.CreateApproval(ctx, ws, na)
	if errors.Is(err, ErrConflict) {
		if a, ok, ferr := find(); ferr != nil || ok {
			return a, ferr
		}
	}
	return a, err
}

// buttons builds one row of approval buttons from (button, kind) pairs.
func (e *Engine) buttons(nonce string, pairs ...any) []Button {
	var row []Button
	for i := 0; i+1 < len(pairs); i += 2 {
		b, _ := pairs[i].(Button)
		kind, _ := pairs[i+1].(CallbackKind)
		data, err := CallbackData{Kind: kind, Nonce: nonce, Option: -1}.Encode()
		if err != nil {
			continue
		}
		b.Data = data
		row = append(row, b)
	}
	return row
}

func (e *Engine) feedbackRow(t texts, nonce string) []Button {
	return e.buttons(nonce, Button{Text: t.btnUp()}, CallbackRateUp, Button{Text: t.btnDown()}, CallbackRateDown)
}

// sendOnce sends a message of the run under key at most once. It returns
// the message ID (0 when an earlier send's outcome is unknown).
func (e *Engine) sendOnce(ctx context.Context, run Run, key string, chat ChatRef, m OutMessage) (int64, error) {
	ws, runID := run.WorkspaceID, run.ID
	claimed, id, err := e.d.Store.ClaimMessage(ctx, ws, runID, key)
	if err != nil || !claimed {
		return id, err
	}
	sctx, cancel := context.WithTimeout(ctx, e.s.RequestTimeout)
	id, err = e.d.Messenger.SendMessage(sctx, chat, m)
	cancel()
	if err != nil {
		log.Printf("behavior run %d corr=%s: message %s not sent (%s)", runID, run.CorrelationID, key, sendCode(err))
		if rerr := e.d.Store.ReleaseMessage(context.WithoutCancel(ctx), ws, runID, key); rerr != nil {
			log.Printf("behavior run %d corr=%s: message %s claim not released", runID, run.CorrelationID, key)
		}
		return 0, err
	}
	if err := e.d.Store.SetMessageID(ctx, ws, runID, key, id); err != nil {
		log.Printf("behavior run %d corr=%s: message %s sent, id not stored", runID, run.CorrelationID, key)
		return id, err
	}
	log.Printf("behavior run %d corr=%s: message %s sent", runID, run.CorrelationID, key)
	return id, nil
}

// sendApproval sends an approval's message once and records its ID.
func (e *Engine) sendApproval(ctx context.Context, rc RunContext, chat ChatRef, a Approval, m OutMessage) error {
	if a.TelegramMessageID != 0 {
		return nil
	}
	id, err := e.sendOnce(ctx, rc.Run, fmt.Sprintf("approval:%d", a.ID), chat, m)
	if err != nil || id == 0 {
		return err
	}
	return e.d.Store.SetApprovalMessage(ctx, rc.Run.WorkspaceID, a.ID, id)
}

// citations renders an item's citations as short locators.
func citations(cs []Citation) string {
	var out []string
	for i, c := range cs {
		if i == 5 {
			out = append(out, "…")
			break
		}
		out = append(out, capRunes(c.Locator, 80))
	}
	return strings.Join(out, ", ")
}

// itemText is the body of a draft or ask message.
func itemText(t texts, head, proposalLabel string, rc RunContext, it Item) string {
	var b strings.Builder
	b.WriteString(head + " (" + t.runRef(rc.Doc.Name, rc.Run.ID) + ")\n")
	label := it.Label
	if strings.TrimSpace(label) == "" {
		label = it.Key
	}
	b.WriteString(capRunes(label, 200) + ": " + capRunes(it.Text, 1800) + "\n")
	if strings.TrimSpace(it.Proposal) != "" {
		b.WriteString(proposalLabel + ": " + capRunes(it.Proposal, 1500) + "\n")
	}
	if len(it.Citations) > 0 {
		b.WriteString(t.sourcesLabel() + ": " + citations(it.Citations))
	}
	return capRunes(strings.TrimSpace(b.String()), maxTelegramRunes)
}

func (e *Engine) sendDraft(ctx context.Context, rc RunContext, chat ChatRef, it Item) error {
	a, err := e.ensureApproval(ctx, rc.Run.WorkspaceID, NewApproval{
		Kind: ApprovalPostDraft, RunID: rc.Run.ID, ItemID: it.ID, Action: string(ClassAuto), Resource: rc.Run.Subject,
		ExpiresAt: e.expiry(rc.Doc),
	}, false)
	if err != nil || a.Decision != "" {
		return err
	}
	t := textsFor(rc.Doc.Language)
	msg := OutMessage{Text: itemText(t, t.draftHead(), t.proposalLabel(), rc, it), Buttons: [][]Button{
		e.buttons(a.Nonce, Button{Text: t.btnPost()}, CallbackApprove, Button{Text: t.btnSkip()}, CallbackReject),
		e.feedbackRow(t, a.Nonce),
	}}
	return e.sendApproval(ctx, rc, chat, a, msg)
}

func (e *Engine) sendAsk(ctx context.Context, rc RunContext, chat ChatRef, it Item) error {
	options := []string{}
	if strings.TrimSpace(it.Proposal) != "" {
		options = append(options, it.Proposal)
	}
	opts, _ := json.Marshal(options)
	a, err := e.ensureApproval(ctx, rc.Run.WorkspaceID, NewApproval{
		Kind: ApprovalItemAnswer, RunID: rc.Run.ID, ItemID: it.ID, Action: string(ClassAsk), Resource: rc.Run.Subject,
		Options: opts, ExpiresAt: e.expiry(rc.Doc),
	}, false)
	if err != nil || a.Decision != "" {
		return err
	}
	t := textsFor(rc.Doc.Language)
	var rows [][]Button
	if len(options) > 0 {
		if data, err := (CallbackData{Kind: CallbackOption, Nonce: a.Nonce, Option: 0}).Encode(); err == nil {
			rows = append(rows, []Button{{Text: t.btnAgree(), Data: data}})
		}
	}
	rows = append(rows, e.buttons(a.Nonce, Button{Text: t.btnSkip()}, CallbackReject, Button{Text: t.btnOther()}, CallbackFreeText))
	rows = append(rows, e.feedbackRow(t, a.Nonce))
	msg := OutMessage{Text: itemText(t, t.askHead(), t.suggestLabel(), rc, it), Buttons: rows}
	return e.sendApproval(ctx, rc, chat, a, msg)
}

// summaryLines lists items one per line: label, answer (clipped to per
// runes) and, with withStatus, the item status.
func summaryLines(t texts, items []Item, withStatus bool, per int) string {
	var b strings.Builder
	for _, it := range items {
		label := it.Label
		if strings.TrimSpace(label) == "" {
			label = it.Key
		}
		line := "• " + capRunes(label, 120) + ": " + capRunes(strings.Join(strings.Fields(answerOf(it)), " "), per)
		if withStatus {
			line += " [" + t.statusWord(it.Status) + "]"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// summary sends a digest or FYI listing items, once per run under key. A
// summary about one item carries 👍/👎 through a feedback approval.
func (e *Engine) summary(ctx context.Context, rc RunContext, chat ChatRef, key, head string, items []Item, withStatus bool) error {
	if len(items) == 0 {
		return nil
	}
	t := textsFor(rc.Doc.Language)
	kind := reporting.KindNotify
	if key == DigestKeyFYI {
		kind = reporting.KindFYI
	}
	if rc.Trigger.Mentioned {
		kind = reporting.KindMention
	}
	text, err := e.d.Reports.Render(ctx, reporting.Report{
		Kind: kind, UserID: reporting.DefaultOwnerUserID, WorkspaceID: rc.Run.WorkspaceID,
		Behavior: rc.Run.BehaviorName, Language: rc.Doc.Language, Platform: "clickup", ExternalID: rc.Trigger.SenderID,
		Values: reporting.Values{
			AISummary: head + "\n" + strings.TrimSpace(summaryLines(t, items, withStatus, max(maxTelegramRunes/len(items)-40, 60))),
			Link:      RunLink(rc), Behavior: rc.Doc.Name, RunID: fmt.Sprint(rc.Run.ID), Channel: rc.Trigger.ChannelName,
		}, TraceHint: fmt.Sprintf("/trace %d", rc.Run.ID),
	})
	if err != nil {
		return err
	}
	text = capRunes(text, maxTelegramRunes)
	msg := OutMessage{Text: text}
	if len(items) == 1 {
		fb, err := e.ensureApproval(ctx, rc.Run.WorkspaceID, NewApproval{
			Kind: ApprovalFeedback, RunID: rc.Run.ID, ItemID: items[0].ID, Action: key, Resource: rc.Run.Subject,
			ExpiresAt: e.expiry(rc.Doc),
		}, false)
		if err != nil {
			return err
		}
		msg.Buttons = [][]Button{e.feedbackRow(t, fb.Nonce)}
	}
	_, err = e.sendOnce(ctx, rc.Run, key, chat, msg)
	return err
}

// clickUpAppURL is the ClickUp web app.
const clickUpAppURL = "https://app.clickup.com"

// RunLink is the ClickUp web link of what a run is about, built only from
// the run's subject and trusted trigger data: the task
// (https://app.clickup.com/t/<task_id>) for a task subject, the chat
// channel (https://app.clickup.com/<team_id>/chat/r/<channel_id>) for a
// chat subject. "" when the run has neither or an identifier does not
// validate.
func RunLink(rc RunContext) string {
	if id, ok := strings.CutPrefix(rc.Run.Subject, SubjectTaskPrefix); ok {
		if !taskIDRe.MatchString(id) {
			return ""
		}
		return clickUpAppURL + "/t/" + id
	}
	if id, ok := strings.CutPrefix(rc.Run.Subject, SubjectChatPrefix); ok {
		t := rc.Trigger
		if id != t.MessageID || !chatIDRe.MatchString(id) || !chatTeamIDRe.MatchString(t.TeamID) || !chatIDRe.MatchString(t.ChannelID) {
			return ""
		}
		return clickUpAppURL + "/" + t.TeamID + "/chat/r/" + t.ChannelID
	}
	return ""
}

// --- Dispatcher ---

// Dispatch delivers a run's items by class. It may be called again for the
// same run: approvals are found again, every message is sent once per key,
// items move only from drafted, and every ClickUp write is keyed by (item,
// action) in the executor.
func (e *Engine) Dispatch(ctx context.Context, rc RunContext, items []Item) error {
	ws := rc.Run.WorkspaceID
	chat, err := e.d.Chats.OwnerChat(ctx, ws, rc.Trigger)
	if err != nil {
		return err
	}
	var env writeEnv
	if needsClickUp(rc.Doc, items) {
		if env, err = e.env(ctx, rc); err != nil {
			return err
		}
		if err := e.ensureBaseline(ctx, rc, env); err != nil {
			return err
		}
	}
	t := textsFor(rc.Doc.Language)
	var autos, notifies []Item
	for _, it := range items {
		switch it.Class {
		case ClassAuto:
			if rc.Doc.DryRun {
				if it.Status == ItemDrafted {
					if err := e.sendDraft(ctx, rc, chat, it); err != nil {
						return err
					}
				}
				continue
			}
			if it.Status == ItemDrafted {
				if it, err = e.finish(ctx, rc, env, chat, it, ClassAuto, answerOf(it), ItemDrafted); err != nil {
					return err
				}
			}
			autos = append(autos, it)
		case ClassAsk:
			if it.Status != ItemDrafted {
				continue
			}
			if err := e.sendAsk(ctx, rc, chat, it); err != nil {
				return err
			}
			if _, err := e.d.Store.TransitionItem(ctx, ws, it.ID, []ItemStatus{ItemDrafted}, ItemAsked, nil); err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		case ClassNotify:
			if it.Status == ItemDrafted {
				if it, err = e.finish(ctx, rc, env, chat, it, ClassNotify, answerOf(it), ItemDrafted); err != nil {
					return err
				}
			}
			notifies = append(notifies, it)
		}
	}
	// Digest and FYI follow the profile's quiet hours and digest mode.
	if err := e.inform(ctx, rc, chat, DigestKeyNotify, t.digestHead(), notifies, false); err != nil {
		return err
	}
	return e.inform(ctx, rc, chat, DigestKeyFYI, t.fyiHead(), autos, true)
}

// finish executes a class's actions for an item and moves it from `from`
// to its final status. A lost race (the item already moved) keeps the
// stored item.
func (e *Engine) finish(ctx context.Context, rc RunContext, env writeEnv, chat ChatRef, it Item, class Class, answer string, from ItemStatus) (Item, error) {
	status, result, err := e.execute(ctx, rc, env, chat, it, class, answer, true)
	if err != nil {
		return it, err
	}
	if status == ItemAnswered && class != ClassAsk {
		status = ItemDone
	}
	next, err := e.d.Store.TransitionItem(ctx, rc.Run.WorkspaceID, it.ID, []ItemStatus{from}, status, result)
	if errors.Is(err, ErrConflict) {
		return e.d.Store.GetItem(ctx, rc.Run.WorkspaceID, it.ID)
	}
	if err != nil {
		return it, err
	}
	log.Printf("behavior run %d item %d corr=%s: %s", rc.Run.ID, it.ID, rc.Run.CorrelationID, status)
	return next, nil
}

// --- Notifier ---

// RunFailed tells the owner once that a run failed, by run ID and code.
func (e *Engine) RunFailed(ctx context.Context, rc RunContext, code string) error {
	chat, err := e.d.Chats.OwnerChat(ctx, rc.Run.WorkspaceID, rc.Trigger)
	if err != nil {
		return err
	}
	name := rc.Doc.Name
	if name == "" {
		name = rc.Run.BehaviorName
	}
	if name == "" {
		name = "behavior"
	}
	if !codeRe.MatchString(code) {
		code = "failed"
	}
	text, renderErr := e.d.Reports.Render(ctx, reporting.Report{
		Kind: reporting.KindFailure, UserID: reporting.DefaultOwnerUserID, WorkspaceID: rc.Run.WorkspaceID,
		Behavior: rc.Run.BehaviorName, Language: rc.Doc.Language, Platform: "clickup", ExternalID: rc.Trigger.SenderID,
		Values:    reporting.Values{Reason: code, Behavior: name, RunID: fmt.Sprint(rc.Run.ID)},
		TraceHint: fmt.Sprintf("/trace %d", rc.Run.ID),
	})
	if renderErr != nil {
		return renderErr
	}
	_, err = e.sendOnce(ctx, rc.Run, "failed", chat, OutMessage{Text: text})
	return err
}
