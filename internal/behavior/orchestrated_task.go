package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Owner-facing texts of /plan (Vietnamese, like the rest).
const (
	usagePlan          = "Cú pháp: /plan <yêu cầu> (trong topic của task) hoặc /plan <TASK-ID> <yêu cầu>. Có thể thêm owner/repo trước yêu cầu."
	msgPlanNotEnabled  = "Chế độ lập kế hoạch bằng AI chưa bật trên hub này."
	msgPlanNotReady    = "Không bắt đầu được việc này, thử lại sau."
	msgPlanRepoUnknown = "Repo này chưa đăng ký trên hub (nh github-repo)."
)

// OrchestratedTaskStarter starts orchestrated tasks (implemented by
// storage.OrchestratedTaskStore). A repeated request of the same update
// returns the same execution; ErrNotAvailable when the workspace does not
// run durable workflows. The caller stores the request as the execution's
// first instruction right after.
type OrchestratedTaskStarter interface {
	StartOrchestratedTask(ctx context.Context, req orchestrator.TaskRequest) (workflow.Execution, error)
}

// OrchestratorPlanReader reads the stored plans of an execution (the read
// side of orchestrator.PlanStore). Used by /status.
type OrchestratorPlanReader interface {
	ListPlans(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.PlanRecord, error)
	ListTasks(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.PlanTaskRecord, error)
	ListDecisions(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.DecisionRecord, error)
}

// Statuses of startPlan; "" is success.
const (
	planStatusUsage          = "usage"
	planStatusNotAvailable   = "not_available"
	planStatusTaskUnreadable = "task_unreadable"
	planStatusRepoUnknown    = "repo_not_registered"
	planStatusFailed         = "failed"
)

// planCommand starts an orchestrated task: "/plan <request>" inside the topic
// of a ClickUp task, or "/plan <TASK-ID or link> <request>" anywhere. An
// optional registered owner/repo may precede the request.
func (w *InboxWorker) planCommand(ctx context.Context, u InboundUpdate, to ChatRef, args string) outcome {
	if w.d.OrchestratedTasks == nil {
		return w.reply(ctx, to, msgPlanNotEnabled, "not_available")
	}
	ref, rest, ok := w.taskTarget(ctx, u, strings.TrimSpace(args))
	if !ok {
		return w.reply(ctx, to, usagePlan, "usage")
	}
	repo, request, status := w.explicitRepo(ctx, u.WorkspaceID, rest)
	var exec workflow.Execution
	if status == "" {
		state := conversationFromContext(ctx)
		exec, status = w.startPlan(ctx, u, ref, repo, request, state.id)
	}
	switch status {
	case "":
	case planStatusUsage:
		return w.reply(ctx, to, msgTaskTooLong, "usage")
	case planStatusNotAvailable:
		return w.reply(ctx, to, msgPlanNotEnabled, "not_available")
	case planStatusTaskUnreadable:
		return w.reply(ctx, to, msgTopicUnread, "task_unreadable")
	case planStatusRepoUnknown:
		return w.reply(ctx, to, msgPlanRepoUnknown, "no_repo")
	default:
		return w.reply(ctx, to, msgPlanNotReady, "handler_failed")
	}
	return w.reply(ctx, to, workflow.FormatRef(exec.ID)+": đã nhận, đang lập kế hoạch…", "")
}

// explicitRepo peels an owner/repo first word. A first word that is not
// owner/repo shaped is part of the request (repo ""). An owner/repo first
// word must be registered: else the status is planStatusRepoUnknown (as for
// start_task), never a fall-through to another repo; a read error is
// planStatusFailed.
func (w *InboxWorker) explicitRepo(ctx context.Context, workspaceID, rest string) (string, string, string) {
	rest = strings.TrimSpace(rest)
	first, tail, _ := strings.Cut(rest, " ")
	if !tenant.ValidRepo(first) {
		return "", rest, ""
	}
	if w.d.LocalTasks == nil {
		return "", rest, planStatusRepoUnknown
	}
	repos, err := w.d.LocalTasks.RegisteredRepos(ctx, workspaceID)
	if err != nil {
		return "", rest, planStatusFailed
	}
	for _, r := range repos {
		if strings.EqualFold(r, first) {
			return r, strings.TrimSpace(tail), ""
		}
	}
	return "", rest, planStatusRepoUnknown
}

// topicTaskRef is the ClickUp task whose topic the update was written in.
func (w *InboxWorker) topicTaskRef(ctx context.Context, u InboundUpdate) (TaskRef, bool) {
	if topic, ok := w.topicOf(ctx, u); ok && topic.SubjectKind == TopicSubjectClickUpTask {
		if key := strings.TrimSpace(topic.SubjectKey); taskIDRe.MatchString(key) {
			return TaskRef{ID: key}, true
		}
	}
	return TaskRef{}, false
}

