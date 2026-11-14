package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const workflowUsage = `usage: workflow <command> --workspace-id <uuid> [args]
  list [--state S] [--limit N] [--cursor C]    executions, newest first
  show <id>                                    state, reason, version, deadline,
                                               waits, live job, lineage, legacy links
  steps <id>                                   steps, attempts, usage, checkpoint refs
  history <id> [--limit N] [--cursor C]        ordered timeline
  jobs [--status S] [--execution-id ID] [--limit N] [--cursor C]
  dead-letter [--execution-id ID] [--limit N] [--cursor C]
  cancel <id>                                  cancel a running or waiting execution
  resume <id>                                  queue a wait whose decision is recorded
  retry <id>                                   retry a FAILED execution as a linked child
  cutover [--apply]                            read-only preflight; --apply cuts over
Lists take --limit 1-200 (default 50) and print next_cursor ("" at the end).
Output holds IDs, states, reason codes, counters and references only.`

// workflowRefRe is the shape of a checkpoint reference that may be printed.
var workflowRefRe = regexp.MustCompile(`^[A-Za-z0-9_.:#/-]{1,128}$`)

func workflowRef(s string) string {
	if workflowRefRe.MatchString(s) {
		return s
	}
	return "invalid"
}

func workflowTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func workflowTimeV(t time.Time) any { return workflowTime(&t) }

// workflowStoreErr maps a read/command error to a sanitized message.
func workflowStoreErr(err error) error {
	switch {
	case errors.Is(err, workflow.ErrNotFound):
		return errors.New("not_found: no execution with that ID in this workspace")
	case errors.Is(err, workflow.ErrInvalidInput):
		return errors.New(workflowUsage)
	default:
		return errors.New("database operation failed")
	}
}

type workflowCLI struct {
	store *storage.WorkflowStore
	ws    string
	out   io.Writer
}

func (c workflowCLI) emit(v any) error { return json.NewEncoder(c.out).Encode(v) }

func workflowCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(workflowUsage)
	}
	sub, rest := args[0], args[1:]
	f := commandFlags("workflow " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	limit := f.Int("limit", workflow.DefaultListLimit, "page size")
	cursor := f.String("cursor", "", "page cursor")
	state := f.String("state", "", "execution state")
	status := f.String("status", "", "job status")
	executionID := f.Int64("execution-id", 0, "execution ID")
	apply := f.Bool("apply", false, "apply the cutover")
	pos, err := parseInterspersed(f, rest)
	if err != nil || !tenant.ValidUUID(*ws) {
		return errors.New(workflowUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	allowed := map[string][]string{
		"list":        {"limit", "cursor", "state"},
		"show":        {},
		"steps":       {},
		"history":     {"limit", "cursor"},
		"jobs":        {"limit", "cursor", "status", "execution-id"},
		"dead-letter": {"limit", "cursor", "execution-id"},
		"cancel":      {},
		"resume":      {},
		"retry":       {},
		"cutover":     {"apply"},
	}
	ok, known := allowed[sub]
	if !known {
		return errors.New(workflowUsage)
	}
	for name := range set {
		if name == "workspace-id" {
			continue
		}
		permitted := false
		for _, a := range ok {
			permitted = permitted || a == name
		}
		if !permitted {
			return errors.New(workflowUsage)
		}
	}
	if !workflow.ValidListLimit(*limit) {
		return errors.New("--limit must be 1-200")
	}
	var id int64
	switch sub {
	case "show", "steps", "history", "cancel", "resume", "retry":
		if len(pos) != 1 {
			return errors.New(workflowUsage)
		}
		id, err = strconv.ParseInt(pos[0], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("execution ID must be a positive integer")
		}
	default:
		if len(pos) != 0 {
			return errors.New(workflowUsage)
		}
	}
	store, err := storage.NewWorkflowStore(pool)
	if err != nil {
		return err
	}
	c := workflowCLI{store: store, ws: *ws, out: out}
	if err := store.CheckWorkspace(ctx, *ws); err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return errors.New("workspace not found")
		}
		return errors.New("database operation failed")
	}
	switch sub {
	case "list":
		return c.list(ctx, workflow.State(*state), *limit, *cursor)
	case "show":
		return c.show(ctx, id)
	case "steps":
		return c.steps(ctx, id)
	case "history":
		return c.history(ctx, id, *limit, *cursor)
	case "jobs":
		if *status != "" && !workflow.ValidJobStatus(*status) {
			return errors.New("--status must be pending, processing, completed or dead_letter")
		}
		return c.jobs(ctx, "jobs", *status, *executionID, *limit, *cursor)
	case "dead-letter":
		return c.jobs(ctx, "dead_letters", workflow.JobDeadLetter, *executionID, *limit, *cursor)
	case "cancel":
		return c.cancel(ctx, id)
	case "resume":
		return c.resume(ctx, id)
	case "retry":
		return c.retry(ctx, id)
	case "cutover":
		return c.cutover(ctx, *apply)
	}
	return errors.New(workflowUsage)
}

