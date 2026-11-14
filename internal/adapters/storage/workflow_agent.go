package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the durable side of the Phase 2 comment pipeline: adopting
// due agent batches into workflow executions of the internal pipeline
// definition, and the fenced writes of the pipeline steps. Each write runs
// in a transaction that first locks the workflow job fence (claim token,
// lease, execution state, cancellation and deadline, step and attempt) and
// then the batch, which must be the execution's legacy job in the same
// workspace, workflow-owned and processing. A refused fence is
// agent.ErrWorkflowFence; nothing is written.
//
// Nothing here is started by the server yet: StartDueBatches is the
// enqueue path the cutover (P7) switches on behind its runtime fence.

// AgentWorkflowStore stores workflow-owned agent batches.
type AgentWorkflowStore struct {
	pool      *pgxpool.Pool
	q         *sqlc.Queries
	agents    *AgentStore
	workflows *WorkflowStore
}

// NewAgentWorkflowStore builds the store over the agent job store (its
// context loading, known tools and report writes) and the workflow store
// (StartExecutionTx).
func NewAgentWorkflowStore(agents *AgentStore, workflows *WorkflowStore) (*AgentWorkflowStore, error) {
	if agents == nil || workflows == nil || agents.pool == nil {
		return nil, errors.New("agent workflow store requires agent and workflow stores")
	}
	return &AgentWorkflowStore{pool: agents.pool, q: sqlc.New(agents.pool), agents: agents, workflows: workflows}, nil
}

var _ agent.DurableAgentStore = (*AgentWorkflowStore)(nil)

func mapAgentBatch(row sqlc.AgentJob) agent.AgentBatch {
	return agent.AgentBatch{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), ThreadID: uuidString(row.ThreadID),
		SourceIntegrationID: uuidString(row.SourceIntegrationID), Status: row.Status, Attempts: int(row.Attempts),
		Usage: domain.Usage{Input: int64(row.InputTokens), Output: int64(row.OutputTokens),
			CacheRead: int64(row.CacheReadTokens), CacheWrite: int64(row.CacheWriteTokens)},
		ExecutionID: row.WorkflowExecutionID.Int64, AttemptDeadline: tsPtr(row.WorkflowAttemptDeadline),
		ThroughMessageID: row.ThroughMessageID.Int64, ErrorCode: row.LastErrorCode.String,
	}
}

func agentBatchJob(b agent.AgentBatch) agent.AgentJob {
	return agent.AgentJob{ID: b.ID, WorkspaceID: b.WorkspaceID, ThreadID: b.ThreadID,
		SourceIntegrationID: b.SourceIntegrationID, Attempts: b.Attempts, Usage: b.Usage}
}

// --- adoption ---

// StartDueBatches hands up to limit due pending batches of the workspace to
// new executions of the internal pipeline and returns how many it started.
// Debounce and max-wait stay those of the pending job (run_at, set at
// ingest); the oldest due batch goes first; a thread with any processing
// job (legacy or workflow-owned) is skipped, under the same advisory
// thread lock as the legacy claimer. Each batch is adopted in one
// transaction with its execution, pinned to the internal version holding
// the settings snapshot (a changed snapshot publishes a new version).
func (s *AgentWorkflowStore) StartDueBatches(ctx context.Context, workspaceID string, now time.Time, ps agent.PipelineSettings, limit int) (int, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || now.IsZero() || limit < 1 {
		return 0, workflowInvalid("start due batches")
	}
	started := 0
	for started < limit {
		ok, err := s.startOne(ctx, ws, now, ps)
		if err != nil {
			return started, err
		}
		if !ok {
			break
		}
		started++
	}
	return started, nil
}

