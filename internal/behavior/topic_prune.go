package behavior

import (
	"context"
	"errors"
	"strconv"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TopicRemover deletes a forum topic in Telegram (the Messenger).
type TopicRemover interface {
	DeleteTopic(ctx context.Context, to ChatRef, threadID int64) error
}

// ExecutionStateReader reads one execution of a workspace.
type ExecutionStateReader interface {
	GetExecutionDetail(ctx context.Context, workspaceID string, executionID int64) (workflow.ExecutionDetail, error)
}

// TopicPruneCounts are the results of PruneWorkflowTopics: counts only.
// Kept counts the workflow topics left in place (active, unknown or only
// reserved executions); task and fixed topics are not counted.
type TopicPruneCounts struct {
	Deleted int `json:"deleted"`
	Kept    int `json:"kept"`
	Failed  int `json:"failed"`
}

// PruneWorkflowTopics deletes, in Telegram and in the store, every
// workflow topic of the workspace whose execution is terminal. Task and
// fixed topics and topics of executions that are active, unknown or
// unreadable are never touched. A topic already gone in Telegram still has
// its row removed. Running it again changes nothing new.
func PruneWorkflowTopics(ctx context.Context, workspaceID string, store TopicStore, client TopicRemover, executions ExecutionStateReader) (TopicPruneCounts, error) {
	var n TopicPruneCounts
	deleter, ok := store.(topicDeleter)
	if store == nil || !ok || client == nil || executions == nil {
		return n, ErrNotAvailable
	}
	topics, err := store.ListTopics(ctx, workspaceID)
	if err != nil {
		return n, err
	}
	for _, t := range topics {
		if t.SubjectKind != TopicSubjectWorkflow {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		id, perr := strconv.ParseInt(t.SubjectKey, 10, 64)
		if t.ThreadID <= 0 || perr != nil || id <= 0 {
			n.Kept++
			continue
		}
		d, err := executions.GetExecutionDetail(ctx, workspaceID, id)
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			n.Kept++
			continue
		case err != nil:
			n.Failed++
			continue
		case !d.State.IsTerminal():
			n.Kept++
			continue
		}
		chat := ChatRef{WorkspaceID: workspaceID, IntegrationID: t.IntegrationID, ChatID: t.ChatID}
		if err := client.DeleteTopic(ctx, chat, t.ThreadID); err != nil && sendCode(err) != topicCodeNotFound {
			n.Failed++
			continue
		}
		if err := deleter.DeleteTopic(ctx, workspaceID, t.ID); err != nil {
			n.Failed++
			continue
		}
		n.Deleted++
	}
	return n, nil
}
