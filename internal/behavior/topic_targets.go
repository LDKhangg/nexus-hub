package behavior

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// topicLookupLimit bounds the executions of one topic subject that are read
// to find its active ones.
const topicLookupLimit = 50

// topicListLimit bounds the executions /list shows for a task topic.
const topicListLimit = 20

// TopicLookup finds the stored topic of a Telegram thread. Every call is
// scoped by workspace, integration and chat; an unknown thread is
// ErrNotFound.
type TopicLookup interface {
	TopicByThread(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64) (Topic, error)
}

// topicTarget is the subject of the topic an owner message was written in.
// kind is "" outside a topic, in an unknown or foreign thread, and in a
// fixed topic: the Phase 6 rules apply then. active holds the subject's
// active executions, newest first; uncertain is set when a read failed or
// the lookup was capped, so a single candidate must still be confirmed.
type topicTarget struct {
	kind, key string
	active    []workflow.ExecutionDetail
	uncertain bool
}

func (t topicTarget) has() bool { return t.kind != "" }

// single returns the one active execution of the subject, only when it is
// certain that there is exactly one.
func (t topicTarget) single() (int64, bool) {
	if !t.has() || t.uncertain || len(t.active) != 1 {
		return 0, false
	}
	return t.active[0].ID, true
}

// contextLine is the fixed owner-turn prefix for an assistant turn in the
// topic. It names only the subject's identifier.
func (t topicTarget) contextLine() string {
	switch t.kind {
	case TopicSubjectClickUpTask:
		return "[Context] This message is in the topic of ClickUp task " + t.key + "."
	case TopicSubjectWorkflow:
		if id, err := strconv.ParseInt(t.key, 10, 64); err == nil && id > 0 {
			return "[Context] This message is in the topic of " + workflow.FormatRef(id) + "."
		}
	}
	return ""
}

// topicOf returns the stored topic of u's thread, or false. The store query
// is already scoped; a returned row that names another workspace,
// integration, chat or thread is ignored as well.
func (w *InboxWorker) topicOf(ctx context.Context, u InboundUpdate) (Topic, bool) {
	topic, ok, err := w.topicOfChecked(ctx, u)
	return topic, ok && err == nil
}

// topicOfChecked is topicOf that tells a thread with no topic (ok is false,
// no error) from a lookup that failed (an error).
func (w *InboxWorker) topicOfChecked(ctx context.Context, u InboundUpdate) (Topic, bool, error) {
	if u.ThreadID <= 0 || u.ChatID == 0 || w.d.Topics == nil {
		return Topic{}, false, nil
	}
	topic, err := w.d.Topics.TopicByThread(ctx, u.WorkspaceID, u.IntegrationID, u.ChatID, u.ThreadID)
	if errors.Is(err, ErrNotFound) {
		return Topic{}, false, nil
	}
	if err != nil {
		return Topic{}, false, err
	}
	if (topic.WorkspaceID != "" && !strings.EqualFold(topic.WorkspaceID, u.WorkspaceID)) ||
		(topic.IntegrationID != "" && !strings.EqualFold(topic.IntegrationID, u.IntegrationID)) ||
		(topic.ChatID != 0 && topic.ChatID != u.ChatID) || (topic.ThreadID != 0 && topic.ThreadID != u.ThreadID) {
		return Topic{}, false, nil
	}
	return topic, true, nil
}

