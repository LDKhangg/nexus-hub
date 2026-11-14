package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

var assistantRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}$`)

// The assistant's hub tools. Read tools run at once through the same
// stores as the slash commands and the workflow CLI, and return bounded,
// sanitized text: IDs, names, states, reason codes and counters, never a
// stored payload, a secret or a credential. Change tools never act: they
// send a "Làm"/"Huỷ" confirmation (an assistant_action approval), and only
// the owner's tap runs the change (assistantCallback).

// Tool names offered to the model.
const (
	toolContactsSearch  = "contacts_search"
	toolRunsRecent      = "runs_recent"
	toolRunTrace        = "run_trace"
	toolReportPrefs     = "report_prefs"
	toolReportTemplate  = "report_template"
	toolWorkflowList    = "workflow_list"
	toolWorkflowShow    = "workflow_show"
	toolWorkflowHistory = "workflow_history"
	toolWorkflowJobs    = "workflow_jobs"

	toolContactAlias   = "contact_alias"
	toolReportStyle    = "report_style"
	toolWorkflowCancel = "workflow_cancel"
	toolWorkflowResume = "workflow_resume"
	toolWorkflowRetry  = "workflow_retry"
)

// assistant_action approval actions of the contact and report changes
// (the names Phase 4 stored; the workflow actions use their tool names).
const (
	intentAlias      = "alias"
	intentPrefsStyle = "prefs_style"
)

const (
	// assistantToolResultRunes bounds one tool result.
	assistantToolResultRunes = 6000
	assistantContactsMax     = 15
	assistantWorkflowListMax = 20
	assistantHistoryMax      = 50
	assistantStepsShown      = 30
)

const (
	toolMsgNotAvailable = "This tool is not available on this hub."
	toolMsgNotFound     = "Not found in this workspace."
	toolMsgFailed       = "The hub could not read this now; try again later."
)

type assistantTool struct {
	def      domain.ToolDef
	workflow bool // needs the workflow reader
}

func toolSchema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if len(required) == 0 {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `,"additionalProperties":false}`)
}

var assistantTools = []assistantTool{
	{def: domain.ToolDef{Name: toolContactsSearch, Description: "Search the hub's contact directory (people seen on ClickUp, GitHub, Telegram). Returns name and contact ID lines.",
		InputSchema: toolSchema(`"search":{"type":"string","description":"text to search; empty lists the first contacts"}`)}},
	{def: domain.ToolDef{Name: toolRunsRecent, Description: "List recent behavior runs: run ID, behavior, status, error code, time.",
		InputSchema: toolSchema(`"behavior":{"type":"string","description":"behavior name, optional"},"limit":{"type":"integer","minimum":1,"maximum":10}`)}},
	{def: domain.ToolDef{Name: toolRunTrace, Description: "Show the trace of one behavior run: what it read, decided and did, and its linked workflow executions.",
		InputSchema: toolSchema(`"run":{"type":"string","description":"run ID, task ID or task URL; empty for the latest run"},"item":{"type":"string","description":"item key, optional"}`)}},
	{def: domain.ToolDef{Name: toolReportPrefs, Description: "Show the owner's report preferences (style and shown parts).",
		InputSchema: toolSchema(``)}},
	{def: domain.ToolDef{Name: toolReportTemplate, Description: "Show the report template of one kind (the owner's or the default) and a preview.",
		InputSchema: toolSchema(`"kind":{"type":"string","enum":["pr_approve","pr_skipped","verdict_unavailable","mention","notify","fyi","digest","failure"]}`, "kind")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowList, Description: "List durable workflow executions, newest first: ID, state, reason code, behavior and version, times.",
		InputSchema: toolSchema(`"state":{"type":"string","enum":["RECEIVED","TRIAGING","PLANNING","QUEUED","RUNNING","EVALUATING","WAITING_FOR_USER","WAITING_FOR_APPROVAL","COMPLETED","FAILED","CANCELLED"]},"limit":{"type":"integer","minimum":1,"maximum":20},"before_id":{"type":"integer","minimum":1,"description":"only executions with a smaller ID (next page)"}`)}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowShow, Description: "Show one workflow execution: state, reason, version, retry links, live job, waits and steps.",
		InputSchema: toolSchema(`"id":{"type":"integer","minimum":1}`, "id")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowHistory, Description: "Show the event history (timeline) of one workflow execution.",
		InputSchema: toolSchema(`"id":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":50},"after_sequence":{"type":"integer","minimum":0}`, "id")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowJobs, Description: "List rows of the durable job queue, newest first; filter by status (dead_letter shows failed work) or execution.",
		InputSchema: toolSchema(`"status":{"type":"string","enum":["pending","processing","completed","dead_letter"]},"execution_id":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":20}`)}},

	{def: domain.ToolDef{Name: toolContactAlias, Description: "Propose giving a contact a display name. Sends the owner a Làm/Huỷ confirmation; nothing changes unless the owner taps Làm.",
		InputSchema: toolSchema(`"ref":{"type":"string","description":"contact ID or platform:external-id, e.g. clickup:12345"},"name":{"type":"string"}`, "ref", "name")}},
	{def: domain.ToolDef{Name: toolReportStyle, Description: "Propose changing the report style. Sends the owner a Làm/Huỷ confirmation; nothing changes unless the owner taps Làm.",
		InputSchema: toolSchema(`"style":{"type":"string","enum":["short","normal","detailed"]}`, "style")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowCancel, Description: "Propose cancelling a running or waiting workflow execution. Sends a Làm/Huỷ confirmation; nothing changes unless the owner taps Làm.",
		InputSchema: toolSchema(`"id":{"type":"integer","minimum":1}`, "id")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowResume, Description: "Propose resuming a waiting workflow execution whose approval decision is already recorded. Sends a Làm/Huỷ confirmation.",
		InputSchema: toolSchema(`"id":{"type":"integer","minimum":1}`, "id")}},
	{workflow: true, def: domain.ToolDef{Name: toolWorkflowRetry, Description: "Propose retrying a FAILED workflow execution as a new linked execution (never resends a known side effect). Sends a Làm/Huỷ confirmation.",
		InputSchema: toolSchema(`"id":{"type":"integer","minimum":1}`, "id")}},
}

