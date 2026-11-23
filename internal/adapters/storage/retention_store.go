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

const maxRetentionSkippedRows = 1000
const maxDryRunBatchesPerGroup = 5
const retentionDryRunTimeout = 20 * time.Second

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
	return s.sweepRetention(ctx, s.pool, workspace, rules, now, batch, 0)
}

// SweepRetentionDryRun performs the real sweep in one repeatable-read transaction and rolls it back.
func (s *RetentionStore) SweepRetentionDryRun(ctx context.Context, workspace string, rules behavior.RetentionRules, now time.Time, batch int32) (behavior.RetentionResult, error) {
	dryCtx, cancel := context.WithTimeout(ctx, retentionDryRunTimeout)
	defer cancel()
	result := behavior.RetentionResult{Counts: map[string]int64{}}
	tx, err := s.pool.BeginTx(dryCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return dryRunResult(dryCtx, result, err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(dryCtx, `SET LOCAL statement_timeout = '20s'`); err != nil {
		return dryRunResult(dryCtx, result, err)
	}
	if _, err := tx.Exec(dryCtx, `SET LOCAL idle_in_transaction_session_timeout = '20s'`); err != nil {
		return dryRunResult(dryCtx, result, err)
	}
	if _, err := tx.Exec(dryCtx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return dryRunResult(dryCtx, result, err)
	}
	result, err = s.sweepRetention(dryCtx, tx, workspace, rules, now, batch, maxDryRunBatchesPerGroup)
	if err != nil && retentionLockTimeout(err) {
		return behavior.RetentionResult{}, ErrRetentionBusy
	}
	return dryRunResult(dryCtx, result, err)
}

func dryRunResult(ctx context.Context, result behavior.RetentionResult, err error) (behavior.RetentionResult, error) {
	if err != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || retentionQueryTimeout(err)) {
		result.Partial = true
		return result, nil
	}
	return result, err
}

func (s *RetentionStore) sweepRetention(ctx context.Context, txStarter retentionTxStarter, workspace string, rules behavior.RetentionRules, now time.Time, batch int32, maxBatches int) (behavior.RetentionResult, error) {
	if batch <= 0 {
		batch = behavior.DefaultRetentionBatch
	}
	ws, err := optionalWorkspace(workspace)
	if err != nil {
		return behavior.RetentionResult{}, err
	}
	result := behavior.RetentionResult{Counts: map[string]int64{}}
	skipped := 0
	run := func(table string, enabled bool, fn retentionBatchFn) (bool, error) {
		if !enabled {
			return false, nil
		}
		return runRetentionGroup(ctx, &result, table, batch, maxBatches, &skipped, fn)
	}
	outCutoff := retentionCutoff(now, rules.OutboundDays)
	stop, err := run("notification_outbox", rules.OutboundDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteRetentionBatch(ctx, txStarter, "notification_outbox", `DELETE FROM notification_outbox WHERE id=ANY($1::bigint[])`, func(q *sqlc.Queries) ([]int64, error) {
			return q.PickRetentionNotificationOutbox(ctx, sqlc.PickRetentionNotificationOutboxParams{Cutoff: outCutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: optionalRetentionID(onlyID), BatchSize: size})
		})
	})
	if err != nil || stop {
		return result, err
	}
	stop, err = run("outbound_actions", rules.OutboundDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteRetentionBatch(ctx, txStarter, "outbound_actions", `DELETE FROM outbound_actions WHERE id=ANY($1::bigint[])`, func(q *sqlc.Queries) ([]int64, error) {
			return q.PickRetentionOutboundActions(ctx, sqlc.PickRetentionOutboundActionsParams{Cutoff: outCutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: optionalRetentionID(onlyID), BatchSize: size})
		})
	})
	if err != nil || stop {
		return result, err
	}
	msgCutoff := retentionCutoff(now, rules.MessageLogsDays)
	stop, err = run("message_logs", rules.MessageLogsDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteRetentionBatch(ctx, txStarter, "message_logs", `DELETE FROM message_logs WHERE id=ANY($1::bigint[])`, func(q *sqlc.Queries) ([]int64, error) {
			return q.PickRetentionMessageLogs(ctx, sqlc.PickRetentionMessageLogsParams{Cutoff: msgCutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: optionalRetentionID(onlyID), BatchSize: size})
		})
	})
	if err != nil || stop {
		return result, err
	}
	policyCutoff := retentionCutoff(now, rules.PolicyDecisionsDays)
	stop, err = run("policy_decisions", rules.PolicyDecisionsDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteRetentionBatch(ctx, txStarter, "policy_decisions", `DELETE FROM policy_decisions WHERE id=ANY($1::bigint[])`, func(q *sqlc.Queries) ([]int64, error) {
			return q.PickRetentionPolicyDecisions(ctx, sqlc.PickRetentionPolicyDecisionsParams{Cutoff: policyCutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: optionalRetentionID(onlyID), BatchSize: size})
		})
	})
	if err != nil || stop {
		return result, err
	}
	toolCutoff := retentionCutoff(now, rules.ToolRunsDays)
	stop, err = run("tool_runs", rules.ToolRunsDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteRetentionBatch(ctx, txStarter, "tool_runs", `DELETE FROM tool_runs WHERE id=ANY($1::bigint[])`, func(q *sqlc.Queries) ([]int64, error) {
			return q.PickRetentionToolRuns(ctx, sqlc.PickRetentionToolRunsParams{Cutoff: toolCutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: optionalRetentionID(onlyID), BatchSize: size})
		})
	})
	if err != nil || stop {
		return result, err
	}
	workflowCutoff := retentionCutoff(now, rules.WorkflowDays)
	stop, err = run("workflow_executions", rules.WorkflowDays > 0, func(ctx context.Context, size int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
		return deleteWorkflowBatch(ctx, txStarter, ws, workflowCutoff, size, skipIDs, optionalRetentionID(onlyID))
	})
	if err != nil || stop {
		return result, err
	}
	return result, nil
}

