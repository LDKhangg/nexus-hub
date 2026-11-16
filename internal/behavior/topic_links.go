package behavior

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Texts of the jump-link feature (owner-facing, Vietnamese like the rest).
const (
	btnOpenTopic      = "Mở topic"
	msgTopicUsage     = "Cú pháp: /topic <task id hoặc link ClickUp>"
	msgTopicUnread    = "Không đọc được task này."
	msgTopicNoLink    = "Không tạo được liên kết tới topic (nhóm chưa bật Topics hoặc bot thiếu quyền)."
	msgTopicNotAvail  = "Chưa bật tính năng topic."
	maxListJumpRows   = 10
	telegramLinkChatN = int64(1_000_000_000_000)
)

var (
	// ErrTaskUnreadable means the workspace's ClickUp source cannot read
	// the task, so no topic is made for it.
	ErrTaskUnreadable = errors.New("task is not readable")
	// ErrTopicUnavailable means the task's topic could not be created or
	// has no deep link (not a forum, missing right, not a supergroup id).
	ErrTopicUnavailable = errors.New("topic is not available")
)

// taskURLRef returns the task id of text when it is exactly one ClickUp
// task URL (free text with more words is a normal message).
func taskURLRef(text string) (string, bool) {
	ref, ok := taskURLTaskRef(text)
	return ref.ID, ok
}

// taskURLTaskRef is taskURLRef that keeps the URL's team ID.
func taskURLTaskRef(text string) (TaskRef, bool) {
	text = strings.Trim(strings.TrimSpace(text), "<>")
	if !strings.HasPrefix(text, "https://") || strings.ContainsAny(text, " \t\r\n") {
		return TaskRef{}, false
	}
	return ParseTaskRefTeam(text)
}

// TaskResolver turns a task reference into the canonical task ID. Custom
// IDs are read from ClickUp; a canonical ID is returned unchanged.
type TaskResolver interface {
	// ResolveTask returns the canonical ID, or ErrTaskUnreadable.
	ResolveTask(ctx context.Context, workspaceID string, ref TaskRef) (string, error)
}

var _ TaskResolver = (*TopicRouter)(nil)

// ResolveTask implements TaskResolver. Without a resolver wired, the ID is
// used as written.
func (r *TopicRouter) ResolveTask(ctx context.Context, workspaceID string, ref TaskRef) (string, error) {
	if r == nil || !taskIDRe.MatchString(ref.ID) {
		return "", ErrTaskUnreadable
	}
	if !ref.Custom() || r.d.ResolveTask == nil {
		return ref.ID, nil
	}
	nctx, cancel := context.WithTimeout(ctx, taskNameTimeout)
	defer cancel()
	id, err := r.d.ResolveTask(nctx, workspaceID, ref)
	if err != nil || !taskIDRe.MatchString(id) {
		return "", ErrTaskUnreadable
	}
	return id, nil
}

// resolveTask returns the canonical ID of ref through the Links value.
func (w *InboxWorker) resolveTask(ctx context.Context, workspaceID string, ref TaskRef) (string, error) {
	if !ref.Custom() {
		return ref.ID, nil
	}
	rs, ok := w.d.Links.(TaskResolver)
	if !ok {
		return ref.ID, nil
	}
	return rs.ResolveTask(ctx, workspaceID, ref)
}

// TopicDeepLink is the t.me link of a topic of a supergroup: chat ids of
// supergroups are -100<n>. ok is false for any other chat id.
func TopicDeepLink(chatID, threadID int64) (string, bool) {
	if threadID <= 0 || chatID >= -telegramLinkChatN {
		return "", false
	}
	return "https://t.me/c/" + strconv.FormatInt(-chatID-telegramLinkChatN, 10) + "/" + strconv.FormatInt(threadID, 10), true
}

// TopicLinks gives the inbox worker jump links. A nil value turns the
// feature off.
type TopicLinks interface {
	// TaskTopicLink returns the link to the task's topic in the chat of
	// base, creating or reopening the topic. ErrTaskUnreadable when the
	// task cannot be read, ErrTopicUnavailable when no link can be made.
	TaskTopicLink(ctx context.Context, base ChatRef, taskID string) (string, error)
	// ExistingTopicLink returns the link of a topic that already exists.
	ExistingTopicLink(ctx context.Context, base ChatRef, kind, key string) (string, bool)
}

var _ TopicLinks = (*TopicRouter)(nil)

// TaskTopicLink implements TopicLinks.
func (r *TopicRouter) TaskTopicLink(ctx context.Context, base ChatRef, taskID string) (string, error) {
	if r == nil || !taskIDRe.MatchString(taskID) {
		return "", ErrTopicUnavailable
	}
	base.ThreadID, base.topic = 0, nil
	if r.d.TaskName != nil {
		nctx, cancel := context.WithTimeout(ctx, taskNameTimeout)
		_, err := r.d.TaskName(nctx, base.WorkspaceID, "", taskID)
		cancel()
		if err != nil {
			return "", ErrTaskUnreadable
		}
	}
	dest := r.Destination(ctx, base, TopicIntent{Mode: ReportTopicTask, TaskID: taskID})
	link, ok := TopicDeepLink(dest.ChatID, dest.ThreadID)
	if !ok {
		return "", ErrTopicUnavailable
	}
	return link, nil
}

