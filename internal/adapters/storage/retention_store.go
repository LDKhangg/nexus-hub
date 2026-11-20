package storage

import (
	"context"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionStore performs bounded retention work in short transactions.
type RetentionStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// NewRetentionStore wraps pool.
func NewRetentionStore(pool *pgxpool.Pool) (*RetentionStore, error) {
	if pool == nil {
		return nil, errors.New("retention store requires pool")
	}
	return &RetentionStore{pool: pool, q: sqlc.New(pool)}, nil
}

func retentionCutoff(now time.Time, days int64) pgtype.Timestamptz {
	if days == 0 {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: now.Add(-time.Duration(days) * 24 * time.Hour), Valid: true}
}

// CountRetention returns current safe candidates without deleting them.
func (s *RetentionStore) CountRetention(ctx context.Context, workspace string, rules behavior.RetentionRules, now time.Time) (map[string]int64, error) {
	ws, err := optionalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	row, err := s.q.CountRetentionCandidates(ctx, sqlc.CountRetentionCandidatesParams{
		MessageCutoff: retentionCutoff(now, rules.MessageLogsDays), WorkspaceID: ws,
		ToolCutoff: retentionCutoff(now, rules.ToolRunsDays), PolicyCutoff: retentionCutoff(now, rules.PolicyDecisionsDays),
		WorkflowCutoff: retentionCutoff(now, rules.WorkflowDays), OutboundCutoff: retentionCutoff(now, rules.OutboundDays),
	})
	if err != nil {
		return nil, err
	}
	return map[string]int64{
		"message_logs": row.MessageLogs, "tool_runs": row.ToolRuns, "policy_decisions": row.PolicyDecisions,
		"workflow_executions": row.WorkflowExecutions, "notification_outbox": row.NotificationOutbox,
		"outbound_actions": row.OutboundActions,
	}, nil
}

// SweepRetention drains each enabled category in bounded batches. A foreign-key
// failure stops only that category and is returned as table and SQLSTATE.
func (s *RetentionStore) SweepRetention(ctx context.Context, workspace string, rules behavior.RetentionRules, now time.Time, batch int32) (behavior.RetentionResult, error) {
	if batch <= 0 {
		batch = behavior.DefaultRetentionBatch
	}
	ws, err := optionalWorkspace(workspace)
	if err != nil {
		return behavior.RetentionResult{}, err
	}
	result := behavior.RetentionResult{Counts: map[string]int64{}}
	type deleteFn func(context.Context) (int64, error)
	run := func(table string, enabled bool, fn deleteFn) error {
		if !enabled {
			return nil
		}
		for {
			n, err := fn(ctx)
			if err != nil {
				if code := foreignKeyCode(err); code != "" {
					result.Issues = append(result.Issues, behavior.RetentionIssue{Table: table, Code: code})
					return nil
				}
				return err
			}
			result.Counts[table] += n
			if n < int64(batch) {
				return nil
			}
		}
	}
	outCutoff := retentionCutoff(now, rules.OutboundDays)
	if err := run("notification_outbox", rules.OutboundDays > 0, func(ctx context.Context) (int64, error) {
		return s.q.DeleteRetentionNotificationOutbox(ctx, sqlc.DeleteRetentionNotificationOutboxParams{Cutoff: outCutoff, WorkspaceID: ws, BatchSize: batch})
	}); err != nil {
		return result, err
	}
	if err := run("outbound_actions", rules.OutboundDays > 0, func(ctx context.Context) (int64, error) {
		return s.q.DeleteRetentionOutboundActions(ctx, sqlc.DeleteRetentionOutboundActionsParams{Cutoff: outCutoff, WorkspaceID: ws, BatchSize: batch})
	}); err != nil {
		return result, err
	}
	msgCutoff := retentionCutoff(now, rules.MessageLogsDays)
	if err := run("message_logs", rules.MessageLogsDays > 0, func(ctx context.Context) (int64, error) {
		return s.q.DeleteRetentionMessageLogs(ctx, sqlc.DeleteRetentionMessageLogsParams{Cutoff: msgCutoff, WorkspaceID: ws, BatchSize: batch})
	}); err != nil {
		return result, err
	}
	policyCutoff := retentionCutoff(now, rules.PolicyDecisionsDays)
	if err := run("policy_decisions", rules.PolicyDecisionsDays > 0, func(ctx context.Context) (int64, error) {
		return s.q.DeleteRetentionPolicyDecisions(ctx, sqlc.DeleteRetentionPolicyDecisionsParams{Cutoff: policyCutoff, WorkspaceID: ws, BatchSize: batch})
	}); err != nil {
		return result, err
	}
	toolCutoff := retentionCutoff(now, rules.ToolRunsDays)
	if err := run("tool_runs", rules.ToolRunsDays > 0, func(ctx context.Context) (int64, error) {
		return s.q.DeleteRetentionToolRuns(ctx, sqlc.DeleteRetentionToolRunsParams{Cutoff: toolCutoff, WorkspaceID: ws, BatchSize: batch})
	}); err != nil {
		return result, err
	}
	if rules.WorkflowDays > 0 {
		for {
			counts, issue, picked, err := s.deleteWorkflowBatch(ctx, ws, retentionCutoff(now, rules.WorkflowDays), batch)
			if err != nil {
				return result, err
			}
			for table, n := range counts {
				result.Counts[table] += n
			}
			if issue != nil {
				result.Issues = append(result.Issues, *issue)
				break
			}
			if picked < int(batch) {
				break
			}
		}
	}
	return result, nil
}

func foreignKeyCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return pgErr.Code
	}
	return ""
}

