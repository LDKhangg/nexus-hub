package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 REQ-9: the final report of a /task or /plan execution on a
// ClickUp task carries "📤 Đăng lên ClickUp". Pressing it makes a ClickUp
// draft (source execution_report) from the stored result; the owner still
// confirms the draft with "Đăng". The button data is "xr:<execution id>":
// like every button it is accepted only from the owner chat, and the press
// is checked against the stored route of the execution.

const (
	executionReportCallbackPrefix = "xr:"
	// executionReportMaxRunes keeps the draft text well under the draft limit.
	executionReportMaxRunes = 6000
)

// ErrNoReportResult: the execution has no stored result to report.
var ErrNoReportResult = errors.New("behavior: execution has no stored result")

// ExecutionReport is what a report draft is made from.
type ExecutionReport struct {
	IntegrationID string
	ChatID        int64
	TaskID        string // the execution's ClickUp task
	Text          string // the draft text
}

// ExecutionReportSource reads the stored result of an ended /task or /plan
// execution. ErrNotFound when the execution has no ClickUp task or route;
// ErrNoReportResult when it has no stored result.
type ExecutionReportSource interface {
	ExecutionReport(ctx context.Context, workspaceID string, executionID int64) (ExecutionReport, error)
}

// ExecutionReportButtons tells the worker notifier whether the final report
// of a /task job gets the button (the execution is a local task, not a job
// of an orchestrated plan).
type ExecutionReportButtons interface {
	LocalTaskReportButton(ctx context.Context, workspaceID string, executionID int64) (Button, bool)
}

// ReportDrafts is the part of ClickUpDrafts the report button and the
// assistant tool use.
type ReportDrafts interface {
	Create(ctx context.Context, r DraftRequest) (int64, error)
	OpenExecutionReportDraft(ctx context.Context, workspaceID string, executionID int64) (ClickUpDraft, bool, error)
}

var _ ReportDrafts = (*ClickUpDrafts)(nil)

// executionReportDraftFinder is the optional store lookup of an open report
// draft.
type executionReportDraftFinder interface {
	OpenExecutionReportDraft(ctx context.Context, workspaceID string, executionID int64) (ClickUpDraft, bool, error)
}

// OpenExecutionReportDraft returns the draft of an execution's report that
// can still be posted; ErrNotAvailable when the store has no such lookup.
func (c *ClickUpDrafts) OpenExecutionReportDraft(ctx context.Context, workspaceID string, executionID int64) (ClickUpDraft, bool, error) {
	f, ok := c.d.Store.(executionReportDraftFinder)
	if !ok {
		return ClickUpDraft{}, false, ErrNotAvailable
	}
	return f.OpenExecutionReportDraft(ctx, workspaceID, executionID)
}

// reportPreviewResender is the optional part of ReportDrafts that sends the
// preview of an open draft whose first preview never arrived.
type reportPreviewResender interface {
	ResendPreview(ctx context.Context, d ClickUpDraft) error
}

// ResendPreview sends the preview of a pending draft that has none. The
// draft keeps its text and expiry. ErrConflict when it needs no new preview.
func (c *ClickUpDrafts) ResendPreview(ctx context.Context, d ClickUpDraft) error {
	if d.State != DraftPending || d.PreviewMessageID != 0 || d.Body == "" {
		return ErrConflict
	}
	return c.sendPreview(ctx, d)
}

// executionReportCallbackData is the data of the report button.
func executionReportCallbackData(executionID int64) (string, bool) {
	if executionID <= 0 {
		return "", false
	}
	data := executionReportCallbackPrefix + strconv.FormatInt(executionID, 10)
	return data, len(data) <= MaxCallbackData
}

// ExecutionReportButton is the "📤 Đăng lên ClickUp" button of an execution.
func ExecutionReportButton(executionID int64) (Button, bool) {
	data, ok := executionReportCallbackData(executionID)
	if !ok {
		return Button{}, false
	}
	return Button{Text: btnPostToClickUp, Data: data}, true
}

