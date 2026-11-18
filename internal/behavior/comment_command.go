package behavior

import (
	"context"
	"strings"
)

// commentInstruction is the fixed owner-turn line of /comment: the text
// below it is what the owner wants said on the task.
const commentInstruction = "[Instruction] The owner used /comment: draft a ClickUp comment on this task from the message below " +
	"with draft_clickup_comment. Only draft; the owner approves the preview."

// commentCommand handles "/comment <text>" (Phase 10, R-B-3). In a forum
// topic bound to a ClickUp task it always starts the draft flow for that
// task: the assistant gets the text with a fixed instruction to call
// draft_clickup_comment. It never becomes a worker note or a clarification,
// whatever executions are active. A reply to a message showing a ClickUp
// comment of the same task is threaded under that comment by the draft tool.
// Without text, or outside a task topic, it answers usageComment. The text is
// taken from the in-memory stage exactly once and never logged.
func (w *InboxWorker) commentCommand(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	a := w.d.Assistant
	if a == nil || a.Models == nil {
		w.dropStagedComment(u)
		return w.reply(ctx, to, msgHint, "")
	}
	args := strings.TrimSpace(u.Args)
	if args == "" {
		return w.reply(ctx, to, usageComment, "usage")
	}
	// Lookups run before the text is taken, so a retry still has it.
	ref, inTopic, err := w.topicTaskRefChecked(ctx, u)
	if err == nil && inTopic {
		var link CommentLink
		var linked bool
		link, linked, err = lookupReplyComment(ctx, w.d.CommentLinks, u.WorkspaceID, u.IntegrationID, u.ChatID, u.ReplyToMessageID)
		if err == nil {
			text, ok := w.takeCommentText(u, args)
			if !ok {
				return w.reply(ctx, to, msgAssistantGone, "assistant_text_gone")
			}
			if strings.TrimSpace(text) == "" {
				return w.reply(ctx, to, usageComment, "usage")
			}
			if !w.draftToolAvailable() {
				return w.reply(ctx, to, msgNotAvailable, "not_available")
			}
			job := assistantJob{u: u, to: to, actor: actor, corr: corr, text: text,
				topicContext: topicTarget{kind: TopicSubjectClickUpTask, key: ref.ID}.contextLine() + "\n" + commentInstruction}
			if linked && link.TaskID == ref.ID {
				job.replyContext = replyCommentContext(link.TaskID)
			}
			return w.queueAssistantJob(ctx, job)
		}
	}
	if err != nil {
		if u.Attempts < w.settings.MaxAttempts {
			return outcome{code: "storage", retry: true}
		}
		w.dropStagedComment(u)
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	// General, a fixed topic or an unknown thread: no assistant turn.
	w.dropStagedComment(u)
	return w.reply(ctx, to, usageComment, "usage")
}

// takeCommentText returns the /comment text: the staged text when Args holds
// a stage key (taken exactly once), else Args itself.
func (w *InboxWorker) takeCommentText(u InboundUpdate, args string) (string, bool) {
	key, staged := strings.CutPrefix(args, AssistantTextPrefix)
	if !staged {
		return args, true
	}
	a := w.d.Assistant
	if a == nil || a.Inbox == nil {
		return "", false
	}
	return a.Inbox.take(u.WorkspaceID, key, w.now())
}

// dropStagedComment discards a staged /comment text that will not be used.
func (w *InboxWorker) dropStagedComment(u InboundUpdate) {
	key, staged := strings.CutPrefix(strings.TrimSpace(u.Args), AssistantTextPrefix)
	if !staged || w.d.Assistant == nil || w.d.Assistant.Inbox == nil {
		return
	}
	w.d.Assistant.Inbox.take(u.WorkspaceID, key, w.now())
}