func executionSummary(v workflow.ExecutionView) map[string]any {
	m := map[string]any{
		"id": v.ID, "state": v.State, "reason": string(v.Reason),
		"behavior": v.BehaviorName, "behavior_version": v.BehaviorVersion, "version_id": v.VersionID,
		"source_kind": v.References.SourceKind, "created_at": workflowTimeV(v.CreatedAt), "updated_at": workflowTimeV(v.UpdatedAt),
	}
	if v.ParentExecutionID != 0 {
		m["parent_execution_id"] = v.ParentExecutionID
	}
	if v.References.LegacyRunID != 0 {
		m["legacy_run_id"] = v.References.LegacyRunID
	}
	if v.References.LegacyJobID != 0 {
		m["legacy_job_id"] = v.References.LegacyJobID
	}
	if v.Deadline != nil {
		m["deadline_at"] = workflowTime(v.Deadline)
	}
	return m
}

func jobSummary(j workflow.JobView) map[string]any {
	m := map[string]any{
		"job_id": j.ID, "execution_id": j.ExecutionID, "step": j.StepKey, "status": j.Status,
		"attempts": j.Attempts, "max_attempts": j.MaxAttempts, "due_at": workflowTimeV(j.DueAt),
		"updated_at": workflowTimeV(j.UpdatedAt),
	}
	if j.LeaseExpiresAt != nil {
		m["lease_expires_at"] = workflowTime(j.LeaseExpiresAt)
	}
	if j.LastErrorCode != "" {
		m["error_code"] = j.LastErrorCode
	}
	if j.DeadLetterReason != "" {
		m["dead_letter_reason"] = j.DeadLetterReason
	}
	return m
}

func (c workflowCLI) list(ctx context.Context, state workflow.State, limit int, cursor string) error {
	if state != "" && !state.Valid() {
		return errors.New("--state must be an execution state such as RUNNING or FAILED")
	}
	before, err := workflow.DecodeCursor("executions", cursor)
	if err != nil {
		return errors.New("invalid --cursor")
	}
	rows, next, err := c.store.ListExecutions(ctx, workflow.ExecutionQuery{WorkspaceID: c.ws, State: state, BeforeID: before, Limit: limit})
	if err != nil {
		return workflowStoreErr(err)
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, executionSummary(r))
	}
	return c.emit(map[string]any{"executions": items, "next_cursor": workflow.EncodeCursor("executions", next)})
}

