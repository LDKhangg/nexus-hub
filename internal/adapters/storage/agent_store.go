package storage

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentLease bounds one attempt; an expired lease is recovered by Claim.
const AgentLease = 30 * time.Minute

// untriagedLimit is the LIMIT of UntriagedAgentMessages.
const untriagedLimit = 100

// AgentStore is the durable agent job queue.
type AgentStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries

	mu    sync.RWMutex
	known map[string]tenant.Tier
}

func NewAgentStore(pool *pgxpool.Pool) (*AgentStore, error) {
	if pool == nil {
		return nil, errors.New("agent store requires pool")
	}
	return &AgentStore{pool: pool, q: sqlc.New(pool)}, nil
}

var _ agent.AgentStore = (*AgentStore)(nil)

// SetKnownTools registers the tool tiers used to parse agent configs.
func (s *AgentStore) SetKnownTools(known map[string]tenant.Tier) {
	s.mu.Lock()
	s.known = known
	s.mu.Unlock()
}

// Claim takes one due job. Claims are serialized per thread: the candidate
// row lock alone cannot stop two workers from claiming two jobs of one
// thread, so Claim also takes the thread's advisory lock and re-checks for a
// live lease in a fresh snapshot. If the thread is busy, it returns nil and
// the job is picked up on a later poll. A workspace cut over to the durable
// workflow queue is never claimed (runtime-mode fence).
func (s *AgentStore) Claim(ctx context.Context, now time.Time) (*agent.AgentJob, error) {
	tok, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	token, _ := parseDBID(tok.String())
	at := pgtype.Timestamptz{Time: now, Valid: true}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	cand, err := q.SelectAgentClaimCandidate(ctx, at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	locked, err := q.TryLockAgentThread(ctx, cand.ThreadID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}
	// The runtime-mode fence: a cut-over workspace (or one whose cutover is
	// running) belongs to the durable workflow queue.
	if allowed, err := legacyClaimAllowed(ctx, q, cand.WorkspaceID); err != nil || !allowed {
		return nil, err
	}
	busy, err := q.CheckAgentThreadLease(ctx, sqlc.CheckAgentThreadLeaseParams{ThreadID: cand.ThreadID, ID: cand.ID, AtTime: at})
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, nil
	}
	row, err := q.ClaimAgentJobByID(ctx, sqlc.ClaimAgentJobByIDParams{ID: cand.ID, Token: token, AtTime: at, LeaseSecs: AgentLease.Seconds()})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &agent.AgentJob{
		ID: row.ID, WorkspaceID: row.WorkspaceID.String(), ThreadID: row.ThreadID.String(), SourceIntegrationID: row.SourceIntegrationID.String(),
		ClaimToken: tok.String(), Attempts: int(row.Attempts),
		Usage: domain.Usage{Input: int64(row.InputTokens), Output: int64(row.OutputTokens), CacheRead: int64(row.CacheReadTokens), CacheWrite: int64(row.CacheWriteTokens)},
	}, nil
}

func (s *AgentStore) Load(ctx context.Context, job agent.AgentJob) (agent.JobContext, error) {
	ws, thread, err := parseScopedIDs(job.WorkspaceID, job.ThreadID)
	if err != nil {
		return agent.JobContext{}, err
	}
	th, err := s.q.GetAgentThread(ctx, sqlc.GetAgentThreadParams{WorkspaceID: ws, ID: thread})
	if err != nil {
		return agent.JobContext{}, err
	}
	rows, err := s.q.RecentAgentMessages(ctx, thread)
	if err != nil {
		return agent.JobContext{}, err
	}
	last, err := s.q.LastTriagedMessage(ctx, thread)
	if err != nil {
		return agent.JobContext{}, err
	}
	jc := agent.JobContext{Title: th.Title, Summary: th.RollingSummary, LastTriagedID: last, TaskURL: "https://app.clickup.com/t/" + th.ExternalID}
	if jc.Title == "" {
		jc.Title = th.ExternalID
	}
	// The oldest untriaged external messages are loaded on their own, so a
	// batch is never pushed out of the recent window by later messages.
	untriaged, err := s.q.UntriagedAgentMessages(ctx, sqlc.UntriagedAgentMessagesParams{ThreadID: thread, AfterID: last})
	if err != nil {
		return agent.JobContext{}, err
	}
	jc.MoreUntriaged = len(untriaged) == untriagedLimit
	merged := make([]sqlc.RecentAgentMessagesRow, 0, len(rows)+len(untriaged))
	for _, r := range untriaged {
		merged = append(merged, sqlc.RecentAgentMessagesRow(r))
	}
	merged = append(merged, rows...)
	slices.SortStableFunc(merged, func(a, b sqlc.RecentAgentMessagesRow) int { return cmp.Compare(a.ID, b.ID) })
	merged = slices.CompactFunc(merged, func(a, b sqlc.RecentAgentMessagesRow) bool { return a.ID == b.ID })
	if jc.MoreUntriaged {
		// The batch ends at the newest loaded untriaged message; newer
		// external messages from the recent window belong to the follow-up
		// job, so the batch stays contiguous.
		cut := untriaged[len(untriaged)-1].ID
		kept := merged[:0]
		for _, r := range merged {
			if r.ID > cut && r.SenderRole == string(memory.SenderRoleExternalUser) {
				continue
			}
			kept = append(kept, r)
		}
		merged = kept
	}
	for _, r := range merged {
		jc.History = append(jc.History, memory.Message{ID: r.ID, ThreadID: job.ThreadID, SenderRole: memory.SenderRole(r.SenderRole), SenderName: r.SenderName, Content: r.Content})
		var meta map[string]any
		if json.Unmarshal(r.Metadata, &meta) == nil {
			if u, ok := meta["task_url"].(string); ok && u != "" {
				jc.TaskURL = u
			}
		}
	}
	source, err := s.source(ctx, job)
	if err != nil {
		return agent.JobContext{}, err
	}
	s.mu.RLock()
	known := s.known
	s.mu.RUnlock()
	if cfg, err := tenant.ParseAgentConfig(source.Config, known); err == nil {
		jc.Config = cfg // an invalid config leaves the agent disabled
	}
	return jc, nil
}

