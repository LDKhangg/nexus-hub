package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (s *AgentStore) Claim(ctx context.Context, now time.Time) (*agent.AgentJob, error) {
	tok, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	token, _ := parseDBID(tok.String())
	row, err := s.q.ClaimAgentJob(ctx, sqlc.ClaimAgentJobParams{Token: token, AtTime: pgtype.Timestamptz{Time: now, Valid: true}, LeaseSecs: AgentLease.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
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
	for _, r := range rows {
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
			if err := q.InsertOutboundAction(ctx, sqlc.InsertOutboundActionParams{WorkspaceID: ws, IntegrationID: target, Payload: payload, DedupeKey: fmt.Sprintf("agent_job:%d", job.ID)}); err != nil {
				return err
			}
		}
	}
	n, err := q.FinishAgentJob(ctx, sqlc.FinishAgentJobParams{ID: job.ID, Token: token, Status: status, Through: pgtype.Int8{Int64: through, Valid: through > 0}, Code: optText(code)})
	if err := staleIfZero(n, err); err != nil {
		return err
	}
	if err := q.RefreshMessageThread(ctx, thread); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