// ExistingTopicLink implements TopicLinks.
func (r *TopicRouter) ExistingTopicLink(ctx context.Context, base ChatRef, kind, key string) (string, bool) {
	if r == nil || r.d.Store == nil || kind == "" {
		return "", false
	}
	t, err := r.d.Store.TopicBySubject(ctx, TopicKey{
		WorkspaceID: base.WorkspaceID, IntegrationID: base.IntegrationID, ChatID: base.ChatID, SubjectKind: kind, SubjectKey: key,
	})
	if err != nil || t.ThreadID <= 0 {
		return "", false
	}
	return TopicDeepLink(base.ChatID, t.ThreadID)
}

// JumpButton returns the "Mở topic" button for a message of in that went to
// General although its topic exists.
func (r *TopicRouter) JumpButton(ctx context.Context, base ChatRef, in TopicIntent) (Button, bool) {
	if r == nil {
		return Button{}, false
	}
	kind, key := r.subject(ctx, base.WorkspaceID, in)
	link, ok := r.ExistingTopicLink(ctx, base, kind, key)
	if !ok {
		return Button{}, false
	}
	return Button{Text: btnOpenTopic, URL: link}, true
}

// withJumpButton returns m with the button as one more row; a message that
// opens a reply box (force reply cannot carry a keyboard) is unchanged.
func withJumpButton(m OutMessage, b Button) OutMessage {
	if m.ForceReply {
		return m
	}
	for _, row := range m.Buttons {
		for _, have := range row {
			if have.URL == b.URL {
				return m
			}
		}
	}
	rows := make([][]Button, 0, len(m.Buttons)+1)
	rows = append(rows, m.Buttons...)
	m.Buttons = append(rows, []Button{b})
	return m
}

// topicCommand answers "/topic <id or link>".
func (w *InboxWorker) topicCommand(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	ref, ok := ParseTaskRefTeam(u.Args)
	if !ok {
		return w.reply(ctx, to, msgTopicUsage, "usage")
	}
	return w.topicJumpRef(ctx, u, to, ref)
}

// jumpAvailable reports whether a lone task link may take the jump path: the
// chat is a supergroup (a topic link can exist) and the router does not
// remember it as a non-forum chat. Otherwise the text goes to the assistant.
func (w *InboxWorker) jumpAvailable(u InboundUpdate) bool {
	if _, ok := TopicDeepLink(u.ChatID, 1); !ok {
		return false
	}
	if nf, ok := w.d.Links.(interface{ ChatNonForum(ChatRef) bool }); ok {
		return !nf.ChatNonForum(ChatRef{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID})
	}
	return true
}

// topicJump replies with the "Mở topic" button of the task's topic. It is
// for General only: inside a topic the owner is already in one.
func (w *InboxWorker) topicJump(ctx context.Context, u InboundUpdate, to ChatRef, taskID string) outcome {
	return w.topicJumpRef(ctx, u, to, TaskRef{ID: taskID})
}

// topicJumpRef is topicJump for a reference that may be a custom task ID;
// the topic is always keyed by the canonical ID.
func (w *InboxWorker) topicJumpRef(ctx context.Context, u InboundUpdate, to ChatRef, ref TaskRef) outcome {
	if w.d.Links == nil {
		return w.reply(ctx, to, msgTopicNotAvail, "not_available")
	}
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	taskID, err := w.resolveTask(hctx, u.WorkspaceID, ref)
	var link string
	if err == nil {
		link, err = w.d.Links.TaskTopicLink(hctx, ChatRef{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID}, taskID)
	}
	cancel()
	switch {
	case errors.Is(err, ErrTaskUnreadable):
		return w.reply(ctx, to, msgTopicUnread, "task_unreadable")
	case err != nil:
		return w.reply(ctx, to, msgTopicNoLink, "topic_unavailable")
	}
	return w.replyMessage(ctx, to, OutMessage{
		Text:    "Task " + taskID + ": nhấn nút để mở topic.",
		Buttons: [][]Button{{{Text: btnOpenTopic, URL: link}}},
	}, "")
}

// replyMessage is reply for a message with buttons.
func (w *InboxWorker) replyMessage(ctx context.Context, to ChatRef, m OutMessage, code string) outcome {
	if w.d.Messenger == nil {
		return outcome{code: firstCode(code, "reply_failed")}
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	defer cancel()
	id, err := w.d.Messenger.SendMessage(sendCtx, to, m)
	if err != nil {
		return outcome{code: firstCode(code, "reply_failed")}
	}
	w.recordOutboundConversation(ctx, to, id, 0)
	return outcome{code: code}
}

// listJumpRows returns one "WF-n · Mở topic" row for each listed execution
// whose ClickUp task has a topic in this chat (at most maxListJumpRows, one
// per topic).
func (w *InboxWorker) listJumpRows(ctx context.Context, u InboundUpdate, ids []int64) [][]Button {
	lister, ok := w.d.Conversations.(workflow.ResourceLister)
	if w.d.Links == nil || !ok {
		return nil
	}
	base := ChatRef{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID}
	seen := map[string]bool{}
	var rows [][]Button
	for _, id := range ids {
		if len(rows) == maxListJumpRows {
			break
		}
		resources, err := lister.ListResources(ctx, u.WorkspaceID, id)
		if err != nil {
			continue
		}
		for _, res := range resources {
			if res.Kind != resourceClickUpTsk || res.ExternalID == "" || !taskIDRe.MatchString(res.ExternalID) || seen[res.ExternalID] {
				continue
			}
			link, ok := w.d.Links.ExistingTopicLink(ctx, base, TopicSubjectClickUpTask, res.ExternalID)
			if !ok {
				continue
			}
			seen[res.ExternalID] = true
			rows = append(rows, []Button{{Text: workflow.FormatRef(id) + " · " + btnOpenTopic, URL: link}})
			break
		}
	}
	return rows
}