func (s *AgentStore) source(ctx context.Context, job agent.AgentJob) (tenant.WebhookIntegration, error) {
	id, err := parseDBID(job.SourceIntegrationID)
	if err != nil {
		return tenant.WebhookIntegration{}, err
	}
	src, err := lookupNoticeIntegration(ctx, s.q, id)
	if err != nil {
		return src, err
	}
	if src.WorkspaceID != job.WorkspaceID {
		return src, errors.New("agent job source outside workspace")
	}
	return src, nil
}

func (s *AgentStore) fence(job agent.AgentJob) (pgtype.UUID, error) {
	token, err := parseDBID(job.ClaimToken)
	if err != nil {
		return token, agent.ErrStaleAgentClaim
	}
	return token, nil
}

func staleIfZero(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return agent.ErrStaleAgentClaim
	}
	return nil
}

func (s *AgentStore) AddUsage(ctx context.Context, job agent.AgentJob, u domain.Usage) error {
	token, err := s.fence(job)
	if err != nil {
		return err
	}
	return staleIfZero(s.q.AddAgentUsage(ctx, sqlc.AddAgentUsageParams{ID: job.ID, Token: token, Input: int32(u.Input), Output: int32(u.Output), CacheRead: int32(u.CacheRead), CacheWrite: int32(u.CacheWrite)}))
}

func (s *AgentStore) Retry(ctx context.Context, job agent.AgentJob, at time.Time, code string) error {
	token, err := s.fence(job)
	if err != nil {
		return err
	}
	return staleIfZero(s.q.RetryAgentJob(ctx, sqlc.RetryAgentJobParams{ID: job.ID, Token: token, NextAt: pgtype.Timestamptz{Time: at, Valid: true}, Code: code}))
}

func (s *AgentStore) CommitOutcome(ctx context.Context, job agent.AgentJob, r agent.JobResult) error {
	return s.commit(ctx, job, "done", "", r.RouterNote, r.ActorNote, r.Text, r.ThroughMessageID)
}

func (s *AgentStore) CommitFallback(ctx context.Context, job agent.AgentJob, code, text string, through int64) error {
	return s.commit(ctx, job, "failed", code, "", "", text, through)
}

// commit finishes the job, stores the AI notes and enqueues the Telegram
// message in one transaction, fenced by the claim token.
func (s *AgentStore) commit(ctx context.Context, job agent.AgentJob, status, code, routerNote, actorNote, text string, through int64) error {
	token, err := s.fence(job)
	if err != nil {
		return err
	}
	ws, thread, err := parseScopedIDs(job.WorkspaceID, job.ThreadID)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.LockAgentClaim(ctx, sqlc.LockAgentClaimParams{ID: job.ID, Token: token}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agent.ErrStaleAgentClaim
		}
		return err
	}
	finish := func(code string) error {
		n, err := q.FinishAgentJob(ctx, sqlc.FinishAgentJobParams{ID: job.ID, Token: token, Status: status, Through: pgtype.Int8{Int64: through, Valid: through > 0}, Code: optText(code)})
		return staleIfZero(n, err)
	}
	if err := s.writeBatchOutcome(ctx, q, job, ws, thread, code, routerNote, actorNote, text, through, finish); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeBatchOutcome writes a finished batch inside the caller's fenced