type workflowDelete struct {
	table string
	sql   string
}

var workflowDeletes = []workflowDelete{
	{"workflow_signals", `DELETE FROM workflow_signals WHERE execution_id=ANY($1::bigint[])`},
	{"workflow_waits", `DELETE FROM workflow_waits WHERE execution_id=ANY($1::bigint[])`},
	{"workflow_alerts", `DELETE FROM workflow_alerts WHERE execution_id=ANY($1::bigint[]) AND status<>'pending'`},
	{"worker_events", `DELETE FROM worker_events WHERE job_id IN (SELECT id FROM worker_jobs WHERE execution_id=ANY($1::bigint[]))`},
	{"agent_batch_results", `DELETE FROM agent_batch_results WHERE execution_id=ANY($1::bigint[])`},
	{"orchestrator_plan_tasks", `DELETE FROM orchestrator_plan_tasks WHERE plan_id IN (SELECT id FROM orchestrator_plans WHERE execution_id=ANY($1::bigint[]))`},
	{"jobs", `DELETE FROM jobs WHERE execution_id=ANY($1::bigint[])`},
	{"execution_events", `DELETE FROM execution_events WHERE execution_id=ANY($1::bigint[])`},
	{"worker_jobs", `DELETE FROM worker_jobs WHERE execution_id=ANY($1::bigint[])`},
	{"agent_jobs", `DELETE FROM agent_jobs WHERE workflow_execution_id=ANY($1::bigint[])`},
	{"conversation_messages", `DELETE FROM conversation_messages WHERE execution_id=ANY($1::bigint[])`},
	{"conversation_workflows", `DELETE FROM conversation_workflows WHERE execution_id=ANY($1::bigint[])`},
	{"resource_bindings", `DELETE FROM resource_bindings WHERE execution_id=ANY($1::bigint[])`},
	{"workflow_instructions", `DELETE FROM workflow_instructions WHERE execution_id=ANY($1::bigint[])`},
	{"worker_task_routes", `DELETE FROM worker_task_routes WHERE execution_id=ANY($1::bigint[])`},
	{"orchestrator_decisions", `DELETE FROM orchestrator_decisions WHERE execution_id=ANY($1::bigint[])`},
	{"orchestrator_routes", `DELETE FROM orchestrator_routes WHERE execution_id=ANY($1::bigint[])`},
	{"orchestrator_plans", `DELETE FROM orchestrator_plans WHERE execution_id=ANY($1::bigint[])`},
	{"clickup_drafts", `DELETE FROM clickup_drafts WHERE execution_id=ANY($1::bigint[])`},
	{"approvals", `DELETE FROM approvals WHERE execution_id=ANY($1::bigint[]) AND decision IS NOT NULL`},
	{"execution_steps", `DELETE FROM execution_steps WHERE execution_id=ANY($1::bigint[])`},
	{"workflow_executions", `DELETE FROM workflow_executions WHERE id=ANY($1::bigint[])`},
}

func (s *RetentionStore) deleteWorkflowBatch(ctx context.Context, ws pgtype.UUID, cutoff pgtype.Timestamptz, batch int32) (map[string]int64, *behavior.RetentionIssue, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, nil, 0, err
	}
	defer tx.Rollback(ctx)
	ids, err := s.q.WithTx(tx).PickRetentionWorkflowExecutions(ctx, sqlc.PickRetentionWorkflowExecutionsParams{Cutoff: cutoff, WorkspaceID: ws, BatchSize: batch})
	if err != nil {
		return nil, nil, 0, err
	}
	if len(ids) == 0 {
		_ = tx.Rollback(ctx)
		return map[string]int64{}, nil, 0, nil
	}
	if _, err = tx.Exec(ctx, `SET LOCAL nexushub.retention_sweep = 'on'`); err != nil {
		return nil, nil, 0, err
	}
	counts := map[string]int64{}
	for _, d := range workflowDeletes {
		tag, execErr := tx.Exec(ctx, d.sql, ids)
		if execErr != nil {
			_ = tx.Rollback(ctx)
			if code := foreignKeyCode(execErr); code != "" {
				return nil, &behavior.RetentionIssue{Table: d.table, Code: code}, len(ids), nil
			}
			return nil, nil, len(ids), execErr
		}
		counts[d.table] += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		if code := foreignKeyCode(err); code != "" {
			return nil, &behavior.RetentionIssue{Table: "workflow_executions", Code: code}, len(ids), nil
		}
		return nil, nil, len(ids), err
	}
	return counts, nil, len(ids), nil
}
