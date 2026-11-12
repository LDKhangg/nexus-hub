package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// TC-6 (REQ-2): bad optional configuration never breaks Phase 1 ingest.
func TestApproved_BadAgentConfigFallsBackToPhase1(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			ctx, pool, r := newAgentDB(t, enabled)
			cfg := fmt.Sprintf(`{"enabled":%t,"confidence_threshold":7,"tools":{"enabled":["git_missing_forever"]}}`, enabled)
			mustExec(t, ctx, pool, `UPDATE integrations SET config=jsonb_set(config,'{agent}',$2::jsonb) WHERE id=$1`, integrationA, cfg)
			r.SetKnownTools(map[string]tenant.Tier{})
			comment := noticeEvent("bad-agent-comment", "task-1")
			status := noticeEvent("bad-agent-status", "task-1")
			status.EventType = domain.EventStatusChanged
			for _, ev := range []domain.NormalizedEvent{comment, status} {
				res, err := r.Accept(ctx, []domain.NormalizedEvent{ev})
				if err != nil || res.Accepted != 1 {
					t.Fatalf("Phase 1 callback failed: accepted=%d error=%v", res.Accepted, err)
				}
			}
			if countRows(t, ctx, pool, "agent_jobs") != 0 || countRows(t, ctx, pool, "notification_outbox") != 2 {
				t.Error("invalid optional agent config did not preserve both Phase 1 notices")
			}
		})
	}
}