// assistantToolDefs lists the tools this hub can run.
func (w *InboxWorker) assistantToolDefs() []domain.ToolDef {
	hasWorkflows := w.d.Assistant != nil && w.d.Assistant.Workflows != nil
	out := make([]domain.ToolDef, 0, len(assistantTools))
	for _, t := range assistantTools {
		if t.workflow && !hasWorkflows {
			continue
		}
		out = append(out, t.def)
	}
	return out
}

// assistantToolTurn is the state of one turn's tool calls.
type assistantToolTurn struct {
	u         InboundUpdate
	to        ChatRef
	actor     string
	corr      string
	proposals int
	proposed  map[string]bool // action|resource already proposed this turn
}

// toolArgs is a tool call's validated input object.
type toolArgs map[string]any

var errToolArgs = errors.New("invalid tool input")

func parseToolArgs(raw json.RawMessage) (toolArgs, error) {
	args := toolArgs{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return nil, errToolArgs
	}
	return args, nil
}

// str returns a trimmed string argument of at most assistantMaxArgRunes.
func (a toolArgs) str(key string) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = strings.TrimSpace(x)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return "", errToolArgs
	}
	if utf8.RuneCountInString(s) > assistantMaxArgRunes {
		return "", errToolArgs
	}
	return s, nil
}

// num returns an integer argument (a JSON number or a numeric string), def
// when absent, or an error when outside lo..hi.
func (a toolArgs) num(key string, def, lo, hi int64) (int64, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return def, nil
	}
	var n int64
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return 0, errToolArgs
		}
		n = int64(x)
	case string:
		p, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(x), "#")), 10, 64)
		if err != nil {
			return 0, errToolArgs
		}
		n = p
	default:
		return 0, errToolArgs
	}
	if n < lo || n > hi {
		return 0, errToolArgs
	}
	return n, nil
}

