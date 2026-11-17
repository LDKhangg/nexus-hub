package behavior

import (
	"context"
	"log"
	"strings"
	"unicode"
)

// Owner-facing text of the pinned task link (Vietnamese, like the rest).
const taskLinkButton = "Mở task ClickUp"

const (
	// clickUpTaskURLPrefix is the only link a task link button may open.
	clickUpTaskURLPrefix = "https://app.clickup.com/"
	maxTaskLinkURLBytes  = 512
	maxTaskLinkTextBytes = 512
	topicCodeNotModified = "topic_not_modified"
)

// TaskTopicInfo is what a ClickUp task topic shows: the task title, its
// custom id (empty when it has none) and its ClickUp URL. It is used to
// build the topic name and the pinned link; it is never stored or logged.
type TaskTopicInfo struct {
	Title    string
	CustomID string
	URL      string
}

// TopicTaskInfo reads a task's TaskTopicInfo.
type TopicTaskInfo func(ctx context.Context, workspaceID, integrationID, taskID string) (TaskTopicInfo, error)

// TopicLinkStore is implemented by a TopicStore that records the pinned task
// link message, so a topic gets it at most once. ClaimTopicLink returns true
// to one caller only; ReleaseTopicLink gives the claim back after a send
// that surely did not happen.
type TopicLinkStore interface {
	ClaimTopicLink(ctx context.Context, workspaceID string, id int64) (bool, error)
	SetTopicLink(ctx context.Context, workspaceID string, id, messageID int64) error
	ReleaseTopicLink(ctx context.Context, workspaceID string, id int64) error
}

// TopicLinkPoster is implemented by a TopicClient that can post and pin the
// task link message in a topic (the Telegram Messenger).
type TopicLinkPoster interface {
	SendMessage(ctx context.Context, to ChatRef, m OutMessage) (int64, error)
	PinMessage(ctx context.Context, to ChatRef, messageID int64) error
}

// TopicRenamer is implemented by a TopicClient that can rename a topic.
// changed is false when the topic already had that name.
type TopicRenamer interface {
	RenameTopic(ctx context.Context, to ChatRef, threadID int64, name string) (changed bool, err error)
}

// taskInfo reads the task shown by a ClickUp task topic; ok is false when
// it cannot be read.
func (r *TopicRouter) taskInfo(ctx context.Context, workspaceID, integrationID, taskID string) (TaskTopicInfo, bool) {
	nctx, cancel := context.WithTimeout(ctx, taskNameTimeout)
	defer cancel()
	switch {
	case r.d.TaskInfo != nil:
		info, err := r.d.TaskInfo(nctx, workspaceID, integrationID, taskID)
		return info, err == nil
	case r.d.TaskName != nil:
		title, err := r.d.TaskName(nctx, workspaceID, integrationID, taskID)
		return TaskTopicInfo{Title: title}, err == nil
	}
	return TaskTopicInfo{}, false
}

// taskLabel is the custom id of a readable task when it has a valid one,
// else the task id.
func taskLabel(key string, info TaskTopicInfo, ok bool) string {
	if ok {
		if c := strings.TrimSpace(info.CustomID); taskIDRe.MatchString(c) {
			return c
		}
	}
	return key
}

// taskTitled joins the label and the title of a readable task, sanitized
// and cut to max bytes.
func taskTitled(key string, info TaskTopicInfo, ok bool, max int) string {
	label := taskLabel(key, info, ok)
	if ok {
		if t := sanitizeTopicName(info.Title, max); t != "" {
			return sanitizeTopicName(label+" · "+t, max)
		}
	}
	return sanitizeTopicName(label, max)
}

// TaskTopicName is the name of a ClickUp task topic: "<custom id> · <title>"
// when the task has a custom id, "<task id> · <title>" without one, and the
// task id alone when the task could not be read (ok false). At most
// MaxTopicNameBytes, cut on a rune boundary.
func TaskTopicName(taskID string, info TaskTopicInfo, ok bool) string {
	return taskTitled(taskID, info, ok, MaxTopicNameBytes)
}

// taskLinkURL returns the task URL ClickUp gave when it is a ClickUp link,
// else the canonical https://app.clickup.com/t/<task id>.
func taskLinkURL(taskID string, info TaskTopicInfo, ok bool) string {
	u := info.URL
	if ok && strings.HasPrefix(u, clickUpTaskURLPrefix) && len(u) <= maxTaskLinkURLBytes &&
		!strings.ContainsFunc(u, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) || c > unicode.MaxASCII }) {
		return u
	}
	return clickUpTaskURLPrefix + "t/" + taskID
}