// TC-3 (REQ-1), TC-4 (REQ-F11): a row-locked expired job must not let
// a competing pending job bypass thread fencing; another thread can progress.
func TestApproved_ExpiredLockedJobAndPendingClaimRace(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	e.job(t, now)
	old, err := e.store.Claim(e.ctx, now)
	if err != nil || old == nil {
		t.Fatal("initial claim failed")
	}
	e.job(t, now)
	mustExec(t, e.ctx, e.pool, `UPDATE agent_jobs SET locked_until=$2 WHERE id=$1`, old.ID, now.Add(-time.Second))
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(e.ctx, `SELECT id FROM agent_jobs WHERE id=$1 FOR UPDATE`, old.ID); err != nil {
		t.Fatal(err)
	}
	// The row lock is the barrier. The pending candidate can be claimed while
	// recovery skips the locked row; recovery must then observe its live lease.
	secondStore, _ := NewAgentStore(e.pool)
	winner, err := secondStore.Claim(e.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if winner == nil {
		winner, err = secondStore.Claim(e.ctx, now)
		if err != nil || winner == nil {
			t.Fatal("neither expired nor pending work can progress")
		}
	}
	loser, err := e.store.Claim(e.ctx, now)
	if err != nil || loser != nil {
		t.Fatalf("same-thread overlapping claim: claimed=%v error=%v", loser != nil, err)
	}
	mustExec(t, e.ctx, e.pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'task','other')`, threadB, workspaceA, integrationA)
	mustExec(t, e.ctx, e.pool, `INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at) VALUES($1,$2,$3,$4)`, workspaceA, integrationA, threadB, now)
	other, err := e.store.Claim(e.ctx, now)
	if err != nil || other == nil || other.ThreadID != threadB {
		t.Fatal("busy thread blocked unrelated thread")
	}
	// Reissue the expired job's claim before testing token fencing. Expiry
	// alone is not the same as another worker having acquired a new token.
	if winner.ID != old.ID {
		if err := secondStore.CommitOutcome(e.ctx, *winner, agent.JobResult{Outcome: agent.OutcomeIgnore}); err != nil {
			t.Fatal(err)
		}
		recovered, err := e.store.Claim(e.ctx, now)
		if err != nil || recovered == nil || recovered.ID != old.ID {
			t.Fatal("expired job was not recovered after pending completion")
		}
	}
	if err := e.store.AddUsage(e.ctx, *old, domain.Usage{Input: 1}); !errors.Is(err, agent.ErrStaleAgentClaim) {
		t.Error("expired old claim retained mutation authority")
	}
}

// TC-4 (REQ-F11): force the original R16 snapshot race. A test-only trigger
// pauses expired-job recovery immediately before its lease UPDATE. A second
// worker can select the pending job while the first transaction is paused;
// only the production per-thread fence prevents overlapping claims.
func TestApproved_RecoveryUpdateBarrierRejectsConcurrentPendingClaim(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	e.job(t, now)
	old, err := e.store.Claim(e.ctx, now)
	if err != nil || old == nil {
		t.Fatal("initial claim failed")
	}
	e.job(t, now)
	mustExec(t, e.ctx, e.pool, `UPDATE agent_jobs SET locked_until=$2 WHERE id=$1`, old.ID, now.Add(-time.Second))
	mustExec(t, e.ctx, e.pool, fmt.Sprintf(`CREATE FUNCTION public.approved_pause_recovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=%d AND NEW.claim_token IS DISTINCT FROM OLD.claim_token THEN PERFORM pg_advisory_xact_lock(762150022); END IF; RETURN NEW; END $$; CREATE TRIGGER approved_pause BEFORE UPDATE ON agent_jobs FOR EACH ROW EXECUTE FUNCTION public.approved_pause_recovery()`, old.ID))
	gate, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(e.ctx, `SELECT pg_advisory_lock(762150022)`); err != nil {
		gate.Release()
		t.Fatal(err)
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			_, _ = gate.Exec(context.Background(), `SELECT pg_advisory_unlock(762150022)`)
			unlocked = true
			gate.Release()
		}
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	type claimResult struct {
		job *agent.AgentJob
		err error
	}
	done := make(chan claimResult, 1)
	go func() { j, err := e.store.Claim(ctx, now); done <- claimResult{j, err} }()
	// Cleanup must drain a blocked claim before the pool is closed on failure.
	drained := false
	defer func() {
		unlock()
		cancel()
		if !drained {
			<-done
		}
	}()
	barrier := false
	for !barrier && ctx.Err() == nil {
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=762150022 AND NOT granted)`).Scan(&barrier); err != nil {
			t.Fatal("cannot observe recovery barrier")
		}
		if !barrier {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	if !barrier {
		t.Fatal("recovery never reached the update barrier")
	}
	competitor, _ := NewAgentStore(e.pool)
	second, secondErr := competitor.Claim(ctx, now)
	unlock()
	first := <-done
	drained = true
	if first.err != nil || first.job == nil || first.job.ID != old.ID {
		t.Fatal("expired recovery failed after barrier release")
	}
	if secondErr != nil || second != nil {
		t.Errorf("pending job claimed during same-thread recovery: claimed=%v error=%v", second != nil, secondErr)
	}
	if first.job.ClaimToken == old.ClaimToken {
		t.Error("recovery did not rotate claim token")
	}
	if err := e.store.AddUsage(e.ctx, *old, domain.Usage{Input: 1}); !errors.Is(err, agent.ErrStaleAgentClaim) {
		t.Error("prior claim token not fenced after recovery")
	}
}

type approvedEffectTool struct {
	name    string
	tier    tenant.Tier
	effects atomic.Int32
	before  func(context.Context) error
	fail    error
}