// runAssistantTool runs one tool call and returns its result text and
// whether it is an error. It never returns a provider or storage error
// text.
func (w *InboxWorker) runAssistantTool(ctx context.Context, tt *assistantToolTurn, c domain.ToolCall) (string, bool) {
	args, err := parseToolArgs(c.Input)
	if err != nil {
		return "Invalid input: it must be a JSON object matching the tool's schema.", true
	}
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	defer cancel()
	ws := tt.u.WorkspaceID
	var (
		text  string
		isErr bool
	)
	switch c.Name {
	case toolContactsSearch:
		text, isErr = w.toolContacts(hctx, ws, args)
	case toolRunsRecent:
		text, isErr = w.toolRuns(hctx, ws, args)
	case toolRunTrace:
		text, isErr = w.toolTrace(hctx, tt, args)
	case toolReportPrefs:
		text, isErr = w.toolPrefs(hctx, ws)
	case toolReportTemplate:
		text, isErr = w.toolTemplate(hctx, ws, args)
	case toolWorkflowList:
		text, isErr = w.toolWorkflowList(hctx, ws, args)
	case toolWorkflowShow:
		text, isErr = w.toolWorkflowShow(hctx, ws, args)
	case toolWorkflowHistory:
		text, isErr = w.toolWorkflowHistory(hctx, ws, args)
	case toolWorkflowJobs:
		text, isErr = w.toolWorkflowJobs(hctx, ws, args)
	case toolContactAlias, toolReportStyle, toolWorkflowCancel, toolWorkflowResume, toolWorkflowRetry:
		// The confirmation is sent with the turn's context, not the
		// handler timeout, like a reply.
		text, isErr = w.toolPropose(ctx, tt, c.Name, args)
	default:
		return "Unknown tool.", true
	}
	return text, isErr
}

// toolErr maps a store error to a fixed tool result.
func toolErr(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrNotAvailable):
		return toolMsgNotAvailable, true
	case errors.Is(err, ErrNotFound), errors.Is(err, workflow.ErrNotFound):
		return toolMsgNotFound, true
	case errors.Is(err, errToolArgs), errors.Is(err, workflow.ErrInvalidInput):
		return "Invalid input for this tool.", true
	}
	return toolMsgFailed, true
}

func toolJSON(v any) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return toolMsgFailed, true
	}
	return string(b), false
}

func (w *InboxWorker) toolContacts(ctx context.Context, ws string, args toolArgs) (string, bool) {
	if w.d.People == nil {
		return toolMsgNotAvailable, true
	}
	search, err := args.str("search")
	if err != nil {
		return toolErr(err)
	}
	contacts, err := w.d.People.List(ctx, ws, "", search, assistantContactsMax)
	if err != nil {
		return toolErr(err)
	}
	if len(contacts) == 0 {
		return "No contact matches.", false
	}
	lines := make([]string, 0, len(contacts))
	for _, c := range contacts {
		lines = append(lines, oneLine(c.Name, 80)+" · "+c.ID)
	}
	return strings.Join(lines, "\n"), false
}

func (w *InboxWorker) toolRuns(ctx context.Context, ws string, args toolArgs) (string, bool) {
	if w.d.Assistant.Runs == nil {
		return toolMsgNotAvailable, true
	}
	name, err := args.str("behavior")
	if err != nil {
		return toolErr(err)
	}
	if name != "" && !NameRe.MatchString(name) {
		return "Invalid behavior name.", true
	}
	limit, err := args.num("limit", 5, 1, assistantMaxRunsShown)
	if err != nil {
		return toolErr(err)
	}
	list, err := w.d.Assistant.Runs.ListRecentRuns(ctx, ws, name, int(limit))
	if err != nil {
		return toolErr(err)
	}
	if len(list) == 0 {
		return "No runs yet.", false
	}
	lines := make([]string, 0, len(list))
	for _, r := range list {
		line := fmt.Sprintf("#%d %s · %s", r.ID, r.Behavior, r.Status)
		if r.ErrorCode != "" {
			line += " (" + r.ErrorCode + ")"
		}
		line += " · " + r.CreatedAt.UTC().Format("2006-01-02 15:04") + " UTC"
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), false
}

