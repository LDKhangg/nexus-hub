package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is the run-level action executor (actions.run): actions that
// run once per run, in declared order, after the model step (or instead of
// it), on trusted trigger data only. Their targets (the pull requests, the
// chat message and its ClickUp workspace) come from the run's stored
// trigger data, never from model output. Each outcome is stored on the run
// (behavior_runs.run_actions) as it ends, so a retried run never repeats
// an action already recorded; the executor's idempotency keys (scope per
// run, action and target) cover a crash between a call and its record.

// Run-level outcome statuses, besides OutcomeOK, OutcomeError,
// OutcomeUnknown and OutcomePolicyDryRun.
const (
	// OutcomeDryRun: the behavior's dry_run is on; no write was made.
	OutcomeDryRun = "dry_run"
	// OutcomeSkippedPrevious: the action has on: success_of_previous and
	// the action before it did not end OK.
	OutcomeSkippedPrevious = "skipped_previous_failed"
	// OutcomePending: the action has started and is resumed by a retry
	// (some of its targets have no final outcome yet).
	OutcomePending = "pending"
	// OutcomeSkippedVerdict: the action has a "when" condition the run's
	// verdict did not meet, or the run has no verdict.
	OutcomeSkippedVerdict = "skipped_verdict"
)

// Codes of OutcomeSkippedVerdict.
const (
	// CodeVerdictUnavailable: the model step gave no verdict (a model
	// failure, a timeout, invalid output or an exhausted budget).
	CodeVerdictUnavailable = "verdict_unavailable"
	// CodeVerdictMismatch: the verdict has another label than the
	// condition's.
	CodeVerdictMismatch = "verdict_mismatch"
	// CodeVerdictLowConfidence: the verdict has the condition's label but
	// less than the behavior's min_confidence.
	CodeVerdictLowConfidence = "verdict_low_confidence"
)

// Engine codes of run-level action outcomes.
const (
	CodeNoPR        = "no_pr"
	CodeNoMessage   = "no_message"
	CodeNoTeam      = "no_team"
	CodeSendFailed  = "send_failed"
	CodeBadTarget   = "invalid_target"
	codeUnknownType = "unknown_action"
)

// RunActionsResult is a run's run-level action outcomes, stored as JSON in
// behavior_runs.run_actions. It holds identifiers and codes only, and the
// run's verdict, whose summary is model output (cleaned and redacted),
// never the triggering message text.
type RunActionsResult struct {
	Actions []RunActionOutcome `json:"actions"`
	// Verdict is the run's model verdict (behaviors with a verdict block),
	// recorded by the model step before any run-level action runs.
	Verdict *RunVerdict `json:"verdict,omitempty"`
}

// RunVerdict is a run's model verdict as stored. Summary is model output,
// cleaned and redacted, never the triggering text itself. Flags are the
// injection flags the engine found in the untrusted triggering content;
// a flagged verdict's confidence is capped (VerdictFlagCap). When the model
// step gave no verdict, Label is empty and Code names why.
type RunVerdict struct {
	Label      string   `json:"label,omitempty"`
	Confidence float64  `json:"confidence"`
	Summary    string   `json:"summary,omitempty"`
	Flags      []string `json:"flags,omitempty"`
	Code       string   `json:"code,omitempty"`
}

// Available reports whether the model step gave a verdict.
func (v *RunVerdict) Available() bool { return v != nil && v.Label != "" }

// VerdictFlagCap is the highest confidence a verdict keeps when its
// triggering content is flagged as instruction-like.
const VerdictFlagCap = 0.5

// gate decides a "when" condition against the run's verdict: ok, or the
// skip code.
func gate(w *ActionWhen, spec *VerdictSpec, v *RunVerdict) (bool, string) {
	switch {
	case spec == nil || !v.Available():
		return false, CodeVerdictUnavailable
	case v.Label != w.Verdict:
		return false, CodeVerdictMismatch
	case !(v.Confidence >= spec.MinConfidence):
		return false, CodeVerdictLowConfidence
	}
	return true, ""
}

