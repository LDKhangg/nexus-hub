package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Owner-facing texts of /task and /retry (Vietnamese, like the rest).
const (
	usageTask       = "Cú pháp: /task <yêu cầu> (trong topic của task) hoặc /task <TASK-ID> <yêu cầu>. Nếu có nhiều repo: thêm owner/repo trước yêu cầu."
	usageRetry      = "Cú pháp: /retry [WF-id]"
	msgTaskNoRepo   = "Chưa có repo nào đăng ký trên hub (nh github-repo)."
	msgTaskTooLong  = "Yêu cầu phải có từ 1 đến 2.000 ký tự."
	msgTaskNotReady = "Không bắt đầu được việc này, thử lại sau."
)

// taskCommand starts a local task: "/task <instruction>" inside the topic of
// a ClickUp task, or "/task <TASK-ID or link> <instruction>" anywhere. When
// several repositories are registered the instruction starts with
// owner/repo.
func (w *InboxWorker) taskCommand(ctx context.Context, u InboundUpdate, to ChatRef, args string) outcome {
	if w.d.LocalTasks == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	args = strings.TrimSpace(args)
	ref, rest, ok := w.taskTarget(ctx, u, args)
	if !ok {
		return w.reply(ctx, to, usageTask, "usage")
	}
	canonical, err := w.resolveTask(ctx, u.WorkspaceID, ref)
	if err != nil {
		return w.reply(ctx, to, msgTopicUnread, "task_unreadable")
	}
	repos, err := w.d.LocalTasks.RegisteredRepos(ctx, u.WorkspaceID)
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	repo, rest, out := w.taskRepo(ctx, to, repos, rest)
	if out != nil {
		return *out
	}
	instruction := sanitizeWorkflowInstruction(rest)
	if instruction == "" || utf8.RuneCountInString(instruction) > maxLocalInstructionRunes {
		return w.reply(ctx, to, msgTaskTooLong, "usage")
	}
	state := conversationFromContext(ctx)
	if w.d.Conversations == nil || state.id <= 0 {
		return w.reply(ctx, to, msgTaskNotReady, "handler_failed")
	}
	exec, err := w.d.LocalTasks.StartLocalTask(ctx, LocalTaskRequest{
		WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID, UpdateID: u.UpdateID,
		Repo: repo, TaskRef: ref.ID, ClickUpTaskID: canonical,
	})
	switch {
	case errors.Is(err, ErrNotAvailable):
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	case err != nil:
		return w.reply(ctx, to, msgTaskNotReady, "handler_failed")
	}
	if _, err := w.d.Conversations.AppendInstruction(ctx, workflow.InstructionRequest{WorkspaceID: u.WorkspaceID,
		ExecutionID: exec.ID, Text: instruction, ConversationID: state.id, TelegramUpdateID: u.UpdateID}); err != nil {
		// Without its instruction the task cannot run: end it.
		if eng := w.workflowEngine(); eng != nil {
			_, _ = eng.Cancel(ctx, workflow.Command{WorkspaceID: u.WorkspaceID, ExecutionID: exec.ID,
				IdempotencyKey: fmt.Sprintf("telegram:task-abort:%s:%d", u.WorkspaceID, exec.ID)})
		}
		return w.reply(ctx, to, msgTaskNotReady, "storage")
	}
	_ = w.d.Conversations.BindWorkflow(ctx, u.WorkspaceID, state.id, exec.ID, "task")
	if u.MessageID > 0 {
		_ = w.d.Conversations.BindResource(ctx, workflow.ResourceBinding{WorkspaceID: u.WorkspaceID, ExecutionID: exec.ID,
			Kind: "telegram.message", ExternalID: workflow.TelegramMessageRef(u.IntegrationID, u.ChatID, u.MessageID)})
	}
	return w.reply(ctx, to, fmt.Sprintf("%s: đã giao việc cho máy local (repo %s, task %s). Tiến độ sẽ hiện trong topic của task.",
		workflow.FormatRef(exec.ID), repo, ref.ID), "")
}

// taskTarget returns the ClickUp task and the rest of the arguments: the
// subject of the topic the command was written in, else the first word.
func (w *InboxWorker) taskTarget(ctx context.Context, u InboundUpdate, args string) (TaskRef, string, bool) {
	if topic, ok := w.topicOf(ctx, u); ok && topic.SubjectKind == TopicSubjectClickUpTask {
		if key := strings.TrimSpace(topic.SubjectKey); taskIDRe.MatchString(key) {
			return TaskRef{ID: key}, args, true
		}
	}
	first, rest, _ := strings.Cut(args, " ")
	ref, ok := ParseTaskRefTeam(first)
	return ref, strings.TrimSpace(rest), ok
}