func (w *InboxWorker) toolTrace(ctx context.Context, tt *assistantToolTurn, args toolArgs) (string, bool) {
	ref, err := args.str("run")
	if err != nil {
		return toolErr(err)
	}
	item, err := args.str("item")
	if err != nil {
		return toolErr(err)
	}
	if ref == "" {
		id, err := w.latestRun(ctx, tt.u.WorkspaceID)
		if err != nil {
			return toolErr(err)
		}
		ref = strconv.FormatInt(id, 10)
	}
	if strings.ContainsAny(ref, " \t\n") {
		return "Invalid run reference: no spaces allowed.", true
	}
	reply, err := w.d.Tracer.Trace(ctx, TraceRequest{WorkspaceID: tt.u.WorkspaceID, Ref: ref, Item: item, CorrelationID: tt.corr})
	if err != nil {
		var ue *UsageError
		if errors.As(err, &ue) && strings.TrimSpace(ue.Usage) != "" {
			return ue.Usage, true
		}
		return toolErr(err)
	}
	return reply, false
}

func (w *InboxWorker) toolPrefs(ctx context.Context, ws string) (string, bool) {
	owner, err := w.reportOwner(ctx, ws)
	if err != nil {
		return toolErr(err)
	}
	p, err := w.d.Reports.Prefs(ctx, owner, ws, "")
	if err != nil {
		return toolErr(err)
	}
	return prefsText(p), false
}

func (w *InboxWorker) toolTemplate(ctx context.Context, ws string, args toolArgs) (string, bool) {
	k, err := args.str("kind")
	if err != nil {
		return toolErr(err)
	}
	kind, err := reporting.ParseKind(k)
	if err != nil {
		return "Unknown template kind.", true
	}
	owner, err := w.reportOwner(ctx, ws)
	if err != nil {
		return toolErr(err)
	}
	body, ok, err := w.d.Reports.Template(ctx, owner, ws, "", kind)
	if err != nil {
		return toolErr(err)
	}
	preview, err := reporting.Preview(kind)
	if ok {
		preview, err = reporting.Preview(kind, body)
	}
	if err != nil {
		return toolMsgFailed, true
	}
	shown := "(default template)"
	if ok {
		shown = body
	}
	return "Template " + string(kind) + ":\n" + shown + "\n\nPreview:\n" + preview +
		"\n\n(The owner edits it with /template " + string(kind) + ".)", false
}

// --- workflow tools ---

func toolTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func toolExecution(v workflow.ExecutionView) map[string]any {
	m := map[string]any{
		"id": v.ID, "state": v.State, "behavior": oneLine(v.BehaviorName, 80), "behavior_version": v.BehaviorVersion,
		"source_kind": v.References.SourceKind, "created_at": toolTime(&v.CreatedAt), "updated_at": toolTime(&v.UpdatedAt),
	}
	if v.Reason != "" {
		m["reason"] = string(v.Reason)
	}
	if v.ParentExecutionID != 0 {
		m["parent_execution_id"] = v.ParentExecutionID
	}
	if v.References.LegacyRunID != 0 {
		m["run_id"] = v.References.LegacyRunID
	}
	if v.Deadline != nil {
		m["deadline_at"] = toolTime(v.Deadline)
	}
	return m
}

func toolJob(j workflow.JobView) map[string]any {
	m := map[string]any{
		"job_id": j.ID, "execution_id": j.ExecutionID, "step": j.StepKey, "status": j.Status,
		"attempts": j.Attempts, "max_attempts": j.MaxAttempts, "due_at": toolTime(&j.DueAt),
	}
	if j.LastErrorCode != "" {
		m["error_code"] = j.LastErrorCode
	}
	if j.DeadLetterReason != "" {
		m["dead_letter_reason"] = j.DeadLetterReason
	}
	return m
}