// transaction: the AI notes, the Telegram report in the outbox (dedupe key
// "agent_job:<id>"), the job's end via finish (with the error code, which
// becomes invalid_target when the report has no valid target), the thread
// refresh and the follow-up job.
func (s *AgentStore) writeBatchOutcome(ctx context.Context, q *sqlc.Queries, job agent.AgentJob, ws, thread pgtype.UUID, code, routerNote, actorNote, text string, through int64, finish func(code string) error) error {
	for _, note := range []struct{ role, content string }{{"ai_router", routerNote}, {"ai_actor", actorNote}} {
		if note.content == "" {
			continue
		}
		if _, err := q.SaveAgentMessage(ctx, sqlc.SaveAgentMessageParams{ThreadID: thread, SenderName: "Nexus", SenderRole: note.role, Content: note.content,
			OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Metadata: []byte(fmt.Sprintf(`{"agent_job_id":%d}`, job.ID))}); err != nil {
			return err
		}
	}
	if text != "" {
		target, err := s.targetOf(ctx, job)
		if err != nil {
			code = "invalid_target"
		} else {
			payload, _ := json.Marshal(map[string]string{"text": text})
			if err := q.InsertOutboundAction(ctx, sqlc.InsertOutboundActionParams{WorkspaceID: ws, IntegrationID: target, Payload: payload, DedupeKey: agent.AgentOperationKey(job.ID)}); err != nil {
				return err
			}
		}
	}
	if err := finish(code); err != nil {
		return err
	}
	if err := q.RefreshMessageThread(ctx, thread); err != nil {
		return err
	}
	// External messages past this batch (a batch capped by the untriaged
	// limit, or comments that arrived while it was processed) get a due
	// follow-up job in the same transaction, so none is left untriaged. An
	// existing pending job is kept.
	source, err := parseDBID(job.SourceIntegrationID)
	if err != nil {
		return err
	}
	if _, err := q.EnqueueAgentFollowUp(ctx, sqlc.EnqueueAgentFollowUpParams{WorkspaceID: ws, SourceIntegrationID: source, ThreadID: thread, Through: through}); err != nil {
		return err
	}
	return nil
}

// knownTools returns the registered tool tiers used to parse agent configs.
func (s *AgentStore) knownTools() map[string]tenant.Tier {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.known
}

// targetOf is the Telegram control integration named by the source config.
func (s *AgentStore) targetOf(ctx context.Context, job agent.AgentJob) (pgtype.UUID, error) {
	src, err := s.source(ctx, job)
	if err != nil {
		return pgtype.UUID{}, err
	}
	cfg, err := tenant.ParseClickUpNoticeConfig(src.Config)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return parseDBID(cfg.TargetIntegrationID)
}

var _ memory.SummaryStore = (*AgentStore)(nil)

func (s *AgentStore) SummaryCandidates(ctx context.Context, workspaceID, threadID string) (string, int64, []memory.Message, error) {
	ws, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return "", 0, nil, err
	}
	th, err := s.q.GetThreadSummary(ctx, sqlc.GetThreadSummaryParams{WorkspaceID: ws, ID: thread})
	if err != nil {
		return "", 0, nil, err
	}
	cursor := th.SummaryThroughMessageID.Int64
	rows, err := s.q.SummaryCandidates(ctx, sqlc.SummaryCandidatesParams{Thread: thread, Cursor: cursor})
	if err != nil {
		return "", 0, nil, err
	}
	out := make([]memory.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, memory.Message{ID: r.ID, ThreadID: threadID, SenderRole: memory.SenderRole(r.SenderRole), SenderName: r.SenderName, Content: r.Content})
	}
	return th.RollingSummary, cursor, out, nil
}

// SaveSummary stores the summary under the thread lock, only if the cursor
// is still expectedCursor.
func (s *AgentStore) SaveSummary(ctx context.Context, workspaceID, threadID string, expectedCursor, through int64, summary string) error {
	ws, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.LockMessageThread(ctx, sqlc.LockMessageThreadParams{WorkspaceID: ws, ID: thread}); err != nil {
		return err
	}
	n, err := q.SaveThreadSummary(ctx, sqlc.SaveThreadSummaryParams{WorkspaceID: ws, ID: thread, Expected: expectedCursor, Through: through, Summary: summary})
	if err != nil {
		return err
	}
	if n == 0 {
		return memory.ErrSummaryCursorMoved
	}
	return tx.Commit(ctx)
}