// startPlan is the path shared by /plan and the start_task tool: validate the
// request, resolve the task, start the execution (idempotent per update) and
// store the request as its first instruction. The status is "" on success.
// explicitRepo is "" or a registered repo.
func (w *InboxWorker) startPlan(ctx context.Context, u InboundUpdate, ref TaskRef, explicitRepo, request string, conversationID int64) (workflow.Execution, string) {
	if w.d.OrchestratedTasks == nil {
		return workflow.Execution{}, planStatusNotAvailable
	}
	instruction := sanitizeWorkflowInstruction(request)
	if instruction == "" || utf8.RuneCountInString(instruction) > maxLocalInstructionRunes {
		return workflow.Execution{}, planStatusUsage
	}
	canonical, err := w.resolveTask(ctx, u.WorkspaceID, ref)
	if err != nil {
		return workflow.Execution{}, planStatusTaskUnreadable
	}
	if w.d.Conversations == nil || conversationID <= 0 {
		return workflow.Execution{}, planStatusFailed
	}
	exec, err := w.d.OrchestratedTasks.StartOrchestratedTask(ctx, orchestrator.TaskRequest{
		WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID, ThreadID: u.ThreadID, UpdateID: u.UpdateID,
		Repo: explicitRepo, TaskRef: ref.ID, ClickUpTaskID: canonical,
	})
	switch {
	case errors.Is(err, ErrNotAvailable):
		return workflow.Execution{}, planStatusNotAvailable
	case err != nil:
		return workflow.Execution{}, planStatusFailed
	}
	if _, err := w.d.Conversations.AppendInstruction(ctx, workflow.InstructionRequest{WorkspaceID: u.WorkspaceID,
		ExecutionID: exec.ID, Text: instruction, ConversationID: conversationID, TelegramUpdateID: u.UpdateID}); err != nil {
		// Without its instruction the plan cannot be made: end it.
		if eng := w.workflowEngine(); eng != nil {
			_, _ = eng.Cancel(ctx, workflow.Command{WorkspaceID: u.WorkspaceID, ExecutionID: exec.ID,
				IdempotencyKey: fmt.Sprintf("telegram:plan-abort:%s:%d", u.WorkspaceID, exec.ID)})
		}
		return workflow.Execution{}, planStatusFailed
	}
	_ = w.d.Conversations.BindWorkflow(ctx, u.WorkspaceID, conversationID, exec.ID, "task")
	if u.MessageID > 0 {
		_ = w.d.Conversations.BindResource(ctx, workflow.ResourceBinding{WorkspaceID: u.WorkspaceID, ExecutionID: exec.ID,
			Kind: "telegram.message", ExternalID: workflow.TelegramMessageRef(u.IntegrationID, u.ChatID, u.MessageID)})
	}
	return exec, ""
}

// startTaskAvailable reports whether start_task is offered: the orchestrator
// is wired and instructions can be stored. The assistant only answers the
// owner's chat.
func (w *InboxWorker) startTaskAvailable() bool {
	return w.d.OrchestratedTasks != nil && w.d.Conversations != nil
}

// toolStartTask starts an orchestrated task. The model gets status codes and
// the workflow id only, never the plan or any text.
func (w *InboxWorker) toolStartTask(ctx context.Context, tt *assistantToolTurn, args toolArgs) (string, bool) {
	request, err := args.str("request")
	if err != nil || request == "" {
		return toolErr(errToolArgs)
	}
	rawTask, err := args.str("task")
	if err != nil {
		return toolErr(errToolArgs)
	}
	rawRepo, err := args.str("repo")
	if err != nil {
		return toolErr(errToolArgs)
	}
	if !w.startTaskAvailable() {
		return toolJSON(map[string]any{"status": planStatusNotAvailable})
	}
	var ref TaskRef
	switch {
	case rawTask != "":
		var ok bool
		if ref, ok = taskURLTaskRef(rawTask); !ok {
			ref, ok = ParseTaskRefTeam(rawTask)
		}
		if !ok {
			return toolErr(errToolArgs)
		}
	default:
		var ok bool
		if ref, ok = w.topicTaskRef(ctx, tt.u); !ok {
			return toolJSON(map[string]any{"status": "task_required"})
		}
	}
	repo := ""
	if rawRepo != "" {
		repo = w.registeredRepo(ctx, tt.u.WorkspaceID, rawRepo)
		if repo == "" {
			return toolJSON(map[string]any{"status": planStatusRepoUnknown})
		}
	}
	if tt.proposed == nil {
		tt.proposed = make(map[string]bool)
	}
	if tt.proposed[toolStartTaskName] {
		return toolJSON(map[string]any{"status": "already_started"})
	}
	conversation, err := w.ensureConversation(ctx, tt.to)
	if err != nil || conversation.ID <= 0 {
		return toolJSON(map[string]any{"status": planStatusFailed})
	}
	exec, status := w.startPlan(ctx, tt.u, ref, repo, request, conversation.ID)
	if status != "" {
		return toolJSON(map[string]any{"status": status})
	}
	tt.proposed[toolStartTaskName] = true
	return toolJSON(map[string]any{"status": "started", "workflow": workflow.FormatRef(exec.ID)})
}