func (w *InboxWorker) toolWorkflowList(ctx context.Context, ws string, args toolArgs) (string, bool) {
	r := w.d.Assistant.Workflows
	if r == nil {
		return toolMsgNotAvailable, true
	}
	st, err := args.str("state")
	if err != nil {
		return toolErr(err)
	}
	state := workflow.State(st)
	if st != "" && !state.Valid() {
		return "Unknown execution state.", true
	}
	limit, err := args.num("limit", 10, 1, assistantWorkflowListMax)
	if err != nil {
		return toolErr(err)
	}
	before, err := args.num("before_id", 0, 0, math.MaxInt64)
	if err != nil {
		return toolErr(err)
	}
	list, next, err := r.ListExecutions(ctx, workflow.ExecutionQuery{WorkspaceID: ws, State: state, BeforeID: before, Limit: int(limit)})
	if err != nil {
		return toolErr(err)
	}
	items := make([]map[string]any, 0, len(list))
	for _, v := range list {
		items = append(items, toolExecution(v))
	}
	out := map[string]any{"executions": items}
	if next > 0 {
		out["next_before_id"] = next
	}
	return toolJSON(out)
}

func (w *InboxWorker) toolWorkflowShow(ctx context.Context, ws string, args toolArgs) (string, bool) {
	r := w.d.Assistant.Workflows
	if r == nil {
		return toolMsgNotAvailable, true
	}
	id, err := args.num("id", 0, 1, math.MaxInt64)
	if err != nil || id == 0 {
		return toolErr(errToolArgs)
	}
	d, err := r.GetExecutionDetail(ctx, ws, id)
	if err != nil {
		return toolErr(err)
	}
	m := toolExecution(d.ExecutionView)
	m["started_at"], m["finished_at"], m["cancelled_at"] = toolTime(d.StartedAt), toolTime(d.FinishedAt), toolTime(d.CancelledAt)
	if d.RetryChildID != 0 {
		m["retry_execution_id"] = d.RetryChildID
	}
	if d.LiveJob != nil {
		m["live_job"] = toolJob(*d.LiveJob)
	}
	waits := make([]map[string]any, 0, len(d.Waits))
	for _, wv := range d.Waits {
		waits = append(waits, map[string]any{
			"wait_id": wv.ID, "step": wv.StepKey, "approval_id": wv.ApprovalID, "kind": wv.Kind,
			"decision": wv.Decision, "expires_at": toolTime(wv.ExpiresAt), "closed": wv.ClosedAt != nil, "signalled": wv.Signalled,
		})
	}
	m["waits"] = waits
	if steps, err := r.ListSteps(ctx, ws, id); err == nil {
		items := make([]map[string]any, 0, min(len(steps), assistantStepsShown))
		for i, s := range steps {
			if i >= assistantStepsShown {
				break
			}
			items = append(items, map[string]any{"step": s.Key, "kind": s.Kind, "state": s.State, "attempts": s.Attempts,
				"tokens": s.Usage.InputTokens + s.Usage.OutputTokens})
		}
		m["steps"] = items
	}
	switch {
	case d.Reason == workflow.ReasonSideEffectUncertain:
		m["attention"] = "an action's outcome is unknown; check the provider; nothing is resent"
	case d.State.IsWaiting():
		m["attention"] = "waiting: deciding the open approval resumes it"
	}
	return toolJSON(m)
}

