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

// ErrRetentionBusy means a dry run could not promptly acquire a database lock.
var ErrRetentionBusy = errors.New("retention database is busy")

const maxRetentionSkippedExecutions = 1000

type retentionTxStarter interface {
	Begin(context.Context) (pgx.Tx, error)
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

// SweepRetention drains each enabled category in bounded batches.
func (s *RetentionStore) SweepRetention(ctx context.Context, workspace string, rules behavior.RetentionRules, now time.Time, batch int32) (behavior.RetentionResult, error) {
	return s.sweepRetention(ctx, s.pool, workspace, rules, now, batch)
}

// SweepRetentionDryRun performs the real sweep in one repeatable-read transaction and rolls it back.
func (s *RetentionStore) SweepRetentionDryRun(ctx context.Context, workspace string, rules behavior.RetentionRules, now time.Time, batch int32) (behavior.RetentionResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return behavior.RetentionResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
		return behavior.RetentionResult{}, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return behavior.RetentionResult{}, err
	}
	result, err := s.sweepRetention(ctx, tx, workspace, rules, now, batch)
	if err != nil && retentionLockTimeout(err) {
		return behavior.RetentionResult{}, ErrRetentionBusy
	}
	return result, err
}

func (s *RetentionStore) sweepRetention(ctx context.Context, txStarter retentionTxStarter, workspace string, rules behavior.RetentionRules, now time.Time, batch int32) (behavior.RetentionResult, error) {
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
					addRetentionIssue(&result, table, code, 1)
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
		return executeRetentionDelete(ctx, txStarter, func(q *sqlc.Queries) (int64, error) {
			return q.DeleteRetentionNotificationOutbox(ctx, sqlc.DeleteRetentionNotificationOutboxParams{Cutoff: outCutoff, WorkspaceID: ws, BatchSize: batch})
		})
	}); err != nil {
		return result, err
	}
	if err := run("outbound_actions", rules.OutboundDays > 0, func(ctx context.Context) (int64, error) {
		return executeRetentionDelete(ctx, txStarter, func(q *sqlc.Queries) (int64, error) {
			return q.DeleteRetentionOutboundActions(ctx, sqlc.DeleteRetentionOutboundActionsParams{Cutoff: outCutoff, WorkspaceID: ws, BatchSize: batch})
		})
	}); err != nil {
		return result, err
	}
	msgCutoff := retentionCutoff(now, rules.MessageLogsDays)
	if err := run("message_logs", rules.MessageLogsDays > 0, func(ctx context.Context) (int64, error) {
		return executeRetentionDelete(ctx, txStarter, func(q *sqlc.Queries) (int64, error) {
			return q.DeleteRetentionMessageLogs(ctx, sqlc.DeleteRetentionMessageLogsParams{Cutoff: msgCutoff, WorkspaceID: ws, BatchSize: batch})
		})
	}); err != nil {
		return result, err
	}
	policyCutoff := retentionCutoff(now, rules.PolicyDecisionsDays)
	if err := run("policy_decisions", rules.PolicyDecisionsDays > 0, func(ctx context.Context) (int64, error) {
		return executeRetentionDelete(ctx, txStarter, func(q *sqlc.Queries) (int64, error) {
			return q.DeleteRetentionPolicyDecisions(ctx, sqlc.DeleteRetentionPolicyDecisionsParams{Cutoff: policyCutoff, WorkspaceID: ws, BatchSize: batch})
		})
	}); err != nil {
		return result, err
	}
	toolCutoff := retentionCutoff(now, rules.ToolRunsDays)
	if err := run("tool_runs", rules.ToolRunsDays > 0, func(ctx context.Context) (int64, error) {
		return executeRetentionDelete(ctx, txStarter, func(q *sqlc.Queries) (int64, error) {
			return q.DeleteRetentionToolRuns(ctx, sqlc.DeleteRetentionToolRunsParams{Cutoff: toolCutoff, WorkspaceID: ws, BatchSize: batch})
		})
	}); err != nil {
		return result, err
	}
	if rules.WorkflowDays > 0 {
		skipIDs := make([]int64, 0)
		for {
			counts, issue, ids, err := deleteWorkflowBatch(ctx, txStarter, ws, retentionCutoff(now, rules.WorkflowDays), batch, skipIDs)
			if err != nil {
				return result, err
			}
			if issue == nil {
				addRetentionCounts(&result, counts)
			} else {
				for _, id := range ids {
					singleCounts, singleIssue, err := deleteWorkflowIDs(ctx, txStarter, []int64{id})
					if err != nil {
						return result, err
					}
					if singleIssue == nil {
						addRetentionCounts(&result, singleCounts)
						continue
					}
					if len(skipIDs) >= maxRetentionSkippedExecutions {
						addRetentionIssue(&result, "workflow_executions", "skip_limit", int64(len(skipIDs)+1))
						return result, nil
					}
					skipIDs = append(skipIDs, id)
					addRetentionIssue(&result, singleIssue.Table, singleIssue.Code, 1)
				}
			}
			if len(ids) < int(batch) {
				break
			}
		}
	}
	return result, nil
}