func (s *AgentWorkflowStore) startOne(ctx context.Context, ws pgtype.UUID, now time.Time, ps agent.PipelineSettings) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	cand, err := q.SelectDueAgentBatch(ctx, sqlc.SelectDueAgentBatchParams{WorkspaceID: ws, AtTime: ts(now)})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	locked, err := q.TryLockAgentThread(ctx, cand.ThreadID)
	if err != nil || !locked {
		return false, err
	}
	busy, err := q.AgentThreadBusy(ctx, sqlc.AgentThreadBusyParams{WorkspaceID: ws, ThreadID: cand.ThreadID, ID: cand.ID})
	if err != nil || busy {
		return false, err
	}
	source, err := lookupNoticeIntegration(ctx, q, cand.SourceIntegrationID)
	if err != nil {
		return false, err
	}
	if source.WorkspaceID != uuidString(ws) {
		return false, errors.New("agent batch source outside workspace")
	}
	cfg, perr := tenant.ParseAgentConfig(source.Config, s.agents.knownTools())
	snap := agent.BuildPipelineSnapshot(cfg, perr == nil, ps)
	versionID, err := ensurePipelineVersion(ctx, q, ws, snap)
	if err != nil {
		return false, err
	}
	corr, err := uuid.NewRandom()
	if err != nil {
		return false, err
	}
	batch := agent.AgentBatch{ID: cand.ID, WorkspaceID: uuidString(ws), ThreadID: uuidString(cand.ThreadID),
		SourceIntegrationID: uuidString(cand.SourceIntegrationID)}
	execution, err := s.workflows.StartExecutionTx(ctx, tx, agent.WorkflowStartRequest(batch, versionID, corr.String()))
	if err != nil {
		return false, err
	}
	if execution.References.LegacyJobID != cand.ID || execution.State != workflow.StateQueued {
		return false, workflow.ErrConflict
	}
	if _, err := q.AdoptAgentBatch(ctx, sqlc.AdoptAgentBatchParams{ExecutionID: workflowInt8(execution.ID), WorkspaceID: ws, ID: cand.ID}); err != nil {
		if isNoRows(err) {
			return false, workflow.ErrConflict
		}
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ensurePipelineVersion returns the workspace's internal version whose
// document equals snap, publishing a new immutable version when the
// settings changed. The internal definition row serializes it.
func ensurePipelineVersion(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, snap agent.PipelineSnapshot) (int64, error) {
	doc, err := json.Marshal(snap)
	if err != nil {
		return 0, err
	}
	if err := q.InsertInternalBehavior(ctx, sqlc.InsertInternalBehaviorParams{WorkspaceID: ws, InternalKind: agent.InternalPipelineKind}); err != nil {
		return 0, err
	}
	def, err := q.LockInternalBehavior(ctx, sqlc.LockInternalBehaviorParams{WorkspaceID: ws, InternalKind: agent.InternalPipelineKind})
	if err != nil {
		return 0, err
	}
	next := int32(1)
	latest, err := q.LatestInternalBehaviorVersion(ctx, sqlc.LatestInternalBehaviorVersionParams{WorkspaceID: ws, BehaviorID: def.ID})
	switch {
	case err == nil:
		var prev agent.PipelineSnapshot
		if json.Unmarshal(latest.Document, &prev) == nil {
			if prevDoc, err := json.Marshal(prev); err == nil && bytes.Equal(prevDoc, doc) {
				return latest.ID, nil
			}
		}
		next = latest.Version + 1
	case !isNoRows(err):
		return 0, err
	}
	v, err := q.InsertInternalBehaviorVersion(ctx, sqlc.InsertInternalBehaviorVersionParams{
		WorkspaceID: ws, BehaviorID: def.ID, Version: next, Document: doc,
	})
	if err != nil {
		return 0, err
	}
	return v.ID, nil
}

// --- reads ---

func (s *AgentWorkflowStore) GetBatch(ctx context.Context, workspaceID string, jobID int64) (agent.AgentBatch, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || jobID <= 0 {
		return agent.AgentBatch{}, workflowInvalid("batch")
	}
	row, err := s.q.GetAgentBatch(ctx, sqlc.GetAgentBatchParams{WorkspaceID: ws, ID: jobID})
	if isNoRows(err) {
		return agent.AgentBatch{}, agent.ErrBatchNotFound
	}
	if err != nil {
		return agent.AgentBatch{}, err
	}
	return mapAgentBatch(row), nil
}

func (s *AgentWorkflowStore) LoadBatch(ctx context.Context, b agent.AgentBatch) (agent.JobContext, error) {
	return s.agents.Load(ctx, agentBatchJob(b))
}

func (s *AgentWorkflowStore) PipelineSnapshot(ctx context.Context, workspaceID string, versionID int64) (agent.PipelineSnapshot, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || versionID <= 0 {
		return agent.PipelineSnapshot{}, workflowInvalid("version")
	}
	v, err := s.q.GetInternalBehaviorVersion(ctx, sqlc.GetInternalBehaviorVersionParams{WorkspaceID: ws, ID: versionID, InternalKind: agent.InternalPipelineKind})
	if isNoRows(err) {
		return agent.PipelineSnapshot{}, agent.ErrBatchNotFound
	}
	if err != nil {
		return agent.PipelineSnapshot{}, err
	}
	var snap agent.PipelineSnapshot
	if err := json.Unmarshal(v.Document, &snap); err != nil || snap.Kind != agent.InternalPipelineKind {
		return agent.PipelineSnapshot{}, agent.ErrBatchNotFound
	}
	return snap, nil
}

func parseOutcome(s string) agent.Outcome {
	switch s {
	case "notify":
		return agent.OutcomeNotify
	case "investigate":
		return agent.OutcomeInvestigate
	case "ignore":
		return agent.OutcomeIgnore
	default:
		return 0
	}
}

func (s *AgentWorkflowStore) BatchResults(ctx context.Context, workspaceID string, jobID int64) ([]agent.BatchResult, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || jobID <= 0 {
		return nil, workflowInvalid("batch")
	}
	rows, err := s.q.ListAgentBatchResults(ctx, sqlc.ListAgentBatchResultsParams{WorkspaceID: ws, AgentJobID: jobID})
	if err != nil {
		return nil, err
	}
	out := make([]agent.BatchResult, 0, len(rows))
	for _, r := range rows {
		out = append(out, agent.BatchResult{
			ID: r.ID, Stage: r.Stage, Outcome: parseOutcome(r.Outcome), ThroughMessageID: r.ThroughMessageID,
			LastTriagedID: r.LastTriagedID, MaxMessageID: r.MaxMessageID,
			Decision: agent.TriageDecision{Category: agent.Category(r.Category), Urgency: agent.Urgency(r.Urgency),
				Confidence: r.Confidence, Summary: r.Summary, Goal: r.Goal},
			ToolsAvailable: r.ToolsAvailable, RouterNote: r.RouterNote, Unseen: int(r.UnseenCount),
			ReportText: r.ReportText, ToolsLine: r.ToolsLine,
			TotalInputTokens: r.TotalInputTokens, TotalOutputTokens: r.TotalOutputTokens,
		})
	}
	return out, nil
}

// --- fenced writes ---

type durableBatchTx struct {
	q     *sqlc.Queries
	ws    pgtype.UUID
	job   sqlc.Job
	batch sqlc.AgentJob
	req   workflow.JobCheckpointRequest
}

func (t *durableBatchTx) progress(ctx context.Context, cp *workflow.Checkpoint, usage workflow.Usage) error {
	var checkpoint []byte
	if cp != nil {
		var err error
		if checkpoint, err = workflow.MarshalCheckpoint(*cp); err != nil {
			return invalid("checkpoint")
		}
	}
	req := t.req
	req.Usage = usage
	_, err := updateWorkflowProgress(ctx, t.q, t.ws, t.job, req, checkpoint)
	if errors.Is(err, workflow.ErrStaleClaim) {
		return agent.ErrWorkflowFence
	}
	return err
}

// withDurableBatch runs fn under the fence and the batch lock.
func (s *AgentWorkflowStore) withDurableBatch(ctx context.Context, f agent.AgentFence, jobID int64, fn func(*durableBatchTx) error) error {
	ws, err := parseDBID(f.WorkspaceID)
	if err != nil || jobID <= 0 || f.JobID <= 0 || f.ExecutionID <= 0 || strings.TrimSpace(f.StepKey) == "" ||
		f.Attempt < 1 || f.At.IsZero() {
		return workflowInvalid("batch fence")
	}
	token, err := workflowJobToken(f.ClaimToken)
	if err != nil {
		return workflowInvalid("batch fence")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	req := workflow.JobCheckpointRequest{
		JobID: f.JobID, WorkspaceID: f.WorkspaceID, ExecutionID: f.ExecutionID, StepKey: f.StepKey,
		ExpectedAttempt: f.Attempt, ClaimToken: f.ClaimToken, At: f.At,
	}
	// Lock order (R6): the execution, then its job, then the batch.
	if _, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: f.ExecutionID}); err != nil {
		if isNoRows(err) {
			return agent.ErrWorkflowFence
		}
		return err
	}
	job, err := lockWorkflowFence(ctx, q, ws, token, req)
	if errors.Is(err, workflow.ErrStaleClaim) {
		return agent.ErrWorkflowFence
	}
	if err != nil {
		return err
	}
	batch, err := q.LockDurableAgentBatch(ctx, sqlc.LockDurableAgentBatchParams{WorkspaceID: ws, ID: jobID, ExecutionID: f.ExecutionID})
	if isNoRows(err) {
		return agent.ErrWorkflowFence
	}
	if err != nil {
		return err
	}
	if batch.Status != agent.BatchProcessing || batch.ClaimToken.Valid {
		return agent.ErrBatchConflict
	}
	if err := fn(&durableBatchTx{q: q, ws: ws, job: job, batch: batch, req: req}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func batchRows(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return agent.ErrBatchConflict
	}
	return nil
}

func (s *AgentWorkflowStore) BeginBatchAttempt(ctx context.Context, f agent.AgentFence, jobID int64, deadline time.Time) (agent.AgentBatch, error) {
	if deadline.IsZero() {
		return agent.AgentBatch{}, workflowInvalid("deadline")
	}
	var out agent.AgentBatch
	err := s.withDurableBatch(ctx, f, jobID, func(t *durableBatchTx) error {
		row, err := t.q.BeginDurableAgentAttempt(ctx, sqlc.BeginDurableAgentAttemptParams{Deadline: ts(deadline), WorkspaceID: t.ws, ID: jobID})
		if isNoRows(err) {
			return agent.ErrBatchConflict
		}
		if err != nil {
			return err
		}
		out = mapAgentBatch(row)
		return nil
	})
	return out, err
}

func (s *AgentWorkflowStore) AddBatchUsage(ctx context.Context, f agent.AgentFence, jobID int64, u domain.Usage) error {
	if u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
		return workflowInvalid("usage")
	}
	return s.withDurableBatch(ctx, f, jobID, func(t *durableBatchTx) error {
		if err := batchRows(t.q.AddDurableAgentUsage(ctx, sqlc.AddDurableAgentUsageParams{
			Input: int32(u.Input), Output: int32(u.Output), CacheRead: int32(u.CacheRead), CacheWrite: int32(u.CacheWrite),
			WorkspaceID: t.ws, ID: jobID,
		})); err != nil {
			return err
		}
		return t.progress(ctx, nil, workflow.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite})
	})
}