func (w *InboxWorker) toolWorkflowHistory(ctx context.Context, ws string, args toolArgs) (string, bool) {
	r := w.d.Assistant.Workflows
	if r == nil {
		return toolMsgNotAvailable, true
	}
	id, err := args.num("id", 0, 1, math.MaxInt64)
	if err != nil || id == 0 {
		return toolErr(errToolArgs)
	}
	limit, err := args.num("limit", 30, 1, assistantHistoryMax)
	if err != nil {
		return toolErr(err)
	}
	after, err := args.num("after_sequence", 0, 0, math.MaxInt64)
	if err != nil {
		return toolErr(err)
	}
	events, next, err := r.ListEvents(ctx, ws, id, after, int(limit))
	if err != nil {
		return toolErr(err)
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		m := map[string]any{"sequence": e.Sequence, "kind": e.Kind, "at": toolTime(&e.CreatedAt)}
		if e.FromState != "" {
			m["from"] = e.FromState
		}
		if e.ToState != "" {
			m["to"] = e.ToState
		}
		if e.StepKey != "" {
			m["step"] = e.StepKey
		}
		if e.Attempt != 0 {
			m["attempt"] = e.Attempt
		}
		if e.Reason != "" {
			m["reason"] = e.Reason
		}
		items = append(items, m)
	}
	out := map[string]any{"execution_id": id, "events": items}
	if next > 0 {
		out["next_after_sequence"] = next
	}
	return toolJSON(out)
}

func (w *InboxWorker) toolWorkflowJobs(ctx context.Context, ws string, args toolArgs) (string, bool) {
	r := w.d.Assistant.Workflows
	if r == nil {
		return toolMsgNotAvailable, true
	}
	status, err := args.str("status")
	if err != nil {
		return toolErr(err)
	}
	if status != "" && !workflow.ValidJobStatus(status) {
		return "Unknown job status.", true
	}
	execID, err := args.num("execution_id", 0, 0, math.MaxInt64)
	if err != nil {
		return toolErr(err)
	}
	limit, err := args.num("limit", 10, 1, assistantWorkflowListMax)
	if err != nil {
		return toolErr(err)
	}
	jobs, _, err := r.ListJobs(ctx, workflow.JobQuery{WorkspaceID: ws, Status: status, ExecutionID: execID, Limit: int(limit)})
	if err != nil {
		return toolErr(err)
	}
	items := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, toolJob(j))
	}
	return toolJSON(map[string]any{"jobs": items})
}

// --- change tools: confirmation only ---

const toolMsgProposed = "A confirmation with Làm/Huỷ buttons was sent to the owner. Nothing has changed: the change runs only if the owner taps Làm (within 10 minutes). Tell the owner to confirm with the buttons."