func executeRetentionDelete(ctx context.Context, starter retentionTxStarter, fn func(*sqlc.Queries) (int64, error)) (int64, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	count, err := fn(sqlc.New(tx))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func addRetentionCounts(result *behavior.RetentionResult, counts map[string]int64) {
	for table, count := range counts {
		result.Counts[table] += count
	}
}

func addRetentionIssue(result *behavior.RetentionResult, table, code string, count int64) {
	for i := range result.Issues {
		if result.Issues[i].Table == table && result.Issues[i].Code == code {
			result.Issues[i].Count += count
			return
		}
	}
	result.Issues = append(result.Issues, behavior.RetentionIssue{Table: table, Code: code, Count: count})
}

func foreignKeyCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return pgErr.Code
	}
	return ""
}

func retentionLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
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

func deleteWorkflowBatch(ctx context.Context, starter retentionTxStarter, ws pgtype.UUID, cutoff pgtype.Timestamptz, batch int32, skipIDs []int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback(ctx)
	ids, err := sqlc.New(tx).PickRetentionWorkflowExecutions(ctx, sqlc.PickRetentionWorkflowExecutionsParams{Cutoff: cutoff, WorkspaceID: ws, SkipIds: skipIDs, BatchSize: batch})
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ids) == 0 {
		return map[string]int64{}, nil, ids, nil
	}
	counts, issue, err := executeWorkflowDeletes(ctx, tx, ids)
	if err != nil || issue != nil {
		return nil, issue, ids, err
	}
	if err := tx.Commit(ctx); err != nil {
		if code := foreignKeyCode(err); code != "" {
			return nil, &behavior.RetentionIssue{Table: "workflow_executions", Code: code}, ids, nil
		}
		return nil, nil, ids, err
	}
	return counts, nil, ids, nil
}

func deleteWorkflowIDs(ctx context.Context, starter retentionTxStarter, ids []int64) (map[string]int64, *behavior.RetentionIssue, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	counts, issue, err := executeWorkflowDeletes(ctx, tx, ids)
	if err != nil || issue != nil {
		return nil, issue, err
	}
	if err := tx.Commit(ctx); err != nil {
		if code := foreignKeyCode(err); code != "" {
			return nil, &behavior.RetentionIssue{Table: "workflow_executions", Code: code}, nil
		}
		return nil, nil, err
	}
	return counts, nil, nil
}

func executeWorkflowDeletes(ctx context.Context, tx pgx.Tx, ids []int64) (map[string]int64, *behavior.RetentionIssue, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL nexushub.retention_sweep = 'on'`); err != nil {
		return nil, nil, err
	}
	counts := map[string]int64{}
	for _, d := range workflowDeletes {
		tag, execErr := tx.Exec(ctx, d.sql, ids)
		if execErr != nil {
			if code := foreignKeyCode(execErr); code != "" {
				return nil, &behavior.RetentionIssue{Table: d.table, Code: code}, nil
			}
			return nil, nil, execErr
		}
		counts[d.table] += tag.RowsAffected()
	}
	return counts, nil, nil
}