// postTaskLink posts the task link message in a ClickUp task topic and pins
// it, once per topic. posted is true when the message was sent; failed is
// true when the send failed. A pin failure is logged and not retried.
func (r *TopicRouter) postTaskLink(ctx context.Context, chat ChatRef, topicID int64, taskID string, info TaskTopicInfo, ok bool) (posted, failed bool) {
	p, can := r.d.Client.(TopicLinkPoster)
	if !can || chat.ThreadID <= 0 || !taskIDRe.MatchString(taskID) {
		return false, false
	}
	ctx = context.WithoutCancel(ctx)
	links, hasStore := r.d.Store.(TopicLinkStore)
	if hasStore {
		claimed, err := links.ClaimTopicLink(ctx, chat.WorkspaceID, topicID)
		if err != nil {
			log.Print("telegram topic: task link not claimed")
			return false, true
		}
		if !claimed {
			return false, false
		}
	}
	chat.topic = nil
	msg := OutMessage{
		Text:    taskTitled(taskID, info, ok, maxTaskLinkTextBytes),
		Buttons: [][]Button{{{Text: taskLinkButton, URL: taskLinkURL(taskID, info, ok)}}},
	}
	id, err := p.SendMessage(ctx, chat, msg)
	if err != nil || id <= 0 {
		// An unclear outcome may have posted it: keep the claim.
		if hasStore && err != nil && !unclearOutcome(err) {
			if rerr := links.ReleaseTopicLink(ctx, chat.WorkspaceID, topicID); rerr != nil {
				log.Print("telegram topic: task link claim not released")
			}
		}
		log.Printf("telegram topic: task link not posted (%s)", sendCode(err))
		return false, true
	}
	if hasStore {
		if err := links.SetTopicLink(ctx, chat.WorkspaceID, topicID, id); err != nil {
			log.Print("telegram topic: task link posted, not recorded")
		}
	}
	if err := p.PinMessage(ctx, chat, id); err != nil {
		log.Printf("telegram topic: task link not pinned (%s)", sendCode(err))
	}
	return true, false
}

// TopicRefreshCounts are the results of RefreshTaskTopics: counts only.
type TopicRefreshCounts struct {
	Renamed int `json:"renamed"`
	Linked  int `json:"linked"`
	Failed  int `json:"failed"`
}

// RefreshTaskTopics renames every ClickUp task topic of the workspace that
// has a thread to its TaskTopicName and posts and pins the task link where
// it was never posted (open topics only). A topic already named so is not
// counted; a task that cannot be read is neither renamed nor linked and
// counts as failed.
// Running it again changes nothing new.
func (r *TopicRouter) RefreshTaskTopics(ctx context.Context, workspaceID string) (TopicRefreshCounts, error) {
	var n TopicRefreshCounts
	if r == nil || r.d.Store == nil {
		return n, ErrNotAvailable
	}
	topics, err := r.d.Store.ListTopics(ctx, workspaceID)
	if err != nil {
		return n, err
	}
	renamer, canRename := r.d.Client.(TopicRenamer)
	for _, t := range topics {
		if t.SubjectKind != TopicSubjectClickUpTask || t.ThreadID <= 0 || !taskIDRe.MatchString(t.SubjectKey) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		chat := ChatRef{WorkspaceID: workspaceID, IntegrationID: t.IntegrationID, ChatID: t.ChatID, ThreadID: t.ThreadID}
		info, ok := r.taskInfo(ctx, workspaceID, "", t.SubjectKey)
		failed := false
		switch {
		case !ok:
			failed = true
		case canRename:
			changed, err := renamer.RenameTopic(ctx, chat, t.ThreadID, TaskTopicName(t.SubjectKey, info, ok))
			switch {
			case err != nil && sendCode(err) == topicCodeNotModified:
			case err != nil:
				failed = true
			case changed:
				n.Renamed++
			}
		}
		if t.State == TopicOpen && ok {
			if _, isStore := r.d.Store.(TopicLinkStore); isStore {
				posted, lf := r.postTaskLink(ctx, chat, t.ID, t.SubjectKey, info, ok)
				if posted {
					n.Linked++
				}
				failed = failed || lf
			}
		}
		if failed {
			n.Failed++
		}
	}
	return n, nil
}