// toolPropose validates a change and sends its confirmation. It never
// runs the change.
func (w *InboxWorker) toolPropose(ctx context.Context, tt *assistantToolTurn, name string, args toolArgs) (string, bool) {
	if tt.proposals >= assistantMaxProposals {
		return "Too many changes proposed in one answer; ask the owner first.", true
	}
	var (
		action string
		p      assistantPayload
		text   string
	)
	switch name {
	case toolContactAlias:
		ref, err1 := args.str("ref")
		raw, err2 := args.str("name")
		nm := people.CleanDisplayName(raw)
		if err1 != nil || err2 != nil || !assistantRefRe.MatchString(ref) || nm == "" {
			return "Invalid input: ref must be a contact ID or platform:external-id without spaces, and name must not be empty.", true
		}
		action, p = intentAlias, assistantPayload{Ref: ref, Name: nm}
		text = "Đặt tên cho " + ref + " là: " + cleanAssistantReply(nm) + "?"
	case toolReportStyle:
		s, err := args.str("style")
		style := reporting.Style(s)
		if err != nil || (style != reporting.StyleShort && style != reporting.StyleNormal && style != reporting.StyleDetailed) {
			return "Invalid input: style must be short, normal or detailed.", true
		}
		action, p = intentPrefsStyle, assistantPayload{Style: string(style)}
		text = "Đổi kiểu báo cáo thành " + string(style) + "?"
	case toolWorkflowCancel, toolWorkflowResume, toolWorkflowRetry:
		a := w.d.Assistant
		if a.Workflows == nil || a.WorkflowCommands == nil {
			return toolMsgNotAvailable, true
		}
		id, err := args.num("id", 0, 1, math.MaxInt64)
		if err != nil || id == 0 {
			return toolErr(errToolArgs)
		}
		rctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
		d, err := a.Workflows.GetExecutionDetail(rctx, tt.u.WorkspaceID, id)
		cancel()
		if err != nil {
			return toolErr(err)
		}
		label := fmt.Sprintf("execution #%d (%s · %s)", id, cleanAssistantReply(oneLine(d.BehaviorName, 60)), d.State)
		switch name {
		case toolWorkflowCancel:
			if d.State.IsTerminal() {
				return fmt.Sprintf("Execution %d already ended (%s); it cannot be cancelled.", id, d.State), true
			}
			text = "Huỷ " + label + "? Yêu cầu bên ngoài đang chạy dở sẽ không được hoàn tác."
		case toolWorkflowResume:
			if !d.State.IsWaiting() {
				return fmt.Sprintf("Execution %d is not waiting (%s); there is nothing to resume.", id, d.State), true
			}
			for _, wait := range d.Waits {
				p.WaitID = max(p.WaitID, wait.ID)
			}
			if p.WaitID <= 0 {
				return fmt.Sprintf("Execution %d has no waiting generation to resume.", id), true
			}
			text = "Tiếp tục " + label + "?"
		case toolWorkflowRetry:
			if d.State != workflow.StateFailed {
				return fmt.Sprintf("Only a FAILED execution can be retried; execution %d is %s.", id, d.State), true
			}
			if d.RetryChildID != 0 {
				return fmt.Sprintf("Execution %d already has retry execution %d; look at that one instead.", id, d.RetryChildID), true
			}
			text = "Thử lại " + label + " bằng một execution mới? Hành động đã biết sẽ không bị gửi lại."
		}
		action = name
		p.ExecutionID = id
		switch name {
		case toolWorkflowCancel:
			p.IdempotencyKey = fmt.Sprintf("telegram:cancel:%s:%d", tt.u.WorkspaceID, id)
		case toolWorkflowResume:
			p.IdempotencyKey = fmt.Sprintf("telegram:resume:%s:%d:wait:%d", tt.u.WorkspaceID, id, p.WaitID)
		case toolWorkflowRetry:
			p.IdempotencyKey = fmt.Sprintf("telegram:retry:%s:%d", tt.u.WorkspaceID, id)
		}
	default:
		return "Unknown tool.", true
	}
	key := action + "|" + assistantResource(p) + "|" + p.Name
	if tt.proposed[key] {
		return "This change was already proposed in this answer.", true
	}
	out := w.proposeAssistantAction(ctx, tt.u, tt.to, action, p, text)
	if out.code != "assistant_"+action && out.code != "assistant_message_not_saved" {
		return "The confirmation could not be sent; nothing was changed.", true
	}
	tt.proposed[key] = true
	tt.proposals++
	return toolMsgProposed, false
}