func resultCheckpointKind(stage string) (string, bool) {
	switch stage {
	case agent.StageTriage:
		return agent.CheckpointTriage, true
	case agent.StageInvestigation:
		return agent.CheckpointInvestigation, true
	default:
		return "", false
	}
}

func (s *AgentWorkflowStore) SaveBatchResult(ctx context.Context, f agent.AgentFence, jobID int64, r agent.BatchResult, cp workflow.Checkpoint) (int64, error) {
	kind, ok := resultCheckpointKind(r.Stage)
	if !ok || r.Unseen < 0 || r.ThroughMessageID < 0 || r.LastTriagedID < 0 || r.MaxMessageID < 0 {
		return 0, workflowInvalid("batch result")
	}
	outcome := ""
	if r.Stage == agent.StageTriage {
		switch r.Outcome {
		case agent.OutcomeIgnore, agent.OutcomeNotify, agent.OutcomeInvestigate:
			outcome = r.Outcome.String()
		default:
			return 0, workflowInvalid("batch result outcome")
		}
	}
	var id int64
	err := s.withDurableBatch(ctx, f, jobID, func(t *durableBatchTx) error {
		var err error
		id, err = t.q.InsertAgentBatchResult(ctx, sqlc.InsertAgentBatchResultParams{
			WorkspaceID: t.ws, AgentJobID: jobID, ExecutionID: f.ExecutionID, Stage: r.Stage, Outcome: outcome,
			ThroughMessageID: r.ThroughMessageID, LastTriagedID: r.LastTriagedID, MaxMessageID: r.MaxMessageID,
			Category: string(r.Decision.Category), Urgency: string(r.Decision.Urgency), Confidence: r.Decision.Confidence,
			Summary: r.Decision.Summary, Goal: r.Decision.Goal, RouterNote: r.RouterNote, ToolsAvailable: r.ToolsAvailable,
			UnseenCount: int32(r.Unseen), ReportText: r.ReportText, ToolsLine: r.ToolsLine,
			TotalInputTokens: r.TotalInputTokens, TotalOutputTokens: r.TotalOutputTokens,
		})
		if isNoRows(err) {
			id, err = t.q.GetAgentBatchResultID(ctx, sqlc.GetAgentBatchResultIDParams{WorkspaceID: t.ws, AgentJobID: jobID, Stage: r.Stage})
		}
		if err != nil {
			return err
		}
		ref := workflow.CheckpointReference{Kind: kind, ReferenceID: strconv.FormatInt(id, 10)}
		cp := workflow.Checkpoint{References: append([]workflow.CheckpointReference(nil), cp.References...), Reusable: cp.Reusable}
		present := false
		for _, existing := range cp.References {
			present = present || existing == ref
		}
		if !present {
			cp.References = append(cp.References, ref)
		}
		return t.progress(ctx, &cp, workflow.Usage{})
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *AgentWorkflowStore) RetryBatch(ctx context.Context, f agent.AgentFence, jobID int64, code string) error {
	if strings.TrimSpace(code) == "" {
		return workflowInvalid("code")
	}
	return s.withDurableBatch(ctx, f, jobID, func(t *durableBatchTx) error {
		return batchRows(t.q.RetryDurableAgentBatch(ctx, sqlc.RetryDurableAgentBatchParams{Code: code, WorkspaceID: t.ws, ID: jobID}))
	})
}

func (s *AgentWorkflowStore) CommitBatch(ctx context.Context, f agent.AgentFence, jobID int64, r agent.JobResult, cp workflow.Checkpoint) error {
	return s.finish(ctx, f, jobID, agent.BatchDone, "", r.RouterNote, r.ActorNote, r.Text, r.ThroughMessageID, cp)
}

func (s *AgentWorkflowStore) CommitBatchFallback(ctx context.Context, f agent.AgentFence, jobID int64, code, text string, through int64, cp workflow.Checkpoint) error {
	return s.finish(ctx, f, jobID, agent.BatchFailed, code, "", "", text, through, cp)
}

// finish ends the batch like AgentStore.commit, under the workflow fence
// and with the step checkpoint, in one transaction. The outbox dedupe key
// and the job's end make a repeated commit impossible: a later attempt
// finds the batch finished and reports nothing again.
func (s *AgentWorkflowStore) finish(ctx context.Context, f agent.AgentFence, jobID int64, status, code, routerNote, actorNote, text string, through int64, cp workflow.Checkpoint) error {
	return s.withDurableBatch(ctx, f, jobID, func(t *durableBatchTx) error {
		b := mapAgentBatch(t.batch)
		_, thread, err := parseScopedIDs(b.WorkspaceID, b.ThreadID)
		if err != nil {
			return err
		}
		finish := func(code string) error {
			return batchRows(t.q.FinishDurableAgentBatch(ctx, sqlc.FinishDurableAgentBatchParams{
				Status: status, Through: pgtype.Int8{Int64: through, Valid: through > 0}, Code: optText(code),
				WorkspaceID: t.ws, ID: jobID,
			}))
		}
		if err := s.agents.writeBatchOutcome(ctx, t.q, agentBatchJob(b), t.ws, thread, code, routerNote, actorNote, text, through, finish); err != nil {
			return err
		}
		return t.progress(ctx, &cp, workflow.Usage{})
	})
}