// topicTarget resolves the subject of u's topic and its active executions in
// u's workspace.
func (w *InboxWorker) topicTarget(ctx context.Context, u InboundUpdate) topicTarget {
	r := w.workflowReader()
	if r == nil {
		return topicTarget{}
	}
	topic, ok := w.topicOf(ctx, u)
	if !ok {
		return topicTarget{}
	}
	key := strings.TrimSpace(topic.SubjectKey)
	switch topic.SubjectKind {
	case TopicSubjectClickUpTask:
		if !taskIDRe.MatchString(key) {
			return topicTarget{}
		}
		t := topicTarget{kind: TopicSubjectClickUpTask, key: key}
		items, uncertain := w.taskExecutions(ctx, u.WorkspaceID, key, topicLookupLimit, true)
		t.active, t.uncertain = items, uncertain
		return t
	case TopicSubjectWorkflow:
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 {
			return topicTarget{}
		}
		t := topicTarget{kind: TopicSubjectWorkflow, key: strconv.FormatInt(id, 10)}
		d, err := r.GetExecutionDetail(ctx, u.WorkspaceID, id)
		switch {
		case err == nil && !d.State.IsTerminal():
			t.active = []workflow.ExecutionDetail{d}
		case err != nil && !errors.Is(err, workflow.ErrNotFound):
			t.uncertain = true
		}
		return t
	}
	// A fixed topic (or an unknown kind) has no subject.
	return topicTarget{}
}

// taskExecutions returns the executions of this workspace bound to the
// ClickUp task, newest first, at most limit of them (only non-terminal
// ones when activeOnly). uncertain is true when a read failed or older
// bindings were not read.
func (w *InboxWorker) taskExecutions(ctx context.Context, ws, taskID string, limit int, activeOnly bool) ([]workflow.ExecutionDetail, bool) {
	r := w.workflowReader()
	if r == nil || w.d.Conversations == nil {
		return nil, true
	}
	ids, err := w.d.Conversations.ExecutionsByResource(ctx, ws, resourceClickUpTsk, taskID)
	if err != nil {
		return nil, true
	}
	seen := make(map[int64]bool, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] > unique[j] })
	uncertain := false
	if len(unique) > topicLookupLimit {
		unique, uncertain = unique[:topicLookupLimit], true
	}
	items := make([]workflow.ExecutionDetail, 0, min(len(unique), limit))
	for _, id := range unique {
		d, err := r.GetExecutionDetail(ctx, ws, id)
		if err != nil {
			if !errors.Is(err, workflow.ErrNotFound) {
				uncertain = true
			}
			continue
		}
		if activeOnly && d.State.IsTerminal() {
			continue
		}
		items = append(items, d)
		if len(items) == limit {
			break
		}
	}
	return items, uncertain
}

// queueAssistantTopic queues text for the assistant with the topic subject
// as context; the router is skipped like for a workflow target.
func (w *InboxWorker) queueAssistantTopic(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr, text string, t topicTarget) outcome {
	line := t.contextLine()
	if line == "" {
		return w.queueAssistantText(ctx, u, to, actor, corr, text)
	}
	return w.queueAssistantJob(ctx, assistantJob{u: u, to: to, actor: actor, corr: corr, text: text, topicContext: line})
}

// topicWorkflowList answers /list inside a task topic: the task's
// executions first, then a hint for the full list. ok is false outside a
// task topic.
func (w *InboxWorker) topicWorkflowList(ctx context.Context, u InboundUpdate, to ChatRef) (outcome, bool) {
	topic, ok := w.topicOf(ctx, u)
	if !ok || topic.SubjectKind != TopicSubjectClickUpTask || !taskIDRe.MatchString(strings.TrimSpace(topic.SubjectKey)) {
		return outcome{}, false
	}
	const hint = "Danh sách đầy đủ: gõ /list ở General."
	items, uncertain := w.taskExecutions(ctx, u.WorkspaceID, strings.TrimSpace(topic.SubjectKey), topicListLimit, false)
	if uncertain && len(items) == 0 {
		return w.reply(ctx, to, msgFailed, "handler_failed"), true
	}
	if len(items) == 0 {
		return w.reply(ctx, to, "Task này chưa có workflow.\n"+hint, ""), true
	}
	lines := make([]string, 0, len(items)+1)
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("%s · %s · %s", workflow.FormatRef(item.ID), oneLine(item.BehaviorName, 60), item.State))
	}
	lines = append(lines, hint)
	return w.reply(ctx, to, strings.Join(lines, "\n"), ""), true
}