// workflowCommand runs a confirmed workflow change through the engine and
// returns the owner's reply and an update code ("" when it took effect or
// had already taken effect). Idempotency keys follow plan §3.3 like the
// CLI's, with their own "telegram:" source.
func (w *InboxWorker) workflowCommand(ctx context.Context, ws, action string, p assistantPayload) (string, string) {
	a := w.d.Assistant
	if a == nil || a.Workflows == nil || a.WorkflowCommands == nil {
		return msgNotAvailable, "not_available"
	}
	id := p.ExecutionID
	if id <= 0 || p.IdempotencyKey == "" {
		return msgBadButton, NoteBadCallback
	}
	d, err := a.Workflows.GetExecutionDetail(ctx, ws, id)
	if errors.Is(err, workflow.ErrNotFound) {
		return msgNotFound, "not_found"
	}
	if err != nil {
		return msgFailed, "handler_failed"
	}
	cmd := workflow.Command{WorkspaceID: ws, ExecutionID: id, WaitID: p.WaitID, IdempotencyKey: p.IdempotencyKey}
	switch action {
	case toolWorkflowCancel:
		if d.State == workflow.StateCancelled {
			return fmt.Sprintf("Execution #%d đã bị huỷ trước đó.", id), ""
		}
		if d.State.IsTerminal() {
			return fmt.Sprintf("Execution #%d đã kết thúc (%s), không huỷ được.", id, d.State), "not_cancellable"
		}
		e, err := a.WorkflowCommands.Cancel(ctx, cmd)
		switch {
		case errors.Is(err, workflow.ErrNotEligible), errors.Is(err, workflow.ErrConflict):
			if now, rerr := a.Workflows.GetExecutionDetail(ctx, ws, id); rerr == nil && now.State == workflow.StateCancelled {
				return fmt.Sprintf("Execution #%d đã bị huỷ trước đó.", id), ""
			}
			return fmt.Sprintf("Execution #%d vừa đổi trạng thái, không huỷ được.", id), "not_cancellable"
		case err != nil:
			return msgFailed, "handler_failed"
		}
		return fmt.Sprintf("Đã huỷ execution #%d (%s). Yêu cầu bên ngoài đang chạy dở không được hoàn tác.", e.ID, e.State), ""
	case toolWorkflowResume:
		e, err := a.WorkflowCommands.Resume(ctx, cmd)
		switch {
		case errors.Is(err, workflow.ErrNotEligible), errors.Is(err, workflow.ErrConflict):
			now, rerr := a.Workflows.GetExecutionDetail(ctx, ws, id)
			if rerr != nil {
				now = d
			}
			switch {
			case now.State.IsTerminal():
				return fmt.Sprintf("Execution #%d đã kết thúc (%s).", id, now.State), "not_resumable"
			case !now.State.IsWaiting():
				return fmt.Sprintf("Execution #%d không ở trạng thái chờ (%s).", id, now.State), "not_resumable"
			default:
				return fmt.Sprintf("Execution #%d chưa có quyết định cho bước đang chờ; hãy quyết định yêu cầu duyệt trước.", id), "not_resumable"
			}
		case err != nil:
			return msgFailed, "handler_failed"
		}
		if !d.State.IsWaiting() {
			return fmt.Sprintf("Execution #%d đã được tiếp tục trước đó (%s).", e.ID, e.State), ""
		}
		return fmt.Sprintf("Đã tiếp tục execution #%d (%s).", e.ID, e.State), ""
	case toolWorkflowRetry:
		child, err := a.WorkflowCommands.Retry(ctx, cmd)
		switch {
		case errors.Is(err, workflow.ErrProviderUncertainty):
			return fmt.Sprintf("Không thử lại execution #%d: có hành động chưa rõ kết quả; không gửi lại gì cả, hãy kiểm tra phía nhà cung cấp.", id), "side_effect_uncertain"
		case errors.Is(err, workflow.ErrUnsafeRetry):
			return fmt.Sprintf("Không thử lại execution #%d: có bước đã xong mà không có checkpoint dùng lại được.", id), "unsafe_retry"
		case errors.Is(err, workflow.ErrNotEligible):
			return fmt.Sprintf("Chỉ thử lại được execution FAILED; execution #%d đang là %s.", id, d.State), "not_retryable"
		case err != nil:
			return msgFailed, "handler_failed"
		}
		if d.RetryChildID != 0 {
			return fmt.Sprintf("Execution #%d đã có lần thử lại #%d (%s).", id, child.ID, child.State), ""
		}
		return fmt.Sprintf("Đã tạo lần thử lại #%d cho execution #%d (%s).", child.ID, id, child.State), ""
	}
	return msgBadButton, NoteBadCallback
}

// cleanToolResult removes secrets (when a redactor is set) and control or
// bidi characters from a tool result and bounds its length.
func (a *Assistant) cleanToolResult(s string) string {
	if a.Redact != nil {
		s = a.Redact(s)
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || isAssistantBidi(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if utf8.RuneCountInString(out) > assistantToolResultRunes {
		out = capRunes(out, assistantToolResultRunes-20) + " [truncated]"
	}
	return out
}