func (c workflowCLI) show(ctx context.Context, id int64) error {
	d, err := c.store.GetExecutionDetail(ctx, c.ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	m := executionSummary(d.ExecutionView)
	m["trigger_key"] = d.TriggerKey
	m["correlation_id"] = d.CorrelationID
	m["source_id"] = d.References.SourceID
	m["subject"] = d.References.Subject
	m["revision"] = d.References.Revision
	m["operation_key"] = d.References.OperationKey
	m["started_at"] = workflowTime(d.StartedAt)
	m["finished_at"] = workflowTime(d.FinishedAt)
	m["cancelled_at"] = workflowTime(d.CancelledAt)
	m["definition"] = map[string]any{
		"steps": len(d.Definition.Steps), "max_attempts": d.Definition.Retry.MaxAttempts,
		"step_timeout_s": int64(d.Definition.StepTimeout / time.Second), "lease_s": int64(d.Definition.LeaseDuration / time.Second),
		"execution_deadline_s": int64(d.Definition.ExecutionDeadline / time.Second),
	}
	if d.RetryChildID != 0 {
		m["retry_execution_id"] = d.RetryChildID
	}
	if d.LiveJob != nil {
		m["live_job"] = jobSummary(*d.LiveJob)
	}
	waits := make([]map[string]any, 0, len(d.Waits))
	for _, w := range d.Waits {
		wm := map[string]any{
			"wait_id": w.ID, "step": w.StepKey, "approval_id": w.ApprovalID, "kind": w.Kind,
			"decision": w.Decision, "decided_at": workflowTime(w.DecidedAt), "expires_at": workflowTime(w.ExpiresAt),
			"closed_at": workflowTime(w.ClosedAt), "signalled": w.Signalled, "signal_consumed_at": workflowTime(w.SignalConsumedAt),
		}
		waits = append(waits, wm)
	}
	m["waits"] = waits
	switch {
	case d.Reason == workflow.ReasonSideEffectUncertain:
		m["attention"] = "side_effect_uncertain: an action's outcome is unknown; check the provider; nothing is resent"
	case d.State.IsWaiting():
		m["attention"] = "waiting: decide the open approval, then the execution resumes (or run workflow resume)"
	}
	return c.emit(m)
}

func (c workflowCLI) steps(ctx context.Context, id int64) error {
	steps, err := c.store.ListSteps(ctx, c.ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	items := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		deps := s.DependsOn
		if deps == nil {
			deps = []string{}
		}
		m := map[string]any{
			"step": s.Key, "position": s.Position, "kind": s.Kind, "depends_on": deps, "state": s.State,
			"attempts": s.Attempts, "updated_at": workflowTimeV(s.UpdatedAt),
			"usage": map[string]int64{"input_tokens": s.Usage.InputTokens, "output_tokens": s.Usage.OutputTokens,
				"cache_read_tokens": s.Usage.CacheReadTokens, "cache_write_tokens": s.Usage.CacheWriteTokens},
		}
		if s.Checkpoint != nil {
			refs := make([]map[string]string, 0, len(s.Checkpoint.References))
			for _, r := range s.Checkpoint.References {
				refs = append(refs, map[string]string{"kind": workflowRef(r.Kind), "reference_id": workflowRef(r.ReferenceID)})
			}
			m["checkpoint"] = map[string]any{"reusable": s.Checkpoint.Reusable, "references": refs}
		}
		items = append(items, m)
	}
	return c.emit(map[string]any{"execution_id": id, "steps": items})
}

func (c workflowCLI) history(ctx context.Context, id int64, limit int, cursor string) error {
	scope := "events:" + strconv.FormatInt(id, 10)
	after, err := workflow.DecodeCursor(scope, cursor)
	if err != nil {
		return errors.New("invalid --cursor")
	}
	events, next, err := c.store.ListEvents(ctx, c.ws, id, after, limit)
	if err != nil {
		return workflowStoreErr(err)
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		m := map[string]any{"sequence": e.Sequence, "kind": e.Kind, "at": workflowTimeV(e.CreatedAt)}
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
		if e.Usage != (workflow.Usage{}) {
			m["usage"] = map[string]int64{"input_tokens": e.Usage.InputTokens, "output_tokens": e.Usage.OutputTokens,
				"cache_read_tokens": e.Usage.CacheReadTokens, "cache_write_tokens": e.Usage.CacheWriteTokens}
		}
		items = append(items, m)
	}
	return c.emit(map[string]any{"execution_id": id, "events": items, "next_cursor": workflow.EncodeCursor(scope, next)})
}

func (c workflowCLI) jobs(ctx context.Context, scope, status string, executionID int64, limit int, cursor string) error {
	if executionID < 0 {
		return errors.New("--execution-id must be a positive integer")
	}
	before, err := workflow.DecodeCursor(scope, cursor)
	if err != nil {
		return errors.New("invalid --cursor")
	}
	rows, next, err := c.store.ListJobs(ctx, workflow.JobQuery{WorkspaceID: c.ws, Status: status, ExecutionID: executionID, BeforeID: before, Limit: limit})
	if err != nil {
		return workflowStoreErr(err)
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, jobSummary(r))
	}
	return c.emit(map[string]any{scope: items, "next_cursor": workflow.EncodeCursor(scope, next)})
}

// Command idempotency keys follow plan §3.3: cancel and retry keys derive
// from workspace, execution and action; a resume key also names the newest
// wait, so a later legitimate wait is not suppressed by an earlier resume.
func cancelKey(ws string, id int64) string { return fmt.Sprintf("cli:cancel:%s:%d", ws, id) }
func retryKey(ws string, id int64) string  { return fmt.Sprintf("cli:retry:%s:%d", ws, id) }
func resumeKey(ws string, id int64, waits []workflow.WaitView) string {
	var newest int64
	for _, w := range waits {
		if w.ID > newest {
			newest = w.ID
		}
	}
	return fmt.Sprintf("cli:resume:%s:%d:wait:%d", ws, id, newest)
}