type retentionBatchFn func(context.Context, int32, []int64, *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error)

func runRetentionGroup(ctx context.Context, result *behavior.RetentionResult, table string, batch int32, maxBatches int, skipped *int, fn retentionBatchFn) (bool, error) {
	skipIDs := make([]int64, 0)
	batchCount := 0
	for {
		if maxBatches > 0 && batchCount >= maxBatches {
			result.Partial = true
			return false, nil
		}
		counts, issue, ids, err := fn(ctx, batch, skipIDs, nil)
		if err != nil {
			return false, err
		}
		batchCount++
		if issue == nil {
			addRetentionCounts(result, counts)
		} else {
			for _, id := range ids {
				id := id
				singleCounts, singleIssue, singleIDs, err := fn(ctx, 1, skipIDs, &id)
				if err != nil {
					return false, err
				}
				if len(singleIDs) == 0 {
					continue
				}
				if singleIssue == nil {
					addRetentionCounts(result, singleCounts)
					continue
				}
				if *skipped >= maxRetentionSkippedRows {
					addRetentionIssue(result, table, "skip_limit", 1)
					return true, nil
				}
				skipIDs = append(skipIDs, id)
				(*skipped)++
				addRetentionIssue(result, singleIssue.Table, singleIssue.Code, 1)
			}
		}
		if len(ids) < int(batch) {
			return false, nil
		}
	}
}

func optionalRetentionID(id *int64) pgtype.Int8 {
	if id == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *id, Valid: true}
}

func deleteRetentionBatch(ctx context.Context, starter retentionTxStarter, table, deleteSQL string, pick func(*sqlc.Queries) ([]int64, error)) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback(ctx)
	ids, err := pick(sqlc.New(tx))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ids) == 0 {
		return map[string]int64{}, nil, ids, nil
	}
	tag, err := tx.Exec(ctx, deleteSQL, ids)
	if err != nil {
		if code := foreignKeyCode(err); code != "" {
			return nil, &behavior.RetentionIssue{Table: table, Code: code}, ids, nil
		}
		return nil, nil, ids, err
	}
	if err := tx.Commit(ctx); err != nil {
		if code := foreignKeyCode(err); code != "" {
			return nil, &behavior.RetentionIssue{Table: table, Code: code}, ids, nil
		}
		return nil, nil, ids, err
	}
	return map[string]int64{table: tag.RowsAffected()}, nil, ids, nil
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

func retentionQueryTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "25P03")
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

func deleteWorkflowBatch(ctx context.Context, starter retentionTxStarter, ws pgtype.UUID, cutoff pgtype.Timestamptz, batch int32, skipIDs []int64, onlyID pgtype.Int8) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback(ctx)
	ids, err := sqlc.New(tx).PickRetentionWorkflowExecutions(ctx, sqlc.PickRetentionWorkflowExecutionsParams{Cutoff: cutoff, WorkspaceID: ws, SkipIds: skipIDs, OnlyID: onlyID, BatchSize: batch})
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
