package behavior

import (
	"context"
	"errors"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Texts and data of the on-demand task topic of a notice (owner-facing,
// Vietnamese like the rest).
const (
	btnOpenTaskTopic = "📌 Mở topic task"
	// taskTopicCallbackPrefix starts the data of the "📌 Mở topic task"
	// button: "tt:<task id>". Like every button it is accepted only from
	// the owner chat (the Telegram handler's owner rule).
	taskTopicCallbackPrefix = "tt:"
	msgTaskTopicOpening     = "Đang mở topic…"
	// maxMessageExecutions bounds the executions read for one message.
	maxMessageExecutions = 5
)

// TaskTopicButton returns the "📌 Mở topic task" button for a message of in
// that goes to General or to a fixed topic (report.topic general or a fixed
// name) while its run has a ClickUp task. Pressing it creates or reuses the
// task's topic. Task and workflow modes have their own topic: no button.
func (r *TopicRouter) TaskTopicButton(ctx context.Context, workspaceID string, in TopicIntent) (Button, bool) {
	if r == nil {
		return Button{}, false
	}
	switch strings.TrimSpace(in.Mode) {
	case "", ReportTopicTask, ReportTopicWorkflow:
		return Button{}, false
	}
	data, ok := taskTopicCallbackData(r.intentTaskID(ctx, workspaceID, in))
	if !ok {
		return Button{}, false
	}
	return Button{Text: btnOpenTaskTopic, Data: data}, true
}

// taskTopicCallbackData is the button data for taskID; ok is false when
// the ID is not a task ID or the data would not fit Telegram's limit.
func taskTopicCallbackData(taskID string) (string, bool) {
	data := taskTopicCallbackPrefix + taskID
	if !taskIDRe.MatchString(taskID) || len(data) > MaxCallbackData {
		return "", false
	}
	return data, true
}

// parseTaskTopicCallback returns the task ID of "tt:<task id>" data.
func parseTaskTopicCallback(data string) (string, bool) {
	id, ok := strings.CutPrefix(data, taskTopicCallbackPrefix)
	if !ok || len(data) > MaxCallbackData || !taskIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// withDataButton returns m with the callback button as one more row. A
// message that opens a reply box (force reply cannot carry a keyboard), or
// that already has a button with the same data, is unchanged.
func withDataButton(m OutMessage, b Button) OutMessage {
	if m.ForceReply || b.Data == "" {
		return m
	}
	for _, row := range m.Buttons {
		for _, have := range row {
			if have.Data == b.Data {
				return m
			}
		}
	}
	rows := make([][]Button, 0, len(m.Buttons)+1)
	rows = append(rows, m.Buttons...)
	m.Buttons = append(rows, []Button{b})
	return m
}

// taskTopicCallback answers a press on "📌 Mở topic task": it creates the
// task's topic or reuses it (the topic store gives one subject one topic,
// so two presses never make two) and replies, where the pressed message
// is, with the "Mở topic" button.
func (w *InboxWorker) taskTopicCallback(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	if qid, _ := callbackArgs(u.Args); qid == "" {
		return outcome{code: "bad_callback"}
	}
	taskID, ok := parseTaskTopicCallback(u.Command)
	if !ok || u.Note != "" {
		return w.answerReportCallback(ctx, to, u, msgBadButton, NoteBadCallback)
	}
	answered := w.answerReportCallback(ctx, to, u, msgTaskTopicOpening, "")
	out := w.topicJumpRef(ctx, u, to, TaskRef{ID: taskID})
	return outcome{code: firstCode(out.code, answered.code)}
}

// replyTaskID returns the ClickUp task of the hub message u replies to.
func (w *InboxWorker) replyTaskID(ctx context.Context, u InboundUpdate) (string, bool) {
	if u.ReplyToMessageID <= 0 {
		return "", false
	}
	return MessageTaskID(ctx, w.d.Conversations, u.WorkspaceID, u.IntegrationID, u.ChatID, u.ReplyToMessageID)
}

// MessageTaskID returns the ClickUp task of a Telegram message bound to
// workflow executions (for example a notice): the executions bound to the
// message must name exactly one task. ok is false otherwise.
func MessageTaskID(ctx context.Context, store workflow.ConversationStore, workspaceID, integrationID string, chatID, messageID int64) (string, bool) {
	lister, ok := store.(workflow.ResourceLister)
	if !ok || messageID <= 0 || chatID == 0 || integrationID == "" {
		return "", false
	}
	ids, err := store.ExecutionsByResource(ctx, workspaceID, "telegram.message", workflow.TelegramMessageRef(integrationID, chatID, messageID))
	if err != nil || len(ids) == 0 || len(ids) > maxMessageExecutions {
		return "", false
	}
	task := ""
	for _, id := range ids {
		resources, err := lister.ListResources(ctx, workspaceID, id)
		if err != nil {
			return "", false
		}
		for _, res := range resources {
			if res.Kind != resourceClickUpTsk || !taskIDRe.MatchString(res.ExternalID) {
				continue
			}
			if task != "" && task != res.ExternalID {
				return "", false
			}
			task = res.ExternalID
		}
	}
	return task, task != ""
}

// ReplyTasks tells the Telegram handler whether a reply's target is a hub
// message about a ClickUp task, so the reply goes to the assistant (with
// that task as context) instead of "no open reply".
type ReplyTasks struct {
	Conversations workflow.ConversationStore
	// Links finds the messages that show a ClickUp comment; nil: none.
	Links CommentLinkReader
}

// ReplyHasTask reports whether the message names exactly one ClickUp task.
func (r ReplyTasks) ReplyHasTask(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) bool {
	_, ok := MessageTaskID(ctx, r.Conversations, workspaceID, integrationID, chatID, messageID)
	return ok
}

// ReplyHasComment reports whether the message shows a ClickUp comment the
// owner can answer. An error means the link could not be read.
func (r ReplyTasks) ReplyHasComment(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (bool, error) {
	_, ok, err := lookupReplyComment(ctx, r.Links, workspaceID, integrationID, chatID, messageID)
	return ok, err
}

// lookupReplyComment returns the usable comment link of a Telegram message.
func lookupReplyComment(ctx context.Context, links CommentLinkReader, workspaceID, integrationID string, chatID, messageID int64) (CommentLink, bool, error) {
	if links == nil || messageID <= 0 {
		return CommentLink{}, false, nil
	}
	link, err := links.LookupCommentLink(ctx, workspaceID, integrationID, chatID, messageID)
	switch {
	case errors.Is(err, ErrNotFound):
		return CommentLink{}, false, nil
	case err != nil:
		return CommentLink{}, false, err
	}
	return link, replyableCommentLink(link), nil
}

// replyableCommentLink reports whether a draft can be threaded under l.
func replyableCommentLink(l CommentLink) bool {
	return ValidCommentLink(l) && draftCommentIDRe.MatchString(l.CommentID)
}

// replyCommentContext is the fixed owner-turn prefix for a reply to a
// message showing a ClickUp comment. It names only the task's identifier,
// never the comment's text.
func replyCommentContext(taskID string) string {
	return "[Context] This message replies to a ClickUp comment on task " + taskID +
		". To answer on ClickUp, call draft_clickup_comment; the hub threads the draft under that comment."
}

// replyTaskContext is the fixed owner-turn prefix for a reply to a hub
// message about a task. It names only the task's identifier, never the
// replied-to text.
func replyTaskContext(taskID string) string {
	return "[Context] This message replies to a hub message about ClickUp task " + taskID + "."
}

// replyTaskAssistant queues a reply to a hub message about a ClickUp task
// for the assistant with the task ID as context. ok is false (the normal
// rules apply) when the message names no single task or targets an
// active workflow.
func (w *InboxWorker) replyTaskAssistant(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr, text string) (outcome, bool) {
	taskID, ok := w.replyTaskID(ctx, u)
	if !ok {
		return outcome{}, false
	}
	id, resolved := w.resolveWorkflowTarget(ctx, u, 0)
	if resolved && !w.workflowTerminal(ctx, u.WorkspaceID, id) {
		return outcome{}, false
	}
	if !resolved {
		id = 0
	}
	return w.queueAssistantJob(ctx, assistantJob{u: u, to: to, actor: actor, corr: corr, text: text, workflowID: id, replyContext: replyTaskContext(taskID)}), true
}

// inGeneral reports whether u was written in General or in the command
// topic (telegram_command_topic), which works exactly like General.
func (w *InboxWorker) inGeneral(ctx context.Context, u InboundUpdate) bool {
	if u.ThreadID == 0 {
		return true
	}
	name := strings.TrimSpace(w.settings.CommandTopic)
	if name == "" {
		return false
	}
	t, ok := w.topicOf(ctx, u)
	return ok && t.SubjectKind == TopicSubjectFixed && t.SubjectKey == name
}