// taskRepo picks the repository: an explicit registered owner/repo first
// word, else the only registered one. A non-nil outcome is the reply to send.
func (w *InboxWorker) taskRepo(ctx context.Context, to ChatRef, repos []string, rest string) (string, string, *outcome) {
	first, tail, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if tenant.ValidRepo(first) {
		for _, r := range repos {
			if strings.EqualFold(r, first) {
				return r, strings.TrimSpace(tail), nil
			}
		}
	}
	switch len(repos) {
	case 0:
		out := w.reply(ctx, to, msgTaskNoRepo, "no_repo")
		return "", "", &out
	case 1:
		return repos[0], rest, nil
	}
	out := w.reply(ctx, to, "Có nhiều repo: "+strings.Join(repos, ", ")+"\n"+usageTask, "usage")
	return "", "", &out
}

// retryCommand queues the local job of a workflow again: "/retry WF-n", or
// the subject's only active task inside a topic.
func (w *InboxWorker) retryCommand(ctx context.Context, u InboundUpdate, args string) (string, error) {
	if w.d.WorkerJobs == nil {
		return "", ErrNotAvailable
	}
	id, ok := parseJobExecution(args)
	if !ok {
		if args != "" {
			return "", &UsageError{Usage: usageRetry}
		}
		if id, ok = w.topicTarget(ctx, u).single(); !ok {
			return "", &UsageError{Usage: usageRetry}
		}
	}
	job, found, err := w.d.WorkerJobs.JobForStep(ctx, u.WorkspaceID, id, StepKeyWorkerRun)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	if job.State != WorkerJobFailed && job.State != WorkerJobNeedsInput && job.State != WorkerJobInterrupted {
		return fmt.Sprintf("%s: việc đang ở trạng thái \"%s\", không cần thử lại.", workflow.FormatRef(id), WorkerJobStateText(job.State)), nil
	}
	retried, err := w.d.WorkerJobs.RetryJob(ctx, u.WorkspaceID, job.ID, job.Attempt)
	if errors.Is(err, ErrConflict) {
		return "", ErrDecided
	}
	if err != nil {
		return "", err
	}
	if w.d.WorkerNotifier != nil {
		w.d.WorkerNotifier.WorkerJobQueued(ctx, retried)
	}
	return workflow.FormatRef(id) + ": đã xếp lại hàng chờ máy local.", nil
}

// workerCards renders one card per device (REQ-18).
func (w *InboxWorker) workerCards(ctx context.Context, workspaceID string) (string, error) {
	devices, err := w.d.Workers.ListDevices(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "Chưa có máy local nào. Dùng /worker enroll để ghép nối.", nil
	}
	var records []WorkerTelemetryRecord
	if w.d.WorkerTelemetry != nil {
		if records, err = w.d.WorkerTelemetry.ListTelemetry(ctx, workspaceID); err != nil {
			records = nil
		}
	}
	cards := make([]string, 0, len(devices))
	for _, info := range BuildWorkerDeviceInfos(devices, records, w.now()) {
		cards = append(cards, FormatWorkerCard(info))
	}
	return strings.Join(cards, "\n\n"), nil
}

// localJobLine is the extra /status line of a local task: the job state and
// the device holding it. Empty when the execution has no worker job.
func (w *InboxWorker) localJobLine(ctx context.Context, workspaceID string, executionID int64) string {
	if w.d.WorkerJobs == nil {
		return ""
	}
	job, ok, err := w.d.WorkerJobs.JobForStep(ctx, workspaceID, executionID, StepKeyWorkerRun)
	if err != nil || !ok {
		return ""
	}
	line := "Máy local: " + job.State + " (" + WorkerJobStateText(job.State) + ")"
	if job.DeviceID != "" && w.d.Workers != nil {
		if devices, err := w.d.Workers.ListDevices(ctx, workspaceID); err == nil {
			for _, d := range devices {
				if d.ID == job.DeviceID {
					line += " · " + d.Name
				}
			}
		}
	}
	if job.Attempt > 0 {
		line += fmt.Sprintf(" · lần %d", job.Attempt)
	}
	return line
}
