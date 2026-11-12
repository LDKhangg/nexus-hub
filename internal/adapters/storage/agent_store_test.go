package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

type agentEnv struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	store   *AgentStore
	actions *ActionStore
}

func newAgentEnv(t *testing.T, agentCfg string) agentEnv {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES
		($1,$2,'clickup','source',jsonb_build_object('clickup_team_id','team','webhook_id','w1','webhook_secret_ref','clickup-webhook','target_integration_id',($3::uuid)::text,'agent',$4::jsonb)),
		($3::uuid,$2,'telegram','control','{"chat_id":"-10042","bot_token_ref":"telegram"}')`, integrationA, workspaceA, integrationB, agentCfg)
	mustExec(t, ctx, pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id,title) VALUES($1,$2,$3,'task','abc','Login broken')`, threadA, workspaceA, integrationA)
	s, err := NewAgentStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	s.SetKnownTools(map[string]tenant.Tier{"fake_write": tenant.TierWrite, "code_search": tenant.TierRead})
	a, _ := NewActionStore(pool)
	return agentEnv{ctx, pool, s, a}
}

func (e agentEnv) message(t *testing.T, ext, role, content string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO message_logs(thread_id,external_message_id,sender_name,sender_role,content,metadata) VALUES($1,$2,'Alice',$3,$4,'{"task_url":"https://app.clickup.com/t/abc"}') RETURNING id`, threadA, ext, role, content).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e agentEnv) job(t *testing.T, runAt time.Time) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at) VALUES($1,$2,$3,$4) RETURNING id`, workspaceA, integrationA, threadA, runAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

const writeCfg = `{"enabled":true,"tools":{"enabled":[],"github":{"accounts":{"p":{"type":"pat","token_ref":"gh","login":"x","commit_name":"Nexus","commit_email":"n@example.invalid"}},"repos":{"o/r":{"account":"p","managed":true,"allow":["fake_write"]}}}}}`

const enabledCfg = `{"enabled":true,"tools":{"enabled":["code_search"]}}`

func TestAgentStore_ClaimRespectsRunAt(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	id := e.job(t, now.Add(10*time.Second))
	if j, err := e.store.Claim(e.ctx, now); err != nil || j != nil {
		t.Fatalf("early claim %+v %v", j, err)
	}
	j, err := e.store.Claim(e.ctx, now.Add(11*time.Second))
	if err != nil || j == nil || j.ID != id || j.Attempts != 1 || j.WorkspaceID != workspaceA || j.ThreadID != threadA || j.SourceIntegrationID != integrationA {
		t.Fatalf("%+v %v", j, err)
	}
	if again, _ := e.store.Claim(e.ctx, now.Add(12*time.Second)); again != nil {
		t.Fatal("claimed twice")
	}
}

func TestAgentStore_LeaseRecovery(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	e.job(t, now)
	first, _ := e.store.Claim(e.ctx, now)
	// A new comment's pending job must wait while the thread's lease is live.
	e.job(t, now)
	if j, _ := e.store.Claim(e.ctx, now.Add(time.Minute)); j != nil {
		t.Fatalf("thread not serialized: %+v", j)
	}
	second, err := e.store.Claim(e.ctx, now.Add(AgentLease+time.Second))
	if err != nil || second == nil || second.ID != first.ID || second.Attempts != 2 {
		t.Fatalf("recovered %+v %v", second, err)
	}
	if err := e.store.CommitOutcome(e.ctx, *first, agent.JobResult{Outcome: agent.OutcomeIgnore}); !errors.Is(err, agent.ErrStaleAgentClaim) {
		t.Fatalf("old claim: %v", err)
	}
	if err := e.store.AddUsage(e.ctx, *first, domain.Usage{Input: 1}); !errors.Is(err, agent.ErrStaleAgentClaim) {
		t.Fatalf("old claim usage: %v", err)
	}
	// Retry keeps the thread serialized until the retry time.
	if err := e.store.Retry(e.ctx, *second, now.Add(AgentLease+time.Minute), "llm_error"); err != nil {
		t.Fatal(err)
	}
	if j, _ := e.store.Claim(e.ctx, now.Add(AgentLease+30*time.Second)); j != nil {
		t.Fatalf("claimed during retry wait: %+v", j)
	}
	third, _ := e.store.Claim(e.ctx, now.Add(AgentLease+2*time.Minute))
	if third == nil || third.ID != first.ID || third.Attempts != 3 {
		t.Fatalf("retry reclaim %+v", third)
	}
}

func TestAgentStore_NewBatchAfterThrough(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	m1 := e.message(t, "c1", "external_user", "first")
	m2 := e.message(t, "c2", "external_user", "second")
	now := time.Now()
	e.job(t, now)
	j, _ := e.store.Claim(e.ctx, now)
	jc, err := e.store.Load(e.ctx, *j)
	if err != nil || jc.LastTriagedID != 0 || len(jc.History) != 2 || jc.Title != "Login broken" || jc.TaskURL != "https://app.clickup.com/t/abc" || !jc.Config.Enabled {
		t.Fatalf("%+v %v", jc, err)
	}
	if err := e.store.CommitOutcome(e.ctx, *j, agent.JobResult{Outcome: agent.OutcomeIgnore, ThroughMessageID: m2}); err != nil {
		t.Fatal(err)
	}
	e.message(t, "c3", "external_user", "third")
	e.job(t, now)
	j2, _ := e.store.Claim(e.ctx, now.Add(time.Second))
	jc, _ = e.store.Load(e.ctx, *j2)
	if jc.LastTriagedID != m2 || jc.History[0].ID != m1 {
		t.Fatalf("%+v", jc)
	}
	p := memory.BuildPacket(jc.Title, jc.TaskURL, jc.Summary, jc.History, jc.LastTriagedID, agent.CleanInput)
	if len(p.NewBatch) != 1 || p.NewBatch[0].Content != "third" {
		t.Fatalf("batch %+v", p.NewBatch)
	}
}

func TestAgentStore_UsageAccumulates(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	e.job(t, now)
	j, _ := e.store.Claim(e.ctx, now)
	_ = e.store.AddUsage(e.ctx, *j, domain.Usage{Input: 100, Output: 10, CacheRead: 5, CacheWrite: 1})
	_ = e.store.AddUsage(e.ctx, *j, domain.Usage{Input: 50, Output: 5})
	_ = e.store.Retry(e.ctx, *j, now, "llm_error")
	j2, _ := e.store.Claim(e.ctx, now.Add(time.Second))
	if j2 == nil || j2.Usage != (domain.Usage{Input: 150, Output: 15, CacheRead: 5, CacheWrite: 1}) {
		t.Fatalf("%+v", j2)
	}
}

func TestAgentStore_Fallback(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	m := e.message(t, "c1", "external_user", "help")
	now := time.Now()
	id := e.job(t, now)
	j, _ := e.store.Claim(e.ctx, now)
	if err := e.store.CommitFallback(e.ctx, *j, "llm_refused", "⚠️ AI triage unavailable (llm_refused)", m); err != nil {
		t.Fatal(err)
	}
	var status, code string
	var through int64
	if err := e.pool.QueryRow(e.ctx, `SELECT status,last_error_code,through_message_id FROM agent_jobs WHERE id=$1`, id).Scan(&status, &code, &through); err != nil || status != "failed" || code != "llm_refused" || through != m {
		t.Fatalf("%s %s %d %v", status, code, through, err)
	}
	a, err := e.actions.Claim(e.ctx, time.Now().Add(time.Second))
	if err != nil || a == nil || !strings.HasPrefix(a.Text, "⚠️ AI triage unavailable") {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestActionStore_ClaimMarkSentDedupe(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	now := time.Now()
	e.job(t, now)
	j, _ := e.store.Claim(e.ctx, now)
	if err := e.store.CommitOutcome(e.ctx, *j, agent.JobResult{Outcome: agent.OutcomeNotify, Text: "🧠 hi"}); err != nil {
		t.Fatal(err)
	}
	// A duplicate enqueue for the same job is ignored.
	mustExec(t, e.ctx, e.pool, `INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key) VALUES($1,$2,'telegram_message','{"text":"dup"}',$3) ON CONFLICT DO NOTHING`, workspaceA, integrationB, "agent_job:"+itoa(j.ID))
	if n := countRows(t, e.ctx, e.pool, "outbound_actions"); n != 1 {
		t.Fatalf("rows %d", n)
	}
	a, err := e.actions.Claim(e.ctx, time.Now().Add(time.Second))
	if err != nil || a == nil || a.Text != "🧠 hi" || a.IntegrationID != integrationB || a.Attempts != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	target, err := e.actions.ResolveTarget(e.ctx, *a)
	if err != nil || target.ChatID != "-10042" || target.BotTokenRef != "telegram" {
		t.Fatalf("%+v %v", target, err)
	}
	if err := e.actions.MarkSent(e.ctx, *a, 555); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.actions.Claim(e.ctx, now.Add(time.Hour)); again != nil {
		t.Fatal("sent action reclaimed")
	}
	if err := e.actions.MarkSent(e.ctx, *a, 556); err == nil {
		t.Fatal("stale mark accepted")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// --- orchestrator against the real stores ---

type planTriager struct{ d agent.TriageDecision }

func (p planTriager) Triage(context.Context, memory.ContextPacket, string, bool) (agent.TriageDecision, domain.Usage, error) {
	return p.d, domain.Usage{Input: 10, Output: 2}, nil
}

type planInvestigator struct {
	attempt atomic.Int32
	call    string
	failOn  int32
	text    string
}

func (p *planInvestigator) Investigate(ctx context.Context, _ memory.ContextPacket, _ agent.TriageDecision, _ string, _ []tools.ToolSpec, exec *tools.Executor, _ agent.TokenBudget) (agent.ActorReport, error) {
	n := p.attempt.Add(1)
	var r agent.ActorReport
	if p.call != "" {
		res := exec.Call(ctx, 1, p.call, json.RawMessage(`{"branch":"nexus/x"}`))
		r.Calls = append(r.Calls, agent.CallRecord{Step: 1, Name: p.call, Status: res.Status, Tier: res.Tier, IsError: res.IsError})
	}
	r.Usage = domain.Usage{Input: 100, Output: 20}
	if n == p.failOn {
		return r, &agent.TriageError{Code: agent.CodeLLMError, Retryable: true}
	}
	r.Text = p.text
	return r, nil
}

type countingWrite struct{ runs atomic.Int32 }

func (c *countingWrite) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "fake_write", Description: "push", InputSchema: json.RawMessage(`{"type":"object"}`), Tier: tenant.TierWrite}
}
func (c *countingWrite) Repo(json.RawMessage) (string, error) { return "o/r", nil }
func (c *countingWrite) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	c.runs.Add(1)
	return tools.ToolOutput{Text: "pushed", Summary: "pushed nexus/x"}, nil
}

func orchestrate(t *testing.T, e agentEnv, tool tools.Tool, inv *planInvestigator, redactor *agent.Redactor) *agent.Orchestrator {
	t.Helper()
	reg, err := tools.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := NewToolRunStore(e.pool)
	tr := planTriager{d: agent.TriageDecision{Category: agent.CategoryInvestigate, Urgency: agent.UrgencyHigh, Confidence: 0.9, Summary: "login 500", Goal: "find cause"}}
	return agent.NewOrchestrator(e.store, func(context.Context, string) (agent.Triager, agent.Investigator, error) { return tr, inv, nil }, reg, runs, redactor, nil,
		agent.OrchestratorSettings{PollInterval: time.Millisecond, Limits: tools.DefaultLimits(), MaxInputTokens: 60000, MaxOutputTokens: 6000})
}

// runUntil drives the orchestrator until cond holds.
func runUntil(t *testing.T, o *agent.Orchestrator, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	for !cond() {
		if ctx.Err() != nil {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestAgentStore_CommitInvestigate(t *testing.T) {
	e := newAgentEnv(t, writeCfg)
	e.message(t, "c1", "external_user", "login is broken")
	e.job(t, time.Now().Add(-time.Second))
	secret := "s3cr3t-planted-value"
	red := agent.NewRedactor([]string{secret})
	inv := &planInvestigator{call: "fake_write", text: "- cause: nil map\n- leaked " + secret}
	tool := &countingWrite{}
	o := orchestrate(t, e, tool, inv, red)
	runUntil(t, o, func() bool { return countRows(t, e.ctx, e.pool, "outbound_actions") == 1 })
	a, _ := e.actions.Claim(e.ctx, time.Now().Add(time.Second))
	for _, want := range []string{"🔎 Nexus · Login broken", "high · login 500", "- cause: nil map", "Tools: fake_write ✓ (write)", "Tokens: 110/22", "https://app.clickup.com/t/abc"} {
		if !strings.Contains(a.Text, want) {
			t.Fatalf("missing %q:\n%s", want, a.Text)
		}
	}
	if strings.Contains(a.Text, secret) {
		t.Fatal("planted secret sent")
	}
	var roles []string
	rows, _ := e.pool.Query(e.ctx, `SELECT sender_role, content FROM message_logs WHERE sender_role LIKE 'ai_%' ORDER BY id`)
	for rows.Next() {
		var role, content string
		_ = rows.Scan(&role, &content)
		if strings.Contains(content, secret) {
			t.Fatal("planted secret stored")
		}
		roles = append(roles, role)
	}
	if strings.Join(roles, ",") != "ai_router,ai_actor" {
		t.Fatalf("roles %v", roles)
	}
	var in, out int
	_ = e.pool.QueryRow(e.ctx, `SELECT input_tokens, output_tokens FROM agent_jobs`).Scan(&in, &out)
	if in != 110 || out != 22 {
		t.Fatalf("tokens %d/%d", in, out)
	}
}

func TestOrchestrator_RetryDoesNotRepeatWrite(t *testing.T) {
	e := newAgentEnv(t, writeCfg)
	e.message(t, "c1", "external_user", "please push")
	e.job(t, time.Now().Add(-time.Second))
	tool := &countingWrite{}
	inv := &planInvestigator{call: "fake_write", failOn: 1, text: "done"}
	o := orchestrate(t, e, tool, inv, nil)
	// Attempt 1 pushes then fails; the retry waits 5s (backoff), so move the
	// retry time forward instead of sleeping.
	runUntil(t, o, func() bool {
		var code *string
		_ = e.pool.QueryRow(e.ctx, `SELECT last_error_code FROM agent_jobs`).Scan(&code)
		return code != nil && *code == "llm_error"
	})
	mustExec(t, e.ctx, e.pool, `UPDATE agent_jobs SET locked_until = now() - interval '1 second'`)
	runUntil(t, o, func() bool { return countRows(t, e.ctx, e.pool, "outbound_actions") == 1 })
	if tool.runs.Load() != 1 || inv.attempt.Load() != 2 {
		t.Fatalf("write ran %d times over %d attempts", tool.runs.Load(), inv.attempt.Load())
	}
	var statuses []string
	rows, _ := e.pool.Query(e.ctx, `SELECT status FROM tool_runs ORDER BY id`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		statuses = append(statuses, s)
	}
	if strings.Join(statuses, ",") != "ok" {
		t.Fatalf("tool_runs %v", statuses)
	}
}

func TestAgentStore_SaveSummaryRejectsMovedCursor(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	var ids []int64
	for i := 0; i < 9; i++ {
		ids = append(ids, e.message(t, "m"+itoa(int64(i)), "external_user", "msg"))
	}
	summary, cursor, cands, err := e.store.SummaryCandidates(e.ctx, workspaceA, threadA)
	if err != nil || summary != "" || cursor != 0 || len(cands) != 6 || cands[5].ID != ids[5] {
		t.Fatalf("%q %d %d %v", summary, cursor, len(cands), err)
	}
	if err := e.store.SaveSummary(e.ctx, workspaceA, threadA, 0, ids[5], "sum one"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SaveSummary(e.ctx, workspaceA, threadA, 0, ids[6], "stale"); !errors.Is(err, memory.ErrSummaryCursorMoved) {
		t.Fatalf("moved cursor accepted: %v", err)
	}
	summary, cursor, cands, _ = e.store.SummaryCandidates(e.ctx, workspaceA, threadA)
	if summary != "sum one" || cursor != ids[5] || len(cands) != 0 {
		t.Fatalf("%q %d %d", summary, cursor, len(cands))
	}
	jc, _ := e.store.Load(e.ctx, agent.AgentJob{WorkspaceID: workspaceA, ThreadID: threadA, SourceIntegrationID: integrationA})
	if jc.Summary != "sum one" {
		t.Fatalf("load summary %q", jc.Summary)
	}
}