func (x *approvedEffectTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: x.name, Tier: x.tier, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (x *approvedEffectTool) Repo(json.RawMessage) (string, error) { return "o/r", nil }
func (x *approvedEffectTool) Run(ctx context.Context, _ tools.Call) (tools.ToolOutput, error) {
	if x.before != nil {
		if err := x.before(ctx); err != nil {
			return tools.ToolOutput{}, err
		}
	}
	x.effects.Add(1)
	return tools.ToolOutput{Text: "business result", Summary: "effect recorded"}, x.fail
}

// TC-12 (REQ-F8), TC-13 (REQ-6), TC-14 (REQ-F9), TC-46 (REQ-F7),
// TC-47 (REQ-F10), TC-48 (REQ-F10): real durable store, fresh executors.
func TestApproved_DurableAuditAndAttemptBudgets(t *testing.T) {
	for _, mode := range []string{"write_budget", "dangerous_budget", "canonical_replay", "ambiguous", "inside_denial", "invalid_identifier"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool, store, job := newToolRunDB(t)
			tier := tenant.TierWrite
			if mode == "dangerous_budget" {
				tier = tenant.TierDangerous
			}
			x := &approvedEffectTool{name: "approved_effect", tier: tier}
			x.before = func(ctx context.Context) error {
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_runs WHERE agent_job_id=$1 AND status='started'`, job).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					t.Error("effect ran before durable started audit")
				}
				return nil
			}
			if mode == "ambiguous" {
				x.fail = &tools.ToolError{Code: "server_error", Retryable: true, Ambiguous: true}
			}
			if mode == "inside_denial" {
				x.fail = &tools.ToolError{Code: "denied_path", Denied: true}
			}
			reg, err := tools.NewRegistry(x)
			if err != nil {
				t.Fatal(err)
			}
			p := tenant.ToolPolicyConfig{MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"p": {Name: "p"}}, Repos: map[string]tenant.RepoGrant{"o/r": {Repo: "o/r", Account: "p", Allow: []string{x.name}}}}
			fresh := func() *tools.Executor {
				return tools.NewExecutor(reg, p, tools.DefaultLimits(), true, store, agent.NewRedactor([]string{"private-identifier-secret"}), job, workspaceA)
			}
			if mode == "invalid_identifier" {
				r := fresh().Call(ctx, 1, "private-identifier-secret/hostile", json.RawMessage(`{}`))
				if r.Status != tools.StatusDenied || !r.IsError {
					t.Error("invented tool not denied")
				}
				var name string
				if err := pool.QueryRow(ctx, `SELECT tool FROM tool_runs`).Scan(&name); err != nil {
					t.Fatal(err)
				}
				if name != "invalid_tool" {
					t.Error("raw invented tool identifier stored")
				}
				return
			}
			first := fresh().Call(ctx, 1, x.name, json.RawMessage(`{"repo":"o/r","n":1}`))
			want := tools.StatusOK
			if mode == "ambiguous" {
				want = tools.StatusUnknownOutcome
			}
			if mode == "inside_denial" {
				want = tools.StatusDenied
			}
			if first.Status != want || first.IsError != (want != tools.StatusOK) {
				t.Errorf("initial status=%s error=%v want=%s", first.Status, first.IsError, want)
			}
			if mode == "inside_denial" {
				var status string
				if err := pool.QueryRow(ctx, `SELECT status FROM tool_runs`).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != tools.StatusDenied {
					t.Error("inside-tool policy denial not durably denied")
				}
				return
			}
			if mode == "canonical_replay" || mode == "ambiguous" {
				r := fresh().Call(ctx, 2, x.name, json.RawMessage(`{ "n":1,"repo":"o/r" }`))
				if r.Status != want || x.effects.Load() != 1 {
					t.Error("canonical retry repeated or lost prior outcome")
				}
				return
			}
			if mode == "write_budget" {
				// A replay with remaining budget must not consume the second
				// durable write slot or produce a second business effect.
				if r := fresh().Call(ctx, 2, x.name, json.RawMessage(`{"n":1,"repo":"o/r"}`)); r.IsError || r.Status != tools.StatusOK || x.effects.Load() != 1 {
					t.Error("replay consumed an effect or lost successful outcome")
				}
				if r := fresh().Call(ctx, 2, x.name, json.RawMessage(`{"repo":"o/r","n":2}`)); r.IsError {
					t.Error("remaining durable write was denied")
				}
			}
			r := fresh().Call(ctx, 3, x.name, json.RawMessage(`{"repo":"o/r","n":3}`))
			if !r.IsError || r.Status != tools.StatusDenied {
				t.Error("fresh executor bypassed durable job budget")
			}
			wantEffects := int32(2)
			if mode == "dangerous_budget" {
				wantEffects = 1
			}
			if x.effects.Load() != wantEffects {
				t.Errorf("effects=%d want=%d", x.effects.Load(), wantEffects)
			}
		})
	}
}

// TC-49 (REQ-F12), TC-50 (REQ-F12): the original R9 reproduction is one
// external comment followed by 60 owner comments, with no newer external user.
func TestApproved_ExternalCommentNotDisplacedBySixtyOwnerComments(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	id := e.message(t, "must-not-lose", "external_user", "Cần xử lý lỗi này 🧠")
	for i := 0; i < 60; i++ {
		e.message(t, fmt.Sprintf("owner-only-%d", i), "human_owner", "owner-only context")
	}
	now := time.Now()
	e.job(t, now)
	j, err := e.store.Claim(e.ctx, now)
	if err != nil || j == nil {
		t.Fatal("claim failed")
	}
	jc, err := e.store.Load(e.ctx, *j)
	if err != nil {
		t.Fatal(err)
	}
	ownerCount := 0
	for _, m := range jc.History {
		if m.SenderRole == memory.SenderRoleHumanOwner {
			ownerCount++
		}
	}
	p := memory.BuildPacket(jc.Title, jc.TaskURL, jc.Summary, jc.History, jc.LastTriagedID, agent.CleanInput)
	if len(p.NewBatch) != 1 || p.NewBatch[0].ID != id || p.NewBatch[0].Content != "Cần xử lý lỗi này 🧠" {
		t.Error("untriaged comment disappeared behind owner chatter")
	}
	if ownerCount != 60 {
		t.Error("owner messages were not retained as thread context")
	}
}

// TC-49 (REQ-F12), TC-50 (REQ-F12): old external messages cannot be
// displaced by owner chatter; >100 external messages get a durable follow-up.
func TestApproved_UntriagedMessagesSurviveOwnerFloodAndFollowUp(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	first := e.message(t, "old-external", "external_user", "old external needs attention")
	for i := 0; i < 60; i++ {
		e.message(t, fmt.Sprintf("owner-%d", i), "human_owner", "owner context")
	}
	for i := 0; i < 104; i++ {
		e.message(t, fmt.Sprintf("external-%d", i), "external_user", "external question")
	}
	now := time.Now()
	e.job(t, now)
	j, err := e.store.Claim(e.ctx, now)
	if err != nil || j == nil {
		t.Fatal("claim failed")
	}
	jc, err := e.store.Load(e.ctx, *j)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	var through int64
	for _, m := range jc.History {
		if m.SenderRole == memory.SenderRoleExternalUser {
			ids[m.ID] = true
			if m.ID > through {
				through = m.ID
			}
		}
	}
	if !ids[first] || len(ids) != 100 {
		t.Errorf("first batch retained oldest=%v external_count=%d", ids[first], len(ids))
	}
	if err := e.store.CommitOutcome(e.ctx, *j, agent.JobResult{Outcome: agent.OutcomeNotify, ThroughMessageID: through, Text: "explicit batch handled"}); err != nil {
		t.Fatal(err)
	}
	next, err := e.store.Claim(e.ctx, time.Now().Add(time.Second))
	if err != nil || next == nil {
		t.Fatal("untriaged tail lost without follow-up job")
	}
	jc2, err := e.store.Load(e.ctx, *next)
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, m := range jc2.History {
		if m.ID > jc2.LastTriagedID && m.SenderRole == memory.SenderRoleExternalUser {
			remaining++
			if ids[m.ID] {
				t.Error("follow-up duplicated processed external comment")
			}
			ids[m.ID] = true
		}
	}
	if remaining != 5 || len(ids) != 105 {
		t.Errorf("follow-up remaining=%d total unique=%d", remaining, len(ids))
	}
}