// RunActionOutcome is the outcome of one run-level action (actions.run[i]).
type RunActionOutcome struct {
	// Index is the action's position in actions.run.
	Index int    `json:"i"`
	Type  string `json:"type"`
	// Status is OutcomeOK, OutcomeError, OutcomeUnknown, OutcomeDryRun,
	// OutcomePolicyDryRun, OutcomeSkippedPrevious or OutcomePending.
	Status string `json:"status"`
	// Code names a failure (an engine or tool code).
	Code string `json:"code,omitempty"`
	// PRs are the per pull request outcomes of github.pr_approve, in
	// trigger order.
	PRs []RunPROutcome `json:"prs,omitempty"`
	// TeamID, MessageID and Reaction are the target of
	// clickup.chat_reaction; Result is the tool's outcome (reacted or
	// already_reacted).
	TeamID    string `json:"team_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Reaction  string `json:"reaction,omitempty"`
	Result    string `json:"result,omitempty"`
	// TelegramMessageID is the owner message of telegram.message (0 when
	// the send's outcome is unknown).
	TelegramMessageID int64 `json:"telegram_message_id,omitempty"`
	// VerdictLabel and VerdictConfidence are the run's verdict when the
	// action was skipped by its "when" condition (OutcomeSkippedVerdict);
	// both are empty when the run had no verdict.
	VerdictLabel      string   `json:"verdict_label,omitempty"`
	VerdictConfidence *float64 `json:"verdict_confidence,omitempty"`
}

// RunPROutcome is the outcome of approving one pull request.
type RunPROutcome struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Status is OutcomeOK, OutcomeError, OutcomeUnknown, OutcomeDryRun or
	// OutcomePolicyDryRun.
	Status string `json:"status"`
	// Result is the tool's outcome: approved or already_approved.
	Result   string `json:"result,omitempty"`
	Code     string `json:"code,omitempty"`
	ReviewID int64  `json:"review_id,omitempty"`
}

// Tool outcomes read from the run-level write tools' output, which the
// owner report and undo tell apart.
const (
	resultApproved        = "approved"
	resultAlreadyApproved = "already_approved"
	resultReacted         = "reacted"
	resultAlreadyReacted  = "already_reacted"
)

// DecodeRunActions reads a run's stored run-level outcomes; empty or
// invalid JSON gives an empty result.
func DecodeRunActions(raw json.RawMessage) RunActionsResult {
	var r RunActionsResult
	if len(raw) > 0 {
		if json.Unmarshal(raw, &r) != nil {
			return RunActionsResult{}
		}
	}
	return r
}

// Get returns the stored outcome of actions.run[index].
func (r RunActionsResult) Get(index int) (RunActionOutcome, bool) {
	for _, o := range r.Actions {
		if o.Index == index {
			return o, true
		}
	}
	return RunActionOutcome{}, false
}

func (r *RunActionsResult) set(o RunActionOutcome) {
	for i := range r.Actions {
		if r.Actions[i].Index == o.Index {
			r.Actions[i] = o
			return
		}
	}
	r.Actions = append(r.Actions, o)
}

func (r RunActionsResult) encode() json.RawMessage {
	if r.Actions == nil {
		r.Actions = []RunActionOutcome{}
	}
	b, _ := json.Marshal(r)
	return b
}

// final reports whether the outcome is the action's last word.
func (o RunActionOutcome) final() bool { return o.Status != "" && o.Status != OutcomePending }

// Passed reports whether the action ended OK for on: success_of_previous:
// done, or a dry run of it.
func (o RunActionOutcome) Passed() bool { return passed(o.Status) }

// failedRun reports whether the outcome fails the run. An unknown outcome
// of the owner message does not: the message may well have been sent and
// is never sent again.
func (o RunActionOutcome) failedRun() bool {
	switch o.Status {
	case OutcomeError:
		return true
	case OutcomeUnknown:
		return o.Type != ActionTelegramMessage
	}
	return false
}

func passed(status string) bool {
	return status == OutcomeOK || status == OutcomeDryRun || status == OutcomePolicyDryRun
}

// RunActionSaver stores a run's run-level outcomes (RunActionsResult JSON)
// under the run's claim.
type RunActionSaver func(ctx context.Context, result json.RawMessage) error

// RunActionsEnd is how a run's run-level actions ended. Neither flag set
// means every action ran or was skipped by design.
type RunActionsEnd struct {
	// Retry: an action failed transiently and had no effect; the run is
	// tried again and resumes from the stored outcomes.
	Retry bool
	// Failed: an action ended in a definitive failure.
	Failed bool
	// Code is the run's error code when Retry or Failed is set.
	Code string
}

// RunActionRunner executes a run's run-level actions (ResolvedDoc.
// RunActions). It may be called again for the same run (a retry): actions
// whose outcome is already stored in rc.Run.RunActions are not run again.
// An error is a storage failure or a cancellation.
type RunActionRunner interface {
	RunActions(ctx context.Context, rc RunContext, save RunActionSaver) (RunActionsEnd, error)
}

var _ RunActionRunner = (*Engine)(nil)

var (
	toolOutcomeRe = regexp.MustCompile(`(?:^|\s)outcome=([a-z_]{1,40})(?:\s|$)`)
	reviewIDRe    = regexp.MustCompile(`(?:^|\s)review_id=([0-9]{1,19})(?:\s|$)`)
	chatTeamIDRe  = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// runActionMaxWrites bounds the writes of a run's run-level actions across
// all its attempts: every pull request and the reaction, on every attempt.
const runActionMaxWrites = (MaxPRRefs + 1) * (MaxRunAttempts + 1)

// runActionScope namespaces the executor's idempotency keys per run,
// run-level action and target.
func runActionScope(runID int64, index, target int) string {
	return fmt.Sprintf("behavior_run_action:%d:%d:%d", runID, index, target)
}

// runActionEnv is what the run-level writes of a run use.
type runActionEnv struct {
	cfg   tenant.AgentConfig
	final bool // the run's last attempt: a transient failure is final
}

// RunActions executes the run's run-level actions in declared order. Each
// write action's outcome is stored as soon as it ends; a transient failure
// stores what is done and asks for a retry. telegram.message, always last,
// reports every outcome to the owner once per run.
func (e *Engine) RunActions(ctx context.Context, rc RunContext, save RunActionSaver) (RunActionsEnd, error) {
	run := rc.Run
	res := DecodeRunActions(run.RunActions)
	env := runActionEnv{final: rc.finalAttempt()}
	if !rc.Doc.DryRun && hasRunWrite(rc.Doc.RunActions) {
		cfg, err := e.d.Policies.RunPolicy(ctx, run.WorkspaceID, rc.Trigger.IntegrationID)
		if err != nil {
			log.Printf("behavior run %d corr=%s: run actions: policy unavailable", run.ID, run.CorrelationID)
			return RunActionsEnd{Failed: true, Code: CodePolicyUnavailable}, nil
		}
		env.cfg = cfg
	}
	failed := false
	prevPassed := true
	for i, a := range rc.Doc.RunActions {
		if err := ctx.Err(); err != nil {
			return RunActionsEnd{}, err
		}
		if o, ok := res.Get(i); ok && o.final() {
			prevPassed, failed = o.Passed(), failed || o.failedRun()
			continue
		}
		// A durable step re-checks its workflow claim before each action.
		if err := authorizeAction(ctx); err != nil {
			return RunActionsEnd{}, err
		}
		var o RunActionOutcome
		transient := false
		switch {
		case a.When != nil && !whenPasses(a.When, rc.Doc.Verdict, res.Verdict):
			// The verdict only narrows: a condition that is not met, or a
			// run without a verdict, skips the action.
			o = RunActionOutcome{Index: i, Type: a.Type, Status: OutcomeSkippedVerdict}
			_, o.Code = gate(a.When, rc.Doc.Verdict, res.Verdict)
			if res.Verdict.Available() {
				c := res.Verdict.Confidence
				o.VerdictLabel, o.VerdictConfidence = res.Verdict.Label, &c
			}
		case a.On == OnSuccessOfPrevious && !prevPassed:
			o = RunActionOutcome{Index: i, Type: a.Type, Status: OutcomeSkippedPrevious}
		case a.Type == ActionPRApprove:
			prev, _ := res.Get(i)
			var err error
			o, transient, err = e.runPRApprove(ctx, rc, env, i, a, prev)
			if err != nil {
				return RunActionsEnd{}, err
			}
		case a.Type == ActionChatReaction:
			o, transient = e.runChatReaction(ctx, rc, env, i, a)
		case a.Type == ActionTelegramMessage:
			var err error
			o, transient, err = e.runTelegram(ctx, rc, env, i, res)
			if err != nil {
				return RunActionsEnd{}, err
			}
		default:
			o = RunActionOutcome{Index: i, Type: a.Type, Status: OutcomeError, Code: codeUnknownType}
		}
		if o.Status != "" {
			res.set(o)
			if err := save(ctx, res.encode()); err != nil {
				return RunActionsEnd{}, err
			}
		}
		if transient {
			log.Printf("behavior run %d corr=%s: run action %d %s: transient failure", run.ID, run.CorrelationID, i, a.Type)
			return RunActionsEnd{Retry: true, Code: CodeRunActionTransient}, nil
		}
		if o.Code != "" {
			log.Printf("behavior run %d corr=%s: run action %d %s: %s (%s)", run.ID, run.CorrelationID, i, a.Type, o.Status, o.Code)
		} else {
			log.Printf("behavior run %d corr=%s: run action %d %s: %s", run.ID, run.CorrelationID, i, a.Type, o.Status)
		}
		prevPassed, failed = o.Passed(), failed || o.failedRun()
	}
	if failed {
		return RunActionsEnd{Failed: true, Code: CodeRunActionFailed}, nil
	}
	return RunActionsEnd{}, nil
}

func whenPasses(w *ActionWhen, spec *VerdictSpec, v *RunVerdict) bool {
	ok, _ := gate(w, spec, v)
	return ok
}

func hasRunWrite(actions []ActionSpec) bool {
	for _, a := range actions {
		if a.Type == ActionPRApprove || a.Type == ActionChatReaction {
			return true
		}
	}
	return false
}

// callRunWrite runs one run-level write tool through the action executor:
// only that tool enabled, the workspace's dry-run policy, the source's
// GitHub accounts (for an account-scoped tool) and ClickUp credential.
func (e *Engine) callRunWrite(ctx context.Context, rc RunContext, env runActionEnv, scope, tool string, input any) tools.CallResult {
	raw, err := json.Marshal(input)
	if err != nil {
		return tools.CallResult{Content: "error: invalid_input", IsError: true, Status: tools.StatusError}
	}
	policy := tenant.ToolPolicyConfig{Enabled: []string{tool}, DryRun: env.cfg.Tools.DryRun, MaxWritesPerJob: runActionMaxWrites}
	if tool == ToolPRApprove {
		policy.Accounts = env.cfg.Tools.Accounts
	}
	return e.d.Callers(rc.Run, scope, policy, env.cfg.ClickUpTokenRef).Call(ctx, 1, tool, raw)
}

// writeStatus maps a write tool result to a run-level status and code.
// transient is a failure that had no effect and may be tried again; on the
// run's last attempt it is final (an error).
func writeStatus(res tools.CallResult, env runActionEnv) (status, code string, transient bool) {
	o, tr := outcomeOf(res, 0, "")
	status, code = o.Status, o.Code
	if status == OutcomeConflict {
		status = OutcomeError
	}
	if tr && env.final {
		tr = false
	}
	return status, code, tr
}

// runPRApprove approves each pull request of the trigger data, resuming
// after the targets prev already recorded as final. The action is OK when
// every pull request is approved or already approved.
func (e *Engine) runPRApprove(ctx context.Context, rc RunContext, env runActionEnv, i int, a ActionSpec, prev RunActionOutcome) (RunActionOutcome, bool, error) {
	o := RunActionOutcome{Index: i, Type: a.Type, Status: OutcomePending}
	prs := rc.Trigger.PRs
	if len(prs) == 0 {
		o.Status, o.Code = OutcomeError, CodeNoPR
		return o, false, nil
	}
	if len(prs) > MaxPRRefs {
		prs = prs[:MaxPRRefs]
	}
	account := a.Params["account"]
	for k, pr := range prs {
		if k < len(prev.PRs) && prev.PRs[k].Repo == pr.Repo && prev.PRs[k].Number == pr.Number {
			o.PRs = append(o.PRs, prev.PRs[k])
			continue
		}
		po := RunPROutcome{Repo: pr.Repo, Number: pr.Number}
		switch {
		case !tenant.ValidRepo(pr.Repo) || pr.Number < 1 || pr.Number > maxPRNumber:
			po.Status, po.Code = OutcomeError, CodeBadTarget
		case rc.Doc.DryRun:
			po.Status = OutcomeDryRun
		default:
			if err := authorizeAction(ctx); err != nil {
				return o, false, err
			}
			res := e.callRunWrite(ctx, rc, env, runActionScope(rc.Run.ID, i, k), ToolPRApprove,
				map[string]any{"account": account, "repo": pr.Repo, "number": pr.Number})
			status, code, transient := writeStatus(res, env)
			if transient {
				return o, true, nil
			}
			po.Status, po.Code = status, code
			if status == OutcomeOK {
				if m := toolOutcomeRe.FindStringSubmatch(res.Content); m != nil {
					po.Result = m[1]
				}
				if m := reviewIDRe.FindStringSubmatch(res.Content); m != nil {
					po.ReviewID, _ = strconv.ParseInt(m[1], 10, 64)
				}
			}
		}
		o.PRs = append(o.PRs, po)
	}
	// Every target is final: the first one that did not pass decides.
	o.Status = OutcomeOK
	dry, policyDry := true, false
	for _, po := range o.PRs {
		if !passed(po.Status) {
			o.Status, o.Code = po.Status, po.Code
			if o.Status == OutcomeUnknown && o.Code == "" {
				o.Code = OutcomeUnknown
			}
			return o, false, nil
		}
		dry = dry && po.Status == OutcomeDryRun
		policyDry = policyDry || po.Status == OutcomePolicyDryRun
	}
	switch {
	case dry:
		o.Status = OutcomeDryRun
	case policyDry:
		o.Status = OutcomePolicyDryRun
	}
	return o, false, nil
}

// runChatReaction adds the action's reaction to the triggering message in
// the source integration's ClickUp workspace.
func (e *Engine) runChatReaction(ctx context.Context, rc RunContext, env runActionEnv, i int, a ActionSpec) (RunActionOutcome, bool) {
	t := rc.Trigger
	o := RunActionOutcome{Index: i, Type: a.Type, TeamID: t.TeamID, MessageID: t.MessageID, Reaction: a.Params["reaction"]}
	switch {
	case !chatIDRe.MatchString(t.MessageID) || rc.Run.Subject != ChatSubject(t.MessageID):
		o.Status, o.Code = OutcomeError, CodeNoMessage
		return o, false
	case !chatTeamIDRe.MatchString(t.TeamID):
		o.Status, o.Code = OutcomeError, CodeNoTeam
		return o, false
	case rc.Doc.DryRun:
		o.Status = OutcomeDryRun
		return o, false
	}
	res := e.callRunWrite(ctx, rc, env, runActionScope(rc.Run.ID, i, 0), ToolChatReaction,
		map[string]any{"team_id": t.TeamID, "message_id": t.MessageID, "reaction": o.Reaction})
	status, code, transient := writeStatus(res, env)
	if transient {
		return RunActionOutcome{}, true
	}
	o.Status, o.Code = status, code
	if status == OutcomeOK {
		if m := toolOutcomeRe.FindStringSubmatch(res.Content); m != nil {
			o.Result = m[1]
		}
	}
	return o, false
}

// runTelegram sends the owner one message reporting every run-level
// outcome, at most once per run (the message log claims its key first).
// A failed send is transient until the run's last attempt.
func (e *Engine) runTelegram(ctx context.Context, rc RunContext, env runActionEnv, i int, res RunActionsResult) (RunActionOutcome, bool, error) {
	o := RunActionOutcome{Index: i, Type: ActionTelegramMessage}
	chat, err := e.d.Chats.OwnerChat(ctx, rc.Run.WorkspaceID, rc.Trigger)
	if err != nil {
		if env.final {
			o.Status, o.Code = OutcomeError, CodeSendFailed
			return o, false, nil
		}
		return RunActionOutcome{}, true, nil
	}
	text, err := e.runActionsReport(ctx, rc, res)
	if err != nil {
		return RunActionOutcome{}, true, nil
	}
	// A refused workflow claim ends the attempt without an outcome: the
	// send was not tried.
	if err := authorizeAction(ctx); err != nil {
		return RunActionOutcome{}, false, err
	}
	id, err := e.sendOnce(ctx, rc.Run, fmt.Sprintf("run_action:%d", i), chat, e.withAliasButton(ctx, rc, OutMessage{Text: text}))
	switch {
	case err != nil && id == 0:
		if env.final {
			o.Status, o.Code = OutcomeError, CodeSendFailed
			return o, false, nil
		}
		return RunActionOutcome{}, true, nil
	case id == 0:
		// An earlier send's outcome is unknown; it is never sent again.
		o.Status, o.Code = OutcomeUnknown, OutcomeUnknown
	default:
		// Sent (err set only when its ID could not be stored).
		o.Status, o.TelegramMessageID = OutcomeOK, id
	}
	return o, false, nil
}

// runActionsReport converts stored identifiers and safe outcome codes into
// the shared renderer input. Triggering message text is neither available nor
// copied into the report.
func (e *Engine) runActionsReport(ctx context.Context, rc RunContext, res RunActionsResult) (string, error) {
	kind := reporting.KindNotify
	values := reporting.Values{Behavior: rc.Doc.Name, RunID: fmt.Sprint(rc.Run.ID), Channel: rc.Trigger.ChannelName}
	var reaction string
	var prLines []string
	for i, a := range rc.Doc.RunActions {
		o, ok := res.Get(i)
		if !ok || a.Type == ActionTelegramMessage {
			continue
		}
		switch a.Type {
		case ActionPRApprove:
			if o.Status == OutcomeSkippedVerdict || o.Status == OutcomeSkippedPrevious {
				kind = reporting.KindPRSkipped
				values.Reason = runActionReason(textsFor(rc.Doc.Language), a, o, res.Verdict)
			}
			if len(o.PRs) == 0 && o.Code != "" {
				kind = reporting.KindFailure
				values.Reason = textsFor(rc.Doc.Language).outcomeWord(o.Status, o.Code)
			}
			for _, po := range o.PRs {
				if values.PRNumber == "" {
					values.PRNumber = strconv.Itoa(po.Number)
					values.PRRepo = po.Repo
					values.Link = reporting.PRLink(po.Repo, po.Number)
				}
				prLines = append(prLines, "• "+textsFor(rc.Doc.Language).prLine(PRRef{Repo: po.Repo, Number: po.Number}.String(), textsFor(rc.Doc.Language).prWord(po)))
				if passed(po.Status) && kind != reporting.KindPRSkipped && kind != reporting.KindFailure {
					kind = reporting.KindPRApprove
				} else if !passed(po.Status) {
					kind = reporting.KindFailure
					values.Reason = textsFor(rc.Doc.Language).outcomeWord(po.Status, po.Code)
				}
			}
		case ActionChatReaction:
			reaction = textsFor(rc.Doc.Language).reactionLine(o)
		}
	}
	values.PRs = strings.Join(prLines, "\n")
	if v := res.Verdict; v != nil {
		if v.Available() {
			values.Verdict = v.Label
			values.Confidence = reporting.Confidence(v.Confidence)
			values.AISummary = capRunes(strings.Join(strings.Fields(e.d.Redactor.CleanOutbound(v.Summary, true)), " "), MaxVerdictSummaryChars)
		} else {
			if kind != reporting.KindFailure && kind != reporting.KindPRSkipped {
				kind = reporting.KindVerdictUnavailable
			}
			values.Reason = textsFor(rc.Doc.Language).verdictUnavailableLine(verdictCode(v))
		}
	}
	return e.d.Reports.Render(ctx, reporting.Report{
		Kind: kind, WorkspaceID: rc.Run.WorkspaceID,
		Behavior: rc.Run.BehaviorName, Language: rc.Doc.Language, Platform: "clickup", ExternalID: rc.Trigger.SenderID,
		Values: values, Reaction: reaction, TraceHint: fmt.Sprintf("/trace %d", rc.Run.ID),
	})
}

func runActionReason(t texts, a ActionSpec, o RunActionOutcome, verdict *RunVerdict) string {
	if o.Code == CodeVerdictUnavailable || o.VerdictConfidence == nil {
		return t.gateUnavailableLine(a.Type, verdictCode(verdict))
	}
	want := ""
	if a.When != nil {
		want = a.When.Verdict
	}
	return t.gateSkippedLine(a.Type, o.VerdictLabel, *o.VerdictConfidence, want)
}

// runActionsText is the owner's report of a run's run-level actions: the
// run, the message sender (a ClickUp user ID), the model verdict (label,
// confidence and the model's summary, passed through clean), each pull
// request with its outcome and the reaction. It never quotes the message
// text.
func runActionsText(t texts, rc RunContext, res RunActionsResult, clean func(string) string) string {
	var b strings.Builder
	head := t.runActionsHead()
	if rc.Doc.DryRun {
		head = t.runActionsDryHead()
	}
	b.WriteString(head + " (" + t.runRef(rc.Doc.Name, rc.Run.ID) + ")\n")
	if rc.Trigger.SenderID != "" {
		b.WriteString(t.chatFrom(capRunes(rc.Trigger.SenderID, 64)) + "\n")
	}
	if v := res.Verdict; v != nil {
		if v.Available() {
			summary := capRunes(strings.Join(strings.Fields(clean(v.Summary)), " "), MaxVerdictSummaryChars)
			b.WriteString(t.verdictLine(v.Label, v.Confidence, summary) + "\n")
		} else {
			b.WriteString(t.verdictUnavailableLine(verdictCode(v)) + "\n")
		}
	}
	for i, a := range rc.Doc.RunActions {
		if a.Type == ActionTelegramMessage {
			continue
		}
		o, ok := res.Get(i)
		if !ok {
			continue
		}
		if o.Status == OutcomeSkippedVerdict {
			want := ""
			if a.When != nil {
				want = a.When.Verdict
			}
			if o.Code == CodeVerdictUnavailable || o.VerdictConfidence == nil {
				b.WriteString(t.gateUnavailableLine(a.Type, verdictCode(res.Verdict)) + "\n")
			} else {
				b.WriteString(t.gateSkippedLine(a.Type, o.VerdictLabel, *o.VerdictConfidence, want) + "\n")
			}
			continue
		}
		switch a.Type {
		case ActionPRApprove:
			if o.Status == OutcomeSkippedPrevious || len(o.PRs) == 0 {
				b.WriteString(t.prActionLine(t.outcomeWord(o.Status, o.Code)) + "\n")
				continue
			}
			for _, po := range o.PRs {
				ref := PRRef{Repo: po.Repo, Number: po.Number}.String()
				b.WriteString("• " + t.prLine(ref, t.prWord(po)) + "\n")
			}
		case ActionChatReaction:
			b.WriteString(t.reactionLine(o) + "\n")
		}
	}
	return capRunes(strings.TrimSpace(b.String()), maxTelegramRunes)
}

// verdictCode is the code a report names for a run without a verdict.
func verdictCode(v *RunVerdict) string {
	if v != nil && codeRe.MatchString(v.Code) {
		return v.Code
	}
	return CodeVerdictUnavailable
}

// --- undo ---

// runUndoScope namespaces the idempotency keys of a run-level undo per run
// and run-level action.
func runUndoScope(runID int64, index int) string {
	return fmt.Sprintf("behavior_run_undo:%d:%d", runID, index)
}

// undoRunActions undoes a run's run-level actions: it removes each
// reaction the run itself added (result reacted, not already_reacted) and
// dismisses nothing on GitHub. The reply lists every approval the run made
// so the owner can dismiss it by hand. The run is marked undone only when
// a reaction was removed; a run with nothing reversible stays as it is.
func (e *Engine) undoRunActions(ctx context.Context, rc RunContext, r UndoRequest, t texts) (string, error) {
	res := DecodeRunActions(rc.Run.RunActions)
	var reactions []RunActionOutcome
	var links []string
	for _, o := range res.Actions {
		switch {
		case o.Type == ActionChatReaction && o.Status == OutcomeOK && o.Result == resultReacted:
			reactions = append(reactions, o)
		case o.Type == ActionPRApprove:
			for _, p := range o.PRs {
				if p.Status == OutcomeOK && p.Result == resultApproved && tenant.ValidRepo(p.Repo) && p.Number > 0 {
					links = append(links, fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, p.Number))
				}
			}
		}
	}
	byHand := ""
	if len(links) > 0 {
		byHand = "\n" + t.undoApprovalsByHand(links)
	}
	if len(reactions) == 0 {
		return t.undoRunNothing() + byHand, nil
	}
	cfg, err := e.d.Policies.RunPolicy(ctx, rc.Run.WorkspaceID, rc.Trigger.IntegrationID)
	if err != nil {
		return t.undoFailed(CodePolicyUnavailable) + byHand, nil
	}
	env := runActionEnv{cfg: cfg, final: true}
	for _, o := range reactions {
		// The target is the stored outcome's, checked against the run.
		if !chatIDRe.MatchString(o.MessageID) || rc.Run.Subject != ChatSubject(o.MessageID) ||
			!chatTeamIDRe.MatchString(o.TeamID) || o.Reaction == "" {
			return t.undoFailed(CodeBadTarget) + byHand, nil
		}
		out := e.callRunWrite(ctx, rc, env, runUndoScope(r.RunID, o.Index), ToolChatReactionRemove,
			map[string]any{"team_id": o.TeamID, "message_id": o.MessageID, "reaction": o.Reaction})
		status, code, _ := writeStatus(out, env)
		if status != OutcomeOK {
			return t.undoFailed(firstNonEmpty(code, status)) + byHand, nil
		}
	}
	if err := e.d.Store.MarkUndone(ctx, r.WorkspaceID, r.RunID); err != nil && !errors.Is(err, ErrConflict) {
		return "", err
	}
	log.Printf("behavior run %d corr=%s req=%s: undone by owner (%d reactions)", rc.Run.ID, rc.Run.CorrelationID, r.CorrelationID, len(reactions))
	return t.undone(r.RunID) + byHand, nil
}