func (c workflowCLI) cancel(ctx context.Context, id int64) error {
	d, err := c.store.GetExecutionDetail(ctx, c.ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	if d.State == workflow.StateCancelled {
		return fmt.Errorf("already_cancelled: execution %d is CANCELLED", id)
	}
	if d.State.IsTerminal() {
		return fmt.Errorf("not_cancellable: execution %d already ended (%s)", id, d.State)
	}
	e, err := workflow.NewEngine(c.store).Cancel(ctx, workflow.Command{WorkspaceID: c.ws, ExecutionID: id, IdempotencyKey: cancelKey(c.ws, id)})
	switch {
	case errors.Is(err, workflow.ErrNotEligible), errors.Is(err, workflow.ErrConflict):
		if now, readErr := c.store.GetExecutionDetail(ctx, c.ws, id); readErr == nil && now.State == workflow.StateCancelled {
			return fmt.Errorf("already_cancelled: execution %d is CANCELLED", id)
		}
		return fmt.Errorf("not_cancellable: execution %d changed state; run workflow show %d", id, id)
	case err != nil:
		return workflowStoreErr(err)
	}
	return c.emit(map[string]any{"execution_id": e.ID, "outcome": "cancelled", "state": e.State, "reason": string(e.Reason),
		"note": "an external request already in flight is not rolled back"})
}

func (c workflowCLI) resume(ctx context.Context, id int64) error {
	d, err := c.store.GetExecutionDetail(ctx, c.ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	e, err := workflow.NewEngine(c.store).Resume(ctx, workflow.Command{WorkspaceID: c.ws, ExecutionID: id, IdempotencyKey: resumeKey(c.ws, id, d.Waits)})
	switch {
	case errors.Is(err, workflow.ErrNotEligible), errors.Is(err, workflow.ErrConflict):
		now, readErr := c.store.GetExecutionDetail(ctx, c.ws, id)
		if readErr != nil {
			now = d
		}
		switch {
		case now.State.IsTerminal():
			return fmt.Errorf("not_resumable: execution %d already ended (%s)", id, now.State)
		case !now.State.IsWaiting():
			return fmt.Errorf("not_resumable: execution %d is not waiting (%s)", id, now.State)
		default:
			return fmt.Errorf("not_resumable: execution %d has no recorded decision for its wait; decide the approval first", id)
		}
	case err != nil:
		return workflowStoreErr(err)
	}
	outcome := "resumed"
	if !d.State.IsWaiting() {
		outcome = "already_resumed"
	}
	return c.emit(map[string]any{"execution_id": e.ID, "outcome": outcome, "state": e.State, "reason": string(e.Reason)})
}

func (c workflowCLI) retry(ctx context.Context, id int64) error {
	d, err := c.store.GetExecutionDetail(ctx, c.ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	child, err := workflow.NewEngine(c.store).Retry(ctx, workflow.Command{WorkspaceID: c.ws, ExecutionID: id, IdempotencyKey: retryKey(c.ws, id)})
	switch {
	case errors.Is(err, workflow.ErrProviderUncertainty):
		return fmt.Errorf("side_effect_uncertain: execution %d has an action with an unknown outcome; retry refused, nothing is resent; check the provider", id)
	case errors.Is(err, workflow.ErrUnsafeRetry):
		return fmt.Errorf("unsafe_retry: execution %d has a completed step without a reusable checkpoint; retry refused", id)
	case errors.Is(err, workflow.ErrNotEligible):
		return fmt.Errorf("not_retryable: only a FAILED execution can be retried; execution %d is %s", id, d.State)
	case err != nil:
		return workflowStoreErr(err)
	}
	m := map[string]any{"execution_id": id, "retry_execution_id": child.ID, "state": child.State, "outcome": "created"}
	if d.RetryChildID != 0 {
		m["outcome"] = "existing"
		if child.State == workflow.StateFailed {
			m["hint"] = fmt.Sprintf("the retry child failed; retry execution %d instead", child.ID)
		}
	}
	return c.emit(m)
}

func (c workflowCLI) cutover(ctx context.Context, apply bool) error {
	r, err := c.store.Cutover(ctx, c.ws, apply)
	blocked := errors.Is(err, storage.ErrCutoverBlocked)
	switch {
	case errors.Is(err, workflow.ErrNotFound):
		return errors.New("workspace not found")
	case err != nil && !blocked:
		return errors.New("cutover failed; nothing was changed")
	}
	issues := make([]map[string]any, 0, len(r.Blocked))
	for _, b := range r.Blocked {
		issues = append(issues, map[string]any{"kind": b.Kind, "id": b.ID, "code": b.Code})
	}
	report := map[string]any{
		"workspace_id": c.ws, "preflight": !apply, "mode": r.Mode, "applied": r.Applied,
		"already_cut_over": r.AlreadyCutOver, "ready": r.Ready(),
		"behavior_runs": map[string]int{"pending": r.BehaviorRunsPending, "running": r.BehaviorRunsRunning,
			"lease_live": r.BehaviorRunsLeaseLive, "waiting": r.BehaviorRunsWaiting},
		"approval_waits": r.ApprovalWaits, "imported_executions": r.ImportedExecutions,
		"agent_jobs": map[string]int{"pending": r.AgentJobsPending, "pending_due": r.AgentJobsPendingDue,
			"processing": r.AgentJobsProcessing, "lease_live": r.AgentJobsLeaseLive},
		"legacy_route_approvals": r.LegacyRouteApprovals, "in_flight_tool_runs": r.InFlightToolRuns,
		"blocked": issues, "blocked_total": r.BlockedTotal,
	}
	if err := c.emit(report); err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("cutover_blocked: %d records block the cutover; nothing was written", r.BlockedTotal)
	}
	return nil
}