// registeredRepo returns the registered repo equal to name, else "".
func (w *InboxWorker) registeredRepo(ctx context.Context, workspaceID, name string) string {
	if w.d.LocalTasks == nil || !tenant.ValidRepo(name) {
		return ""
	}
	repos, err := w.d.LocalTasks.RegisteredRepos(ctx, workspaceID)
	if err != nil {
		return ""
	}
	for _, r := range repos {
		if strings.EqualFold(r, name) {
			return r
		}
	}
	return ""
}

// maxStatusPlanTasks bounds the task lines of one /status.
const maxStatusPlanTasks = 30

// orchestratedStatusLines are the extra /status lines of an orchestrated
// execution: plan version, each task's state and executor, the last decision
// and the budget used. Metadata only: titles are the sanitized stored
// titles, no model text. Empty for any other execution or on read errors.
func (w *InboxWorker) orchestratedStatusLines(ctx context.Context, workspaceID string, d workflow.ExecutionDetail, steps []workflow.Step) []string {
	if w.d.OrchestratorPlans == nil || d.References.SourceKind != orchestrator.WorkflowSourceOrchestratedTask {
		return nil
	}
	plans, err := w.d.OrchestratorPlans.ListPlans(ctx, workspaceID, d.ID)
	if err != nil {
		return nil
	}
	var lines []string
	var latest *orchestrator.PlanRecord
	for i := range plans {
		if latest == nil || plans[i].Version > latest.Version {
			latest = &plans[i]
		}
	}
	if latest == nil {
		lines = append(lines, "Kế hoạch: chưa có")
	} else {
		tasks, err := w.d.OrchestratorPlans.ListTasks(ctx, workspaceID, d.ID)
		if err != nil {
			return nil
		}
		var own []orchestrator.PlanTaskRecord
		for _, t := range tasks {
			if t.PlanID == latest.ID {
				own = append(own, t)
			}
		}
		lines = append(lines, fmt.Sprintf("Kế hoạch: v%d (%s) · %d việc", latest.Version, latest.Status, len(own)))
		for i, t := range own {
			if i == maxStatusPlanTasks {
				lines = append(lines, fmt.Sprintf("… và %d việc nữa", len(own)-i))
				break
			}
			executor := t.Executor
			if executor == "" {
				executor = "-"
			}
			lines = append(lines, fmt.Sprintf("- %s · %s · %s · %s", oneLine(t.Key, 40), oneLine(t.Title, 60), executor, t.State))
		}
	}
	if decisions, err := w.d.OrchestratorPlans.ListDecisions(ctx, workspaceID, d.ID); err == nil && len(decisions) > 0 {
		last := decisions[0]
		for _, dec := range decisions[1:] {
			if dec.ID > last.ID {
				last = dec
			}
		}
		line := "Quyết định cuối: " + oneLine(last.Decision, 40)
		if last.ReasonCode != "" {
			line += " (" + oneLine(last.ReasonCode, 60) + ")"
		}
		if last.TaskKey != "" {
			line += " · " + oneLine(last.TaskKey, 40)
		}
		if last.ConvertedFrom != "" {
			line += " · converted_from " + oneLine(last.ConvertedFrom, 40)
		}
		lines = append(lines, line)
	}
	if snap, ok := orchestrator.LatestBudgetSnapshot(steps); ok {
		end := w.now()
		if d.FinishedAt != nil {
			end = *d.FinishedAt
		}
		wall := end.Sub(snap.StartedAt)
		if wall < 0 {
			wall = 0
		}
		lines = append(lines, fmt.Sprintf("Ngân sách: lập kế hoạch %d/%d · lập lại %d/%d · sửa %d/%d · token %d/%d · thời gian %d/%d phút",
			snap.Used.PlanIterations, snap.Caps.PlanIterations, snap.Used.Replans, snap.Caps.Replans,
			snap.Used.RepairAttempts, snap.Caps.RepairAttempts, snap.Used.Tokens, snap.Caps.Tokens,
			int(wall/time.Minute), int(snap.Caps.Wall/time.Minute)))
	}
	return lines
}
