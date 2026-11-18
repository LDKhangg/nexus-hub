package behavior

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// DefaultMessageLinkRetention is how long a Telegram message to ClickUp
// comment link is kept when the setting is not set.
const DefaultMessageLinkRetention = 30 * 24 * time.Hour

// CommentLink is the ClickUp comment a Telegram message shows. Ids only.
type CommentLink struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	MessageID     int64
	TaskID        string
	CommentID     string
}

// CommentLinkStore records and reads Telegram message to ClickUp comment
// links. Every call is scoped by workspace, integration and chat. A
// TopicStore that implements it lets the router record the links of the
// messages it delivers.
type CommentLinkStore interface {
	// RecordCommentLink stores l; a second record of the same message keeps
	// the first.
	RecordCommentLink(ctx context.Context, l CommentLink) error
	// LookupCommentLink returns the link of one Telegram message;
	// ErrNotFound when the message shows no known comment.
	LookupCommentLink(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (CommentLink, error)
}

// MessageLinkPruner deletes message links created before a time.
type MessageLinkPruner interface {
	PruneMessageLinks(ctx context.Context, before time.Time) (int64, error)
}

// ValidCommentLink reports whether l carries usable ids: a workspace,
// integration, chat, positive message id, a task id and a comment id in
// the plain identifier shape.
func ValidCommentLink(l CommentLink) bool {
	return l.WorkspaceID != "" && l.IntegrationID != "" && l.ChatID != 0 && l.MessageID > 0 &&
		taskIDRe.MatchString(l.TaskID) && taskIDRe.MatchString(l.CommentID)
}

// CommentIDFromMessageRef returns the ClickUp comment id of a ClickUp
// event's ExternalMessageID ("comment:<id>"); ok is false for any other
// event (status, assignee) or a malformed id.
func CommentIDFromMessageRef(ref string) (string, bool) {
	id, ok := strings.CutPrefix(ref, "comment:")
	if !ok || !taskIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// DestinationResolver finds where a ClickUp task's messages go: the task's
// existing clickup_task topic in the chat, or General. The notice delivery
// (relay.NoticeRouter) and the behavior report path (TopicRouter.Destination
// for report topic "general") both use it; the TopicRouter implements it.
type DestinationResolver interface {
	ForClickUpTask(ctx context.Context, base ChatRef, taskID string) (ChatRef, bool)
}

var (
	_ DestinationResolver = (*TopicRouter)(nil)
	_ relay.NoticeRouter  = (*TopicRouter)(nil)
)

// ForClickUpTask returns base with the thread of the ClickUp task's
// existing topic in base's chat, reopening a closed topic through the
// same path as any other send (the lifecycle is told). It never creates a
// topic and never picks another subject's topic. ok is false, and base
// (General) is returned, when the task has no created topic, the chat is
// remembered as not a forum, or the reopen failed; a topic Telegram no
// longer knows is forgotten.
func (r *TopicRouter) ForClickUpTask(ctx context.Context, base ChatRef, taskID string) (ChatRef, bool) {
	base.topic = nil
	if r == nil || r.d.Store == nil || base.ChatID == 0 || base.ThreadID != 0 || !taskIDRe.MatchString(taskID) {
		return base, false
	}
	if _, blocked := r.blockedCode(base); blocked {
		return base, false
	}
	topic, err := r.d.Store.TopicBySubject(ctx, TopicKey{
		WorkspaceID: base.WorkspaceID, IntegrationID: base.IntegrationID, ChatID: base.ChatID,
		SubjectKind: TopicSubjectClickUpTask, SubjectKey: taskID,
	})
	if err != nil || topic.ThreadID <= 0 || topic.SubjectKind != TopicSubjectClickUpTask || topic.SubjectKey != taskID ||
		(topic.WorkspaceID != "" && !strings.EqualFold(topic.WorkspaceID, base.WorkspaceID)) ||
		(topic.IntegrationID != "" && !strings.EqualFold(topic.IntegrationID, base.IntegrationID)) ||
		(topic.ChatID != 0 && topic.ChatID != base.ChatID) {
		return base, false
	}
	if topic.State == TopicClosed {
		if r.d.Client == nil {
			r.fallback("topic_closed")
			return base, false
		}
		if err := r.d.Client.ReopenTopic(ctx, base, topic.ThreadID); err != nil {
			code := sendCode(err)
			if code == topicCodeNotFound {
				r.drop(ctx, base, topic)
			}
			r.block(base, code)
			r.fallback(code)
			return base, false
		}
		r.unblock(base)
		if _, err := r.d.Store.SetTopicState(ctx, base.WorkspaceID, topic.ID, TopicOpen); err != nil {
			log.Print("telegram topic: reopened, state not stored")
		}
		r.taskTopicOpened(base.WorkspaceID, TopicSubjectClickUpTask, taskID)
	}
	base.ThreadID = topic.ThreadID
	return base, true
}

// NoticeThread returns the thread of the ClickUp task's existing topic in
// the chat, or 0 (General). It satisfies relay.NoticeRouter.
func (r *TopicRouter) NoticeThread(ctx context.Context, workspaceID, integrationID string, chatID int64, taskID string) int64 {
	to, ok := r.ForClickUpTask(ctx, ChatRef{WorkspaceID: workspaceID, IntegrationID: integrationID, ChatID: chatID}, taskID)
	if !ok {
		return 0
	}
	return to.ThreadID
}

// NoticeThreadRejected handles a notice send the topic made fail: it
// returns the thread to resend to (the reopened topic, or 0 for General)
// and whether to resend once.
func (r *TopicRouter) NoticeThreadRejected(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64, err error) (int64, bool) {
	to, ok := r.Recover(ctx, ChatRef{WorkspaceID: workspaceID, IntegrationID: integrationID, ChatID: chatID, ThreadID: threadID}, err)
	return to.ThreadID, ok
}

// NoticeButton returns the "📌 Mở topic task" button of a notice about
// taskID that goes to General.
func (r *TopicRouter) NoticeButton(taskID string) (relay.NoticeButton, bool) {
	if r == nil {
		return relay.NoticeButton{}, false
	}
	data, ok := taskTopicCallbackData(taskID)
	if !ok {
		return relay.NoticeButton{}, false
	}
	return relay.NoticeButton{Text: btnOpenTaskTopic, Data: data}, true
}

// generalTaskDestination routes a message whose report topic is "general"
// into the subject task's existing topic (REQ-3). ok is false when the
// run has no ClickUp task or the task has no topic: the message stays in
// General.
func (r *TopicRouter) generalTaskDestination(ctx context.Context, base ChatRef, in TopicIntent) (ChatRef, bool) {
	if strings.TrimSpace(in.Mode) != ReportTopicGeneral {
		return base, false
	}
	taskID := r.intentTaskID(ctx, base.WorkspaceID, in)
	if taskID == "" {
		return base, false
	}
	return r.ForClickUpTask(ctx, base, taskID)
}

// recordCommentLink stores the comment a delivered message shows, when the
// store keeps links. Failures are logged without ids or text.
func (r *TopicRouter) recordCommentLink(ctx context.Context, to ChatRef, messageID int64, taskID, commentID string) {
	if r == nil || r.d.Store == nil {
		return
	}
	links, ok := r.d.Store.(CommentLinkStore)
	if !ok {
		return
	}
	l := CommentLink{WorkspaceID: to.WorkspaceID, IntegrationID: to.IntegrationID, ChatID: to.ChatID, MessageID: messageID, TaskID: taskID, CommentID: commentID}
	if !ValidCommentLink(l) {
		return
	}
	if err := links.RecordCommentLink(context.WithoutCancel(ctx), l); err != nil {
		log.Print("telegram message link: not stored")
	}
}

// LookupCommentLink returns the ClickUp comment a Telegram message of the
// chat shows; ErrNotFound when none is known.
func (r *TopicRouter) LookupCommentLink(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (CommentLink, error) {
	if r == nil || r.d.Store == nil {
		return CommentLink{}, ErrNotFound
	}
	links, ok := r.d.Store.(CommentLinkStore)
	if !ok {
		return CommentLink{}, ErrNotFound
	}
	return links.LookupCommentLink(ctx, workspaceID, integrationID, chatID, messageID)
}