// parseExecutionReportCallback returns the execution id of "xr:<id>" data.
func parseExecutionReportCallback(data string) (int64, bool) {
	raw, ok := strings.CutPrefix(data, executionReportCallbackPrefix)
	if !ok || len(data) > MaxCallbackData || raw == "" || raw[0] == '0' {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

// ExecutionReports reads the stored result of /task and /plan executions
// from the stores the executions already use.
type ExecutionReports struct {
	Executions workflow.Reader          // execution kind
	Routes     LocalTaskRoutes          // /task route
	Jobs       WorkerJobControl         // /task job result
	PlanRoutes orchestrator.RouteReader // /plan route; nil: /plan is not reported
	Plans      OrchestratorPlanReader   // /plan tasks
}

var (
	_ ExecutionReportSource  = (*ExecutionReports)(nil)
	_ ExecutionReportButtons = (*ExecutionReports)(nil)
)

func (r *ExecutionReports) detail(ctx context.Context, workspaceID string, executionID int64) (workflow.ExecutionDetail, error) {
	if r == nil || r.Executions == nil {
		return workflow.ExecutionDetail{}, ErrNotAvailable
	}
	d, err := r.Executions.GetExecutionDetail(ctx, workspaceID, executionID)
	if errors.Is(err, workflow.ErrNotFound) {
		return workflow.ExecutionDetail{}, ErrNotFound
	}
	return d, err
}

// reportOutcomeLine is the line that tells ClickUp readers the execution
// did not finish well; "" for any other execution. jobState is the /task
// job's state, "" for a /plan execution.
func reportOutcomeLine(state workflow.State, jobState string) string {
	switch {
	case state == workflow.StateCancelled, jobState == WorkerJobCancelled:
		return msgReportOutcomeCancelled
	case state == workflow.StateFailed, jobState == WorkerJobFailed:
		return msgReportOutcomeFailed
	}
	return ""
}

// LocalTaskReportButton implements ExecutionReportButtons.
func (r *ExecutionReports) LocalTaskReportButton(ctx context.Context, workspaceID string, executionID int64) (Button, bool) {
	d, err := r.detail(ctx, workspaceID, executionID)
	if err != nil || d.References.SourceKind != WorkflowSourceLocalTask {
		return Button{}, false
	}
	return ExecutionReportButton(executionID)
}

// ExecutionReport implements ExecutionReportSource.
func (r *ExecutionReports) ExecutionReport(ctx context.Context, workspaceID string, executionID int64) (ExecutionReport, error) {
	d, err := r.detail(ctx, workspaceID, executionID)
	if err != nil {
		return ExecutionReport{}, err
	}
	switch d.References.SourceKind {
	case WorkflowSourceLocalTask:
		return r.localReport(ctx, workspaceID, executionID, d.State)
	case orchestrator.WorkflowSourceOrchestratedTask:
		return r.planReport(ctx, workspaceID, executionID, d.State)
	}
	return ExecutionReport{}, ErrNotFound
}

func (r *ExecutionReports) localReport(ctx context.Context, workspaceID string, executionID int64, state workflow.State) (ExecutionReport, error) {
	if r.Routes == nil || r.Jobs == nil {
		return ExecutionReport{}, ErrNotAvailable
	}
	route, err := r.Routes.GetRoute(ctx, workspaceID, executionID)
	if err != nil {
		return ExecutionReport{}, err
	}
	if route.ClickUpTaskID == "" {
		return ExecutionReport{}, ErrNotFound
	}
	job, ok, err := r.Jobs.JobForStep(ctx, workspaceID, executionID, StepKeyWorkerRun)
	if err != nil {
		return ExecutionReport{}, err
	}
	if !ok || job.Result == nil {
		return ExecutionReport{}, ErrNoReportResult
	}
	text := reportResultText(workflow.FormatRef(executionID), reportOutcomeLine(state, job.State), job.Result.Summary, job.Result.PRURL, job.Result.Branch)
	if text == "" {
		return ExecutionReport{}, ErrNoReportResult
	}
	return ExecutionReport{IntegrationID: route.IntegrationID, ChatID: route.ChatID, TaskID: route.ClickUpTaskID, Text: text}, nil
}

func (r *ExecutionReports) planReport(ctx context.Context, workspaceID string, executionID int64, state workflow.State) (ExecutionReport, error) {
	if r.PlanRoutes == nil || r.Plans == nil {
		return ExecutionReport{}, ErrNotAvailable
	}
	route, err := r.PlanRoutes.GetRoute(ctx, workspaceID, executionID)
	if err != nil {
		if errors.Is(err, orchestrator.ErrRouteNotFound) {
			return ExecutionReport{}, ErrNotFound
		}
		return ExecutionReport{}, err
	}
	if route.ClickUpTaskID == "" {
		return ExecutionReport{}, ErrNotFound
	}
	tasks, err := r.Plans.ListTasks(ctx, workspaceID, executionID)
	if err != nil {
		return ExecutionReport{}, err
	}
	var latest int64
	for _, t := range tasks {
		latest = max(latest, t.PlanID)
	}
	var b strings.Builder
	b.WriteString(msgReportHead + " " + workflow.FormatRef(executionID) + ":")
	if line := reportOutcomeLine(state, ""); line != "" {
		b.WriteString("\n" + line)
	}
	found := false
	for _, t := range tasks {
		if t.PlanID != latest {
			continue
		}
		var res map[string]string
		_ = json.Unmarshal(t.Result, &res)
		pr := res["pr_url"]
		if !strings.HasPrefix(pr, "https://") {
			pr = ""
		}
		summary, branch := strings.TrimSpace(res["summary"]), strings.TrimSpace(res["branch"])
		if summary == "" && pr == "" && branch == "" {
			continue
		}
		found = true
		b.WriteString("\n\n- " + oneLine(t.Title, 200))
		if summary != "" {
			b.WriteString("\n" + capRunes(summary, 1500))
		}
		if branch != "" {
			b.WriteString("\nNhánh: " + oneLine(branch, 200))
		}
		if pr != "" {
			b.WriteString("\nPR: " + pr)
		}
	}
	if !found {
		return ExecutionReport{}, ErrNoReportResult
	}
	return ExecutionReport{IntegrationID: route.IntegrationID, ChatID: route.ChatID, TaskID: route.ClickUpTaskID,
		Text: capRunes(b.String(), executionReportMaxRunes)}, nil
}

// reportResultText is the comment text of one worker result: the outcome
// line (if any), summary, PR URL and branch. "" when the result has no
// summary, PR URL or branch.
func reportResultText(ref, outcomeLine, summary, prURL, branch string) string {
	summary, branch = strings.TrimSpace(summary), strings.TrimSpace(branch)
	if !strings.HasPrefix(prURL, "https://") {
		prURL = ""
	}
	if summary == "" && prURL == "" && branch == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(msgReportHead + " " + ref + ":")
	if outcomeLine != "" {
		b.WriteString("\n" + outcomeLine)
	}
	if summary != "" {
		b.WriteString("\n" + capRunes(summary, WorkerMaxSummaryRunes))
	}
	if branch != "" {
		b.WriteString("\nNhánh: " + oneLine(branch, 200))
	}
	if prURL != "" {
		b.WriteString("\nPR: " + prURL)
	}
	return capRunes(b.String(), executionReportMaxRunes)
}

// executionReportCallback answers a press on "📤 Đăng lên ClickUp": it
// makes the report draft (its preview goes where the button is) or reuses
// the one still open. The execution is never changed.
func (w *InboxWorker) executionReportCallback(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	if qid, _ := callbackArgs(u.Args); qid == "" {
		return outcome{code: "bad_callback"}
	}
	id, ok := parseExecutionReportCallback(u.Command)
	if !ok || u.Note != "" {
		return w.answerReportCallback(ctx, to, u, msgBadButton, NoteBadCallback)
	}
	if w.d.ExecutionReports == nil || w.d.ReportDrafts == nil {
		return w.answerReportCallback(ctx, to, u, msgNotAvailable, "not_available")
	}
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	defer cancel()
	rep, err := w.d.ExecutionReports.ExecutionReport(hctx, u.WorkspaceID, id)
	switch {
	case errors.Is(err, ErrNoReportResult):
		return w.answerReportCallback(ctx, to, u, msgReportNoResult, "no_result")
	case errors.Is(err, ErrNotFound):
		return w.answerReportCallback(ctx, to, u, msgReportNoTask, "no_task")
	case errors.Is(err, ErrNotAvailable):
		return w.answerReportCallback(ctx, to, u, msgNotAvailable, "not_available")
	case err != nil:
		return w.answerReportCallback(ctx, to, u, msgReportFailed, "report_read")
	}
	if rep.ChatID != u.ChatID || (rep.IntegrationID != "" && !strings.EqualFold(rep.IntegrationID, u.IntegrationID)) {
		return w.answerReportCallback(ctx, to, u, msgStale, "stale")
	}
	if open, found, err := w.d.ReportDrafts.OpenExecutionReportDraft(hctx, u.WorkspaceID, id); err != nil && !errors.Is(err, ErrNotAvailable) {
		return w.answerReportCallback(ctx, to, u, msgReportFailed, "report_draft")
	} else if found && open.ID > 0 {
		// A draft whose preview never arrived cannot be seen or cancelled by
		// the owner: this press sends its preview again.
		if resender, ok := w.d.ReportDrafts.(reportPreviewResender); ok && open.State == DraftPending && open.PreviewMessageID == 0 {
			if err := resender.ResendPreview(hctx, open); err != nil {
				log.Printf("execution report: draft %d preview not sent again", open.ID)
				return w.answerReportCallback(ctx, to, u, msgReportPreviewFailed, "preview_not_sent")
			}
			return w.answerReportCallback(ctx, to, u, msgReportDraftMade, "")
		}
		return w.answerReportCallback(ctx, to, u, msgReportDraftOpen, "")
	}
	draftID, err := w.d.ReportDrafts.Create(hctx, DraftRequest{
		Chat: to, TaskID: rep.TaskID, Text: rep.Text, Source: DraftSourceExecutionReport, ExecutionID: id,
	})
	switch {
	case err != nil && draftID > 0:
		log.Printf("execution report: draft %d made, preview not sent", draftID)
		return w.answerReportCallback(ctx, to, u, msgReportPreviewFailed, "preview_not_sent")
	case err != nil:
		return w.answerReportCallback(ctx, to, u, msgReportFailed, "report_draft")
	}
	return w.answerReportCallback(ctx, to, u, msgReportDraftMade, "")
}

// CommentLinkReader reads the ClickUp comment a Telegram message shows
// (TopicRouter implements it).
type CommentLinkReader interface {
	LookupCommentLink(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (CommentLink, error)
}

var _ CommentLinkReader = (*TopicRouter)(nil)
