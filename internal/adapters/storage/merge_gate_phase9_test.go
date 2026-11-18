package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 9 Batch B: the github.merge task under the policy engine with the
// real stores (policy rule sets, policy gates, tool run audit, workflow
// executions) and fakes for GitHub, Telegram and the step waker
// (REQ-9..REQ-12, E2E-004, E2E-009).

const (
	mgHead      = "0123456789abcdef0123456789abcdef01234567"
	mgMergeSHA  = "1111111122222222333333334444444455555555"
	mgOwnerUser = "019a6690-abcd-7000-8000-000000000401"
	mgChat      = int64(-1001)
	mgThread    = int64(7)
	mgStepKey   = "run_01"
)

// ---- fakes ----------------------------------------------------------------

type mgSent struct {
	Chat    behavior.ChatRef
	Text    string
	Buttons [][]behavior.Button
	ID      int64
}

type mgEdit struct {
	Chat behavior.ChatRef
	ID   int64
	Text string
}

// mgMessenger is a fake Telegram: it records sends and edits.
type mgMessenger struct {
	mu       sync.Mutex
	next     int64
	failSend bool
	sent     []mgSent
	edits    []mgEdit
	removed  []int64
}

func (m *mgMessenger) SendMessage(_ context.Context, to behavior.ChatRef, msg behavior.OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSend {
		return 0, errors.New("telegram_unavailable")
	}
	m.next++
	id := 500 + m.next
	m.sent = append(m.sent, mgSent{Chat: to, Text: msg.Text, Buttons: msg.Buttons, ID: id})
	return id, nil
}
func (m *mgMessenger) RemoveButtons(_ context.Context, _ behavior.ChatRef, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, id)
	return nil
}
func (m *mgMessenger) AnswerCallback(context.Context, behavior.ChatRef, string, string) error {
	return nil
}
func (m *mgMessenger) EditMessageText(_ context.Context, to behavior.ChatRef, id int64, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits = append(m.edits, mgEdit{Chat: to, ID: id, Text: text})
	return nil
}
func (m *mgMessenger) counts() (sent, edits int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent), len(m.edits)
}
func (m *mgMessenger) lastEdit() mgEdit {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.edits) == 0 {
		return mgEdit{}
	}
	return m.edits[len(m.edits)-1]
}

// mgGitHub is a fake GitHub API for acme/api#12: the head, the checks and
// the merge call are scripted, and every call is counted.
type mgGitHub struct {
	mu     sync.Mutex
	head   string
	runs   string
	calls  int
	merges int
	bodies []map[string]string
}

func newMGGitHub() *mgGitHub {
	return &mgGitHub{head: mgHead, runs: `{"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`}
}

func (g *mgGitHub) Do(_ context.Context, _ string, _ tenant.GitHubAccount, _ string, _ map[string]string, method, path string, body, out any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	var raw string
	switch {
	case method == "GET" && path == "/repos/acme/api/pulls/12":
		raw = fmt.Sprintf(`{"number":12,"title":"Fix login","state":"open","draft":false,"user":{"login":"alice"},"base":{"ref":"develop"},"head":{"ref":"nexus/fix","sha":%q},"mergeable":true,"mergeable_state":"clean"}`, g.head)
	case method == "GET" && strings.Contains(path, "/check-runs"):
		raw = g.runs
	case method == "GET" && strings.HasSuffix(path, "/status"):
		raw = `{"state":"success","statuses":[]}`
	case method == "PUT" && path == "/repos/acme/api/pulls/12/merge":
		g.merges++
		if b, ok := body.(map[string]string); ok {
			g.bodies = append(g.bodies, b)
		}
		raw = fmt.Sprintf(`{"merged":true,"sha":%q}`, mgMergeSHA)
	default:
		return fmt.Errorf("unexpected fake GitHub call %s %s", method, path)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal([]byte(raw), out)
}

func (g *mgGitHub) state() (calls, merges int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.merges
}

type mgWaker struct {
	mu    sync.Mutex
	calls []string
}

func (w *mgWaker) WakeWorkerStep(_ context.Context, ws string, exec int64, step string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, fmt.Sprintf("%s/%d/%s", ws, exec, step))
	return nil
}

type mgPolicies struct {
	mu  sync.Mutex
	cfg tenant.AgentConfig
}

func (p *mgPolicies) RunPolicy(context.Context, string, string) (tenant.AgentConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg, nil
}
func (p *mgPolicies) set(f func(*tenant.AgentConfig)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(&p.cfg)
}

func mgConfig() tenant.AgentConfig {
	return tenant.AgentConfig{Enabled: true, ClickUpTokenRef: "fake-clickup", Tools: tenant.ToolPolicyConfig{
		Enabled: []string{"github_pr_merge"}, MaxWritesPerJob: 2,
		Accounts: map[string]tenant.GitHubAccount{"p": {Name: "p"}},
		Repos: map[string]tenant.RepoGrant{"acme/api": {Repo: "acme/api", Account: "p", Allow: []string{"github_pr_merge"},
			MergeMethods: []string{"squash"}, MergeBaseBranches: []string{"develop"}, RequireChecksGreen: true}},
	}}
}

// ---- environment ------------------------------------------------------------

type mgOpts struct {
	noOwner bool
	noGates bool
}

type mgEnv struct {
	*b2Env
	store    *PolicyStore
	runs     *ToolRunStore
	msgr     *mgMessenger
	gh       *mgGitHub
	waker    *mgWaker
	policies *mgPolicies
	engine   *policy.Engine
	reg      *tools.Registry
	merge    *behavior.OrchestratorMerge
	actions  *behavior.OrchestratorActions
	gates    *behavior.PolicyGates
	exec     int64
}

func newMGEnv(t *testing.T, o mgOpts) *mgEnv {
	t.Helper()
	e := newB2Env(t)
	if !o.noOwner {
		mustExec(t, e.ctx, e.pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner')`, mgOwnerUser)
		mustExec(t, e.ctx, e.pool, `INSERT INTO workspace_members(user_id,workspace_id,role) VALUES($1,$2,'owner')`, mgOwnerUser, workspaceA)
	}
	store, err := NewPolicyStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyRuleSet(e.ctx, workspaceA, policy.DefaultV1(), policy.CreatedByInit, 0); err != nil {
		t.Fatal(err)
	}
	runs, err := NewWorkflowToolRunStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	gh := newMGGitHub()
	reg, err := tools.NewRegistry(githubtools.NewDangerousTools(gh, p9San{})...)
	if err != nil {
		t.Fatal(err)
	}
	m := &mgEnv{b2Env: e, store: store, runs: runs, msgr: &mgMessenger{}, gh: gh, waker: &mgWaker{},
		policies: &mgPolicies{cfg: mgConfig()}, engine: policy.NewEngine(store, p9Known), reg: reg}
	m.gates = &behavior.PolicyGates{Store: store, Messenger: m.msgr, Waker: m.waker, OwnerTelegramUserID: 555}
	m.exec = e.startTask(t)
	m.build(o)
	return m
}

// build creates the executor; calling it again models a server restart.
func (m *mgEnv) build(o mgOpts) {
	mg := &behavior.OrchestratorMerge{Registry: m.reg, Audit: m.runs, Sanitizer: p9San{}, Limits: tools.DefaultLimits(),
		Policies: m.policies, Engine: m.engine, Gates: m.gates}
	if o.noGates {
		mg.Gates = nil
	}
	m.merge = mg
	m.actions = &behavior.OrchestratorActions{Merge: mg}
}

func (m *mgEnv) run(exec int64, repo string, number int64, sha string) orchestrator.TaskRun {
	return orchestrator.TaskRun{
		WorkspaceID: workspaceA, ExecutionID: exec, StepKey: mgStepKey,
		Task:  orchestrator.PlanTask{Key: "land", Title: "Land the PR", Capabilities: []string{orchestrator.CapGitHubMerge}, Merge: &orchestrator.MergeTarget{Repo: repo, Number: number, HeadSHA: sha}},
		Route: orchestrator.Route{WorkspaceID: workspaceA, ExecutionID: exec, IntegrationID: integrationA, ChatID: mgChat, ThreadID: mgThread},
	}
}

func (m *mgEnv) task() orchestrator.TaskRun { return m.run(m.exec, "acme/api", 12, mgHead) }

// ask runs the task and requires it to wait on a gate; it returns the gate.
func (m *mgEnv) ask(t *testing.T) (behavior.PolicyGateRecord, string) {
	t.Helper()
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	return m.waitingGate(t, out)
}

func (m *mgEnv) waitingGate(t *testing.T, out orchestrator.TaskOutcome) (behavior.PolicyGateRecord, string) {
	t.Helper()
	if out.Succeeded || out.Blocked || !strings.HasPrefix(out.Wait, "gate:") {
		t.Fatalf("expected a wait on a gate, got %+v", out)
	}
	var id int64
	if _, err := fmt.Sscanf(out.Wait, "gate:%d", &id); err != nil {
		t.Fatalf("wait ref %q: %v", out.Wait, err)
	}
	rec, err := m.store.PolicyGate(m.ctx, workspaceA, id)
	if err != nil {
		t.Fatal(err)
	}
	return rec, out.Wait
}

func (m *mgEnv) update() behavior.InboundUpdate {
	return behavior.InboundUpdate{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: mgChat, ThreadID: mgThread}
}

func (m *mgEnv) approvalOf(rec behavior.PolicyGateRecord) behavior.Approval {
	return behavior.Approval{ID: rec.ID, WorkspaceID: workspaceA, Kind: behavior.ApprovalPolicyGate, Nonce: rec.Nonce, ExpiresAt: rec.ExpiresAt, Decision: rec.Decision}
}

func (m *mgEnv) press(rec behavior.PolicyGateRecord, kind behavior.CallbackKind) (toast, code string, keep bool) {
	return m.pressWith(m.update(), rec.MessageID, rec, kind)
}

func (m *mgEnv) pressWith(u behavior.InboundUpdate, msgID int64, rec behavior.PolicyGateRecord, kind behavior.CallbackKind) (string, string, bool) {
	return m.gates.HandlePress(m.ctx, u, msgID, behavior.CallbackData{Kind: kind, Nonce: rec.Nonce, Option: -1}, m.approvalOf(rec))
}

func (m *mgEnv) reload(t *testing.T, id int64) behavior.PolicyGateRecord {
	t.Helper()
	rec, err := m.store.PolicyGate(m.ctx, workspaceA, id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (m *mgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return p9Count(t, m.ctx, m.pool, sql, args...)
}

func (m *mgEnv) gateRows(t *testing.T) int {
	return m.count(t, `SELECT count(*) FROM approvals WHERE kind='policy_gate'`)
}

// approved asks, presses Duyệt and returns the gate and the wait reference.
func (m *mgEnv) approved(t *testing.T) (behavior.PolicyGateRecord, string) {
	t.Helper()
	rec, ref := m.ask(t)
	if toast, code, _ := m.press(rec, behavior.CallbackApprove); toast != "Đã duyệt." || code != "" {
		t.Fatalf("approve press: %q %q", toast, code)
	}
	return m.reload(t, rec.ID), ref
}

func (m *mgEnv) denyRules(t *testing.T) {
	t.Helper()
	rs := p9Parse(t, "schema: policy/v1\ndefaults:\n  read: allow\n  write: allow\n  dangerous: approval_required\nactions:\n  github_pr_merge: deny\napproval_ttl_hours: 24\n")
	if _, _, err := m.store.ApplyRuleSet(m.ctx, workspaceA, rs, policy.CreatedByCLI, 0); err != nil {
		t.Fatal(err)
	}
}

func (m *mgEnv) toolRunStatuses(t *testing.T) string {
	t.Helper()
	rows, err := m.pool.Query(m.ctx, `SELECT status||':'||COALESCE(error_code,'') FROM tool_runs WHERE tool='github_pr_merge' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func (m *mgEnv) requireNoMerge(t *testing.T) {
	t.Helper()
	if _, merges := m.gh.state(); merges != 0 {
		t.Fatalf("the PR was merged %d times", merges)
	}
}

func requireBlocked(t *testing.T, out orchestrator.TaskOutcome, code string) {
	t.Helper()
	if out.Succeeded || !out.Blocked || out.Wait != "" || out.ReasonCode != code {
		t.Fatalf("want blocked %q, got %+v", code, out)
	}
}

// ---- E2E-004 ----------------------------------------------------------------

// TC-E4-1 (REQ-9..11, E2E-004): ask in the execution's topic, wait, Duyệt, resume, revalidate, merge once with the head SHA,
// success with the merge SHA as evidence; a restart after the merge never merges twice.
func TestP9E2E004_MergeWaitsForOwnerThenMergesOnce(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, ref := m.ask(t)

	// The gate is bound to the exact merge, the execution and the owner.
	var action, resource, hash string
	var execID int64
	var requested *string
	var decision *string
	if err := m.pool.QueryRow(m.ctx, `SELECT policy_action, resource_key, input_sha256, execution_id, requested_user_id::text, decision FROM approvals WHERE id=$1`, rec.ID).
		Scan(&action, &resource, &hash, &execID, &requested, &decision); err != nil {
		t.Fatal(err)
	}
	if action != "github_pr_merge" || !strings.Contains(resource, "acme/api") || !strings.Contains(resource, "12") || len(hash) != 64 ||
		execID != m.exec || requested == nil || *requested != mgOwnerUser || decision != nil {
		t.Fatalf("gate binding: %q %q %q %d %v %v", action, resource, hash, execID, requested, decision)
	}
	if time.Until(rec.ExpiresAt) < 23*time.Hour || time.Until(rec.ExpiresAt) > 25*time.Hour {
		t.Fatalf("expiry %v is not about 24 h away", rec.ExpiresAt)
	}
	// The message went to the execution's topic with the two buttons and names the exact head.
	if sent, _ := m.msgr.counts(); sent != 1 {
		t.Fatalf("%d messages sent", sent)
	}
	msg := m.msgr.sent[0]
	if msg.Chat.ChatID != mgChat || msg.Chat.ThreadID != mgThread || !strings.Contains(msg.Text, "🔐 Cần duyệt: merge PR acme/api#12 (head 0123456)") ||
		!strings.Contains(msg.Text, "actions.github_pr_merge") {
		t.Fatalf("approval message %+v", msg)
	}
	if len(msg.Buttons) != 1 || len(msg.Buttons[0]) != 2 || msg.Buttons[0][0].Text != "Duyệt" || msg.Buttons[0][1].Text != "Từ chối" {
		t.Fatalf("buttons %+v", msg.Buttons)
	}
	for _, b := range msg.Buttons[0] {
		if strings.Contains(b.Data, mgHead) || len(b.Data) > 64 {
			t.Fatalf("button data %q", b.Data)
		}
	}
	if strings.Contains(msg.Text, mgHead) {
		t.Fatal("the message shows the full SHA")
	}
	if rec.MessageID != msg.ID {
		t.Fatalf("message id not stored: %d vs %d", rec.MessageID, msg.ID)
	}
	m.requireNoMerge(t)
	if got := m.toolRunStatuses(t); got != "denied:approval_required" {
		t.Fatalf("tool runs %q", got)
	}
	if n := m.count(t, `SELECT count(*) FROM policy_decisions WHERE action='github_pr_merge' AND effect='approval_required' AND execution_id=$1`, m.exec); n != 1 {
		t.Fatalf("%d approval_required decisions", n)
	}

	// Asking again from the same execution resumes the open gate: no second gate, no second message.
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.waitingGate(t, out); again.ID != rec.ID || m.gateRows(t) != 1 {
		t.Fatalf("a second gate was made: %d vs %d (%d rows)", again.ID, rec.ID, m.gateRows(t))
	}
	if sent, _ := m.msgr.counts(); sent != 1 {
		t.Fatalf("the open gate was asked again: %d messages", sent)
	}
	// While open, a resume keeps waiting.
	out, err = m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil || out.Wait != ref || out.Blocked || out.Succeeded {
		t.Fatalf("resume of an open gate: %+v %v", out, err)
	}
	m.requireNoMerge(t)

	// The owner presses Duyệt.
	toast, code, keep := m.press(rec, behavior.CallbackApprove)
	if toast != "Đã duyệt." || code != "" || keep {
		t.Fatalf("press: %q %q keep=%v", toast, code, keep)
	}
	if e := m.msgr.lastEdit(); e.ID != msg.ID || !strings.HasPrefix(e.Text, "✅ Đã duyệt: merge PR acme/api#12 (head 0123456)") {
		t.Fatalf("message edit %+v", e)
	}
	decided := m.reload(t, rec.ID)
	if decided.Decision != behavior.DecisionApproved || decided.DecidedBy != "telegram:555" || decided.Consumed {
		t.Fatalf("decided gate %+v", decided)
	}
	var by *string
	if err := m.pool.QueryRow(m.ctx, `SELECT decided_user_id::text FROM approvals WHERE id=$1`, rec.ID).Scan(&by); err != nil || by == nil || *by != mgOwnerUser {
		t.Fatalf("decided_user_id %v %v", by, err)
	}
	if len(m.waker.calls) != 1 || m.waker.calls[0] != fmt.Sprintf("%s/%d/%s", workspaceA, m.exec, mgStepKey) {
		t.Fatalf("step wake %v", m.waker.calls)
	}
	m.requireNoMerge(t) // the press alone never merges

	// Resume: revalidate, then merge once with the exact head.
	out, err = m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Succeeded || out.Blocked || out.Wait != "" || len(out.Evidence) != 1 || !strings.Contains(out.Summary, "acme/api#12") {
		t.Fatalf("outcome %+v", out)
	}
	if want := "github:merge:acme/api#12:" + mgMergeSHA; out.Evidence[0].Kind != orchestrator.EvidenceArtifact || out.Evidence[0].ID != want {
		t.Fatalf("evidence %+v want %s", out.Evidence, want)
	}
	if _, merges := m.gh.state(); merges != 1 || m.gh.bodies[0]["sha"] != mgHead || m.gh.bodies[0]["merge_method"] != "squash" {
		t.Fatalf("merge calls %d bodies %v", merges, m.gh.bodies)
	}
	if !m.reload(t, rec.ID).Consumed {
		t.Fatal("the approval was not consumed")
	}
	if got := m.toolRunStatuses(t); got != "denied:approval_required,denied:approval_required,ok:" {
		t.Fatalf("tool runs %q", got)
	}
	// The revalidation is a recorded decision that names the approval and the tool run.
	if n := m.count(t, `SELECT count(*) FROM policy_decisions WHERE approval_id=$1 AND tool_run_id IS NOT NULL AND effect<>'deny'`, rec.ID); n != 1 {
		t.Fatalf("%d revalidation decisions", n)
	}

	// Restart after the approval: a new executor replays the merge; the PR is not merged twice.
	m.build(mgOpts{})
	for i := 0; i < 2; i++ {
		again, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil || !again.Succeeded || len(again.Evidence) != 1 || again.Evidence[0].ID != out.Evidence[0].ID {
			t.Fatalf("replay %d: %+v %v", i, again, err)
		}
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("a restart merged again: %d merges", merges)
	}
	if n := m.count(t, `SELECT count(*) FROM tool_runs WHERE tool='github_pr_merge' AND status='ok'`); n != 1 {
		t.Fatalf("%d ok merge runs", n)
	}
	// A retry of the whole task after the merge (attempt stopped before the wait was recorded) also replays.
	whole, err := m.actions.ResumeTask(m.ctx, m.task(), "")
	if err != nil || !whole.Succeeded {
		t.Fatalf("resume without a wait ref: %+v %v", whole, err)
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("resume without a wait ref merged again: %d", merges)
	}
}

// TC-E4-2 (REQ-10, REQ-11): Từ chối blocks the task with approval_rejected, edits the message with ❌ and merges nothing.
func TestP9E2E004_RejectBlocksWithoutMerge(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, ref := m.ask(t)
	toast, code, keep := m.press(rec, behavior.CallbackReject)
	if toast != "Đã từ chối." || code != "" || keep {
		t.Fatalf("press: %q %q %v", toast, code, keep)
	}
	if e := m.msgr.lastEdit(); !strings.HasPrefix(e.Text, "❌ Đã từ chối: merge PR acme/api#12") {
		t.Fatalf("edit %+v", e)
	}
	out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalRejected)
	m.requireNoMerge(t)
	if calls, _ := m.gh.state(); calls != 0 {
		t.Fatalf("GitHub was called %d times after a rejection", calls)
	}
	if got := m.reload(t, rec.ID); got.Decision != behavior.DecisionRejected || got.Consumed {
		t.Fatalf("gate %+v", got)
	}
	// A later press of the other button changes nothing.
	if toast, code, _ := m.press(rec, behavior.CallbackApprove); toast != "Đã xử lý hoặc đã hết hạn." || code != "stale" {
		t.Fatalf("approve after reject: %q %q", toast, code)
	}
	if m.reload(t, rec.ID).Decision != behavior.DecisionRejected {
		t.Fatal("the rejection was overturned")
	}
}

// TC-E4-3 (REQ-11): expiry blocks with approval_expired, edits the message with ⌛, merges nothing, and a press afterwards does nothing.
func TestP9E2E004_ExpiryBlocksWithoutMerge(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, ref := m.ask(t)
	expired, err := m.behaviors.ExpireDue(m.ctx, time.Now().Add(48*time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != rec.ID || expired[0].Kind != behavior.ApprovalPolicyGate {
		t.Fatalf("expired %+v", expired)
	}
	if sent, edits := m.msgr.counts(); sent != 1 || edits != 0 {
		t.Fatalf("the sweep touched the message: sent=%d edits=%d (the woken step edits it)", sent, edits)
	}
	// A press on the expired gate, even with a stale copy that still looks open, does nothing.
	stale := m.approvalOf(rec)
	if toast, code, _ := m.gates.HandlePress(m.ctx, m.update(), rec.MessageID, behavior.CallbackData{Kind: behavior.CallbackApprove, Nonce: rec.Nonce, Option: -1}, stale); code != "stale" || toast == "Đã duyệt." {
		t.Fatalf("press after expiry: %q %q", toast, code)
	}
	out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalExpired)
	if e := m.msgr.lastEdit(); !strings.HasPrefix(e.Text, "⌛ Hết hạn: merge PR acme/api#12") {
		t.Fatalf("edit %+v", e)
	}
	m.requireNoMerge(t)
	if got := m.reload(t, rec.ID); got.Decision != behavior.DecisionExpired || got.Consumed {
		t.Fatalf("gate %+v", got)
	}
	// After expiry the same merge needs a new approval.
	out, err = m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	if next, _ := m.waitingGate(t, out); next.ID == rec.ID || m.gateRows(t) != 2 {
		t.Fatalf("an expired gate was reused: %d", next.ID)
	}
}

// ---- E2E-009 ----------------------------------------------------------------

// TC-E9-1 (REQ-9, E2E-009): a deny rule blocks the task with policy_denied, creates no gate, calls no merge tool and records the decision.
func TestP9E2E009_DenyRuleBlocksWithNoSideEffect(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	m.denyRules(t)
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, tools.DenyPolicyDenied)
	if m.gateRows(t) != 0 {
		t.Fatal("a deny created a gate")
	}
	if sent, _ := m.msgr.counts(); sent != 0 {
		t.Fatal("a deny asked the owner")
	}
	if calls, _ := m.gh.state(); calls != 0 {
		t.Fatalf("the merge tool ran: %d GitHub calls", calls)
	}
	if got := m.toolRunStatuses(t); got != "denied:policy_denied" {
		t.Fatalf("tool runs %q", got)
	}
	var effect, reason, ruleKey, actor string
	if err := m.pool.QueryRow(m.ctx, `SELECT effect, reason, rule_key, actor_kind FROM policy_decisions WHERE action='github_pr_merge' AND execution_id=$1`, m.exec).
		Scan(&effect, &reason, &ruleKey, &actor); err != nil {
		t.Fatalf("the decision was not recorded: %v", err)
	}
	if effect != "deny" || reason != policy.ReasonActionRule || ruleKey != "actions.github_pr_merge" || actor != policy.ActorWorkflow {
		t.Fatalf("decision %s %s %s %s", effect, reason, ruleKey, actor)
	}
}

// TC-E9-2 (REQ-9): the owner can allow the merge by rule; then it merges without a gate (allow).
func TestP9E2E009_AllowRuleMergesWithoutGate(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rs := p9Parse(t, "schema: policy/v1\ndefaults:\n  read: allow\n  write: allow\n  dangerous: approval_required\nactions:\n  github_pr_merge: allow\napproval_ttl_hours: 24\n")
	if _, _, err := m.store.ApplyRuleSet(m.ctx, workspaceA, rs, policy.CreatedByCLI, 0); err != nil {
		t.Fatal(err)
	}
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil || !out.Succeeded {
		t.Fatalf("%+v %v", out, err)
	}
	if _, merges := m.gh.state(); merges != 1 || m.gateRows(t) != 0 {
		t.Fatalf("merges=%d gates=%d", merges, m.gateRows(t))
	}
	// The same task again replays.
	if again, err := m.actions.RunTask(m.ctx, m.task()); err != nil || !again.Succeeded {
		t.Fatalf("%+v %v", again, err)
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("merged %d times", merges)
	}
}

// TC-E9-3 (REQ-11, FR-130): the rule turns to deny after the approval: nothing merges; a head that moved or red checks stop the merge too.
func TestP9Merge_RevalidationAfterApproval(t *testing.T) {
	t.Run("rule changed to deny", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		_, ref := m.approved(t)
		m.denyRules(t)
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil {
			t.Fatal(err)
		}
		// The deny is found by the executor's decision before or at revalidation; both name a deny code.
		if out.Succeeded || !out.Blocked || (out.ReasonCode != tools.DenyPolicyDenied && out.ReasonCode != tools.DenyRevalidateDenied) {
			t.Fatalf("outcome %+v", out)
		}
		m.requireNoMerge(t)
		if calls, _ := m.gh.state(); calls != 0 {
			t.Fatalf("GitHub was called: %d", calls)
		}
		if n := m.count(t, `SELECT count(*) FROM policy_decisions WHERE action='github_pr_merge' AND effect='deny' AND execution_id=$1`, m.exec); n < 1 {
			t.Fatal("the deny was not recorded")
		}
	})
	t.Run("head moved", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		_, ref := m.approved(t)
		m.gh.mu.Lock()
		m.gh.head = "fedcba9876543210fedcba9876543210fedcba98"
		m.gh.mu.Unlock()
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil {
			t.Fatal(err)
		}
		requireBlocked(t, out, "head_sha_mismatch")
		m.requireNoMerge(t)
	})
	t.Run("checks failing", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		_, ref := m.approved(t)
		m.gh.mu.Lock()
		m.gh.runs = `{"check_runs":[{"name":"lint","status":"completed","conclusion":"failure"}]}`
		m.gh.mu.Unlock()
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil {
			t.Fatal(err)
		}
		requireBlocked(t, out, "checks_failing")
		m.requireNoMerge(t)
	})
	t.Run("approval bound to another input", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		rec, ref := m.approved(t)
		mustExec(t, m.ctx, m.pool, `UPDATE approvals SET input_sha256=repeat('a',64) WHERE id=$1`, rec.ID)
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil {
			t.Fatal(err)
		}
		requireBlocked(t, out, tools.DenyRevalidateDenied)
		m.requireNoMerge(t)
		if m.reload(t, rec.ID).Consumed {
			t.Fatal("a mismatched grant was consumed")
		}
	})
	t.Run("approval of another execution", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		rec, ref := m.approved(t)
		other := m.startTask(t)
		mustExec(t, m.ctx, m.pool, `UPDATE approvals SET execution_id=$2 WHERE id=$1`, rec.ID, other)
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil {
			t.Fatal(err)
		}
		requireBlocked(t, out, behavior.ReasonApprovalMissing)
		m.requireNoMerge(t)
	})
	t.Run("wait reference of nothing", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		for _, ref := range []string{"gate:999999", "gate:x", "gate:-3", "draft:5", "gate:"} {
			out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
			if err != nil {
				t.Fatal(err)
			}
			requireBlocked(t, out, behavior.ReasonApprovalMissing)
		}
		m.requireNoMerge(t)
	})
}

// TC-E9-4 (Review A): a dry run with a grant verifies it but never consumes it, so the owner's approval survives.
func TestP9Merge_DryRunDoesNotConsumeGrant(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	m.policies.set(func(c *tenant.AgentConfig) { c.Tools.DryRun = true })
	rec, ref := m.approved(t)
	out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.OutcomePolicyDryRun)
	m.requireNoMerge(t)
	if m.reload(t, rec.ID).Consumed {
		t.Fatal("a dry run consumed the approval")
	}
	// Dry run off again: the same approval still merges, once.
	m.policies.set(func(c *tenant.AgentConfig) { c.Tools.DryRun = false })
	m.build(mgOpts{})
	// The dry run recorded its own run under the same key; a different task key keeps the real merge separate.
	run := m.task()
	run.Task.Key = "land-again"
	out, err = m.actions.ResumeTask(m.ctx, run, ref)
	if err != nil || !out.Succeeded {
		t.Fatalf("%+v %v", out, err)
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("merges %d", merges)
	}
	if !m.reload(t, rec.ID).Consumed {
		t.Fatal("the real merge did not consume the approval")
	}
}

// ---- presses ----------------------------------------------------------------

// TC-PR-1 (REQ-10): a second press, a press in another chat, a press on an old message and a press for another workspace do nothing.
func TestP9Press_StaleCases(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, _ := m.ask(t)
	const stale = "Đã xử lý hoặc đã hết hạn."

	other := m.update()
	other.ChatID = -2002
	if toast, code, _ := m.pressWith(other, rec.MessageID, rec, behavior.CallbackApprove); toast != stale || code != "stale" {
		t.Fatalf("other chat: %q %q", toast, code)
	}
	if toast, code, _ := m.pressWith(m.update(), rec.MessageID+99, rec, behavior.CallbackApprove); toast != stale || code != "stale" {
		t.Fatalf("old message: %q %q", toast, code)
	}
	otherWS := m.update()
	otherWS.WorkspaceID = workspaceB
	if toast, code, _ := m.pressWith(otherWS, rec.MessageID, rec, behavior.CallbackApprove); toast != stale || code != "stale" {
		t.Fatalf("other workspace: %q %q", toast, code)
	}
	if toast, code, _ := m.pressWith(m.update(), rec.MessageID, rec, behavior.CallbackOption); code == "" || toast == "Đã duyệt." {
		t.Fatalf("an option press decided the gate: %q %q", toast, code)
	}
	if got := m.reload(t, rec.ID); got.Decision != "" {
		t.Fatalf("a stale press decided the gate: %+v", got)
	}
	if sent, edits := m.msgr.counts(); sent != 1 || edits != 0 {
		t.Fatalf("stale presses touched the message: %d %d", sent, edits)
	}
	// An unknown nonce for a known approval id decides nothing.
	wrong := m.approvalOf(rec)
	wrong.Nonce = "nonce-does-not-exist-0000"
	if toast, code, _ := m.gates.HandlePress(m.ctx, m.update(), rec.MessageID, behavior.CallbackData{Kind: behavior.CallbackApprove, Nonce: wrong.Nonce, Option: -1}, wrong); toast != stale || code != "stale" {
		t.Fatalf("wrong nonce: %q %q", toast, code)
	}
	// The first real press decides; the second, even from a stale copy, does nothing and does not edit again.
	if toast, _, _ := m.press(rec, behavior.CallbackApprove); toast != "Đã duyệt." {
		t.Fatalf("first press %q", toast)
	}
	_, edits := m.msgr.counts()
	if toast, code, _ := m.press(rec, behavior.CallbackReject); toast != stale || code != "stale" {
		t.Fatalf("second press: %q %q", toast, code)
	}
	if _, after := m.msgr.counts(); after != edits {
		t.Fatal("a second press edited the message again")
	}
	if m.reload(t, rec.ID).Decision != behavior.DecisionApproved {
		t.Fatal("the second press changed the decision")
	}
	// Double tap racing: exactly one wins.
	m2 := newMGEnv(t, mgOpts{})
	rec2, _ := m2.ask(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if toast, _, _ := m2.press(rec2, behavior.CallbackApprove); toast == "Đã duyệt." {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d presses won", won)
	}
}

// TC-PR-2 (REQ-10): the press needs the workspace's owner member; without one the gate stays open and the toast says why.
func TestP9Press_NoOwnerMember(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, _ := m.ask(t)
	mustExec(t, m.ctx, m.pool, `UPDATE approvals SET requested_user_id=NULL WHERE id=$1`, rec.ID)
	mustExec(t, m.ctx, m.pool, `DELETE FROM workspace_members WHERE workspace_id=$1`, workspaceA)
	toast, code, keep := m.press(rec, behavior.CallbackApprove)
	if code != "no_owner_member" || toast == "" || keep {
		t.Fatalf("%q %q %v", toast, code, keep)
	}
	if m.reload(t, rec.ID).Decision != "" {
		t.Fatal("the gate was decided without an owner member")
	}
}

// TC-PR-3 (REQ-9): no owner member when the merge asks: blocked with no_owner_member, no gate, no message.
func TestP9Merge_NoOwnerMemberBlocksTask(t *testing.T) {
	m := newMGEnv(t, mgOpts{noOwner: true})
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalNoApprover)
	if sent, _ := m.msgr.counts(); sent != 0 || m.gateRows(t) != 0 {
		t.Fatalf("sent=%d gates=%d", sent, m.gateRows(t))
	}
	m.requireNoMerge(t)
}

// ---- dedupe and send failure ----------------------------------------------------

// TC-DD-1 (REQ-9): the same merge asked by another execution is approval_pending_elsewhere: no second gate, no second message.
func TestP9Merge_SameInputInAnotherExecution(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, _ := m.ask(t)
	other := m.startTask(t)
	out, err := m.actions.RunTask(m.ctx, m.run(other, "acme/api", 12, mgHead))
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalElsewhere)
	if m.gateRows(t) != 1 {
		t.Fatalf("%d gates", m.gateRows(t))
	}
	if sent, _ := m.msgr.counts(); sent != 1 {
		t.Fatalf("%d messages", sent)
	}
	// The other execution cannot use the first one's approval.
	if toast, _, _ := m.press(rec, behavior.CallbackApprove); toast != "Đã duyệt." {
		t.Fatal(toast)
	}
	out, err = m.actions.ResumeTask(m.ctx, m.run(other, "acme/api", 12, mgHead), fmt.Sprintf("gate:%d", rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalMissing)
	m.requireNoMerge(t)
	// A different head or PR is a different gate and gets its own question.
	if o, err := m.actions.RunTask(m.ctx, m.run(other, "acme/api", 13, mgHead)); err != nil || o.Blocked || !strings.HasPrefix(o.Wait, "gate:") || m.gateRows(t) != 2 {
		t.Fatalf("another PR must get its own gate: %+v %v (%d gates)", o, err, m.gateRows(t))
	}
}

// TC-DD-2 (REQ-9): a failed send blocks with approval_not_sent, leaves one open gate without a message, and an identical later merge resends it.
func TestP9Merge_SendFailureThenResend(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	m.msgr.failSend = true
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	requireBlocked(t, out, behavior.ReasonApprovalNotSent)
	if m.gateRows(t) != 1 {
		t.Fatalf("%d gates", m.gateRows(t))
	}
	var firstID int64
	var msgID *int64
	if err := m.pool.QueryRow(m.ctx, `SELECT id, telegram_message_id FROM approvals WHERE kind='policy_gate'`).Scan(&firstID, &msgID); err != nil || (msgID != nil && *msgID != 0) {
		t.Fatalf("%d %v %v", firstID, msgID, err)
	}
	m.requireNoMerge(t)
	// Telegram is back: the identical merge finds the open gate and sends the message.
	m.msgr.failSend = false
	out, err = m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := m.waitingGate(t, out)
	if rec.ID != firstID || m.gateRows(t) != 1 || rec.MessageID == 0 {
		t.Fatalf("gate %d (first %d), %d rows, message %d", rec.ID, firstID, m.gateRows(t), rec.MessageID)
	}
	if sent, _ := m.msgr.counts(); sent != 1 {
		t.Fatalf("%d messages", sent)
	}
	// And once more: the message is not sent a third time.
	if _, err := m.actions.RunTask(m.ctx, m.task()); err != nil {
		t.Fatal(err)
	}
	if sent, _ := m.msgr.counts(); sent != 1 {
		t.Fatalf("%d messages after a third ask", sent)
	}
}

// ---- configuration ----------------------------------------------------------------

// TC-CF-1 (REQ-9): without a repo grant, a merge method or the enabled tool the task is blocked with merge_not_configured and nothing runs.
func TestP9Merge_NotConfigured(t *testing.T) {
	cases := map[string]func(*tenant.AgentConfig){
		"no repo grant": func(c *tenant.AgentConfig) { c.Tools.Repos = nil },
		"other repo only": func(c *tenant.AgentConfig) {
			c.Tools.Repos = map[string]tenant.RepoGrant{"acme/web": c.Tools.Repos["acme/api"]}
		},
		"no merge methods": func(c *tenant.AgentConfig) {
			g := c.Tools.Repos["acme/api"]
			g.MergeMethods = nil
			c.Tools.Repos["acme/api"] = g
		},
		"merge tool disabled": func(c *tenant.AgentConfig) { c.Tools.Enabled = []string{"github_pr_comment"} },
		"nothing enabled":     func(c *tenant.AgentConfig) { c.Tools.Enabled = nil },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMGEnv(t, mgOpts{})
			m.policies.set(mod)
			out, err := m.actions.RunTask(m.ctx, m.task())
			if err != nil {
				t.Fatal(err)
			}
			requireBlocked(t, out, behavior.ReasonMergeNotConfigured)
			if m.gateRows(t) != 0 {
				t.Fatal("a gate was made for an unconfigured merge")
			}
			if sent, _ := m.msgr.counts(); sent != 0 {
				t.Fatal("the owner was asked for an unconfigured merge")
			}
			m.requireNoMerge(t)
			if calls, _ := m.gh.state(); calls != 0 {
				t.Fatalf("GitHub was called %d times", calls)
			}
		})
	}
}

// TC-CF-2 (REQ-9): a merge task for a repo the grant allows but the tool does not allow is refused by the executor (tool_not_allowed), never asked.
func TestP9Merge_RepoGrantWithoutAllowIsRefused(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	m.policies.set(func(c *tenant.AgentConfig) {
		g := c.Tools.Repos["acme/api"]
		g.Allow = nil
		c.Tools.Repos["acme/api"] = g
	})
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	if out.Succeeded || !out.Blocked || out.ReasonCode != "tool_not_allowed" || m.gateRows(t) != 0 {
		t.Fatalf("%+v gates=%d", out, m.gateRows(t))
	}
	m.requireNoMerge(t)
}

// TC-CF-3 (REQ-9): github.merge is offered only when the merge path is complete; an invalid task never reaches GitHub.
func TestP9Merge_OffersAndInvalidTasks(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	if !m.actions.Offers(orchestrator.CapGitHubMerge) || !m.actions.Offers(orchestrator.CapTelegramNotify) {
		t.Fatal("a complete executor must offer github.merge")
	}
	for name, mg := range map[string]*behavior.OrchestratorMerge{
		"nil":          nil,
		"no gates":     {Registry: m.reg, Audit: m.runs, Sanitizer: p9San{}, Policies: m.policies, Engine: m.engine},
		"no engine":    {Registry: m.reg, Audit: m.runs, Sanitizer: p9San{}, Policies: m.policies, Gates: m.gates},
		"no audit":     {Registry: m.reg, Sanitizer: p9San{}, Policies: m.policies, Engine: m.engine, Gates: m.gates},
		"no policies":  {Registry: m.reg, Audit: m.runs, Sanitizer: p9San{}, Engine: m.engine, Gates: m.gates},
		"no sanitizer": {Registry: m.reg, Audit: m.runs, Policies: m.policies, Engine: m.engine, Gates: m.gates},
		"no registry":  {Audit: m.runs, Sanitizer: p9San{}, Policies: m.policies, Engine: m.engine, Gates: m.gates},
		"no merge tool": func() *behavior.OrchestratorMerge {
			empty, _ := tools.NewRegistry()
			return &behavior.OrchestratorMerge{Registry: empty, Audit: m.runs, Sanitizer: p9San{}, Policies: m.policies, Engine: m.engine, Gates: m.gates}
		}(),
	} {
		a := &behavior.OrchestratorActions{Merge: mg}
		if a.Offers(orchestrator.CapGitHubMerge) {
			t.Errorf("%s: github.merge offered", name)
		}
		out, err := a.RunTask(m.ctx, m.task())
		if err != nil || out.Succeeded || out.Wait != "" {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	m.requireNoMerge(t)
	// Invalid targets and capability mixes fail before any call.
	bad := []orchestrator.TaskRun{
		m.run(m.exec, "acme/api", 0, mgHead),
		m.run(m.exec, "acme/api", 12, "abc1234"),
		m.run(m.exec, "acme/../x", 12, mgHead),
		m.run(m.exec, "", 12, mgHead),
	}
	mixed := m.task()
	mixed.Task.Capabilities = []string{orchestrator.CapGitHubMerge, orchestrator.CapClickUpRead}
	noTarget := m.task()
	noTarget.Task.Merge = nil
	bad = append(bad, mixed, noTarget)
	for i, run := range bad {
		out, err := m.actions.RunTask(m.ctx, run)
		if err != nil || out.Succeeded || out.Wait != "" || m.gateRows(t) != 0 {
			t.Errorf("bad task %d: %+v %v gates=%d", i, out, err, m.gateRows(t))
		}
	}
	if calls, _ := m.gh.state(); calls != 0 || m.gateRows(t) != 0 {
		t.Fatalf("invalid tasks reached GitHub (%d) or the owner (%d gates)", calls, m.gateRows(t))
	}
}

// ---- storage: 00037 and merge_target -------------------------------------------------

// TC-M37-1 (REQ-9): merge_target is stored with the plan task, round-trips and only holds a valid shape.
func TestP9OrchestratorStore_MergeTargetRoundTrip(t *testing.T) {
	e := newB2Env(t)
	ex := e.startTask(t)
	merge := orchTask("land")
	merge.Capabilities = []string{orchestrator.CapGitHubMerge}
	merge.Executor = orchestrator.ExecutorBehaviorActions
	merge.Merge = &orchestrator.MergeTarget{Repo: "acme/api", Number: 12, HeadSHA: mgHead}
	if _, err := e.plans.InsertPlan(e.ctx, workspaceA, orchPlan(ex, 1), []orchestrator.PlanTaskRecord{orchTask("fix"), merge}); err != nil {
		t.Fatal(err)
	}
	tasks, err := e.plans.ListTasks(e.ctx, workspaceA, ex)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("%v %v", tasks, err)
	}
	for _, tk := range tasks {
		switch tk.Key {
		case "land":
			if tk.Merge == nil || *tk.Merge != *merge.Merge {
				t.Fatalf("merge target lost: %+v", tk.Merge)
			}
		default:
			if tk.Merge != nil {
				t.Fatalf("task %s got a target %+v", tk.Key, tk.Merge)
			}
		}
	}
	// The store refuses an invalid target; the database refuses a malformed value.
	bad := orchTask("bad")
	bad.Merge = &orchestrator.MergeTarget{Repo: "acme/api", Number: 0, HeadSHA: mgHead}
	if _, err := e.plans.InsertPlan(e.ctx, workspaceA, orchPlan(ex, 2), []orchestrator.PlanTaskRecord{bad}); err == nil {
		t.Fatal("an invalid merge target was stored")
	}
	for name, v := range map[string]string{
		"short sha":   `{"repo":"a/b","number":1,"head_sha":"abc"}`,
		"no repo":     `{"number":1,"head_sha":"` + mgHead + `"}`,
		"not object":  `[]`,
		"upper sha":   `{"repo":"a/b","number":1,"head_sha":"` + strings.ToUpper(mgHead) + `"}`,
		"too large":   `{"repo":"a/b","number":1,"head_sha":"` + mgHead + `","pad":"` + strings.Repeat("x", 600) + `"}`,
		"missing num": `{"repo":"a/b","head_sha":"` + mgHead + `"}`,
	} {
		_, err := e.pool.Exec(e.ctx, `UPDATE orchestrator_plan_tasks SET merge_target=$1::jsonb WHERE key='land'`, v)
		if err == nil {
			t.Errorf("%s: the database accepted %s", name, v)
		}
	}
}

// TC-M37-2 (REQ-9, D-A-1): 00037 Down removes merge_target and restores the old consumed check; Up applies the fix again.
func TestP9Migration37_DownUp(t *testing.T) {
	e := newMGEnv(t, mgOpts{})
	rec, _ := e.ask(t)
	// Up (current): consumed_at on an open gate is refused.
	_, err := e.pool.Exec(e.ctx, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, rec.ID)
	requirePGCode(t, err, "23514")
	if e.count(t, `SELECT count(*) FROM information_schema.columns WHERE table_name='orchestrator_plan_tasks' AND column_name='merge_target'`) != 1 {
		t.Fatal("merge_target is missing")
	}
	mustExec(t, e.ctx, e.pool, downSection(t, "00037_orchestrator_merge_target.sql"))
	if e.count(t, `SELECT count(*) FROM information_schema.columns WHERE table_name='orchestrator_plan_tasks' AND column_name='merge_target'`) != 0 {
		t.Fatal("Down left merge_target")
	}
	// Down restores the old behaviour (the defect D-A-1 was in it): an open gate passes the old CHECK.
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, rec.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET consumed_at=NULL WHERE id=$1`, rec.ID)
	mustExec(t, e.ctx, e.pool, upSection(t, "00037_orchestrator_merge_target.sql"))
	if e.count(t, `SELECT count(*) FROM information_schema.columns WHERE table_name='orchestrator_plan_tasks' AND column_name='merge_target'`) != 1 {
		t.Fatal("Up did not restore merge_target")
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, rec.ID)
	requirePGCode(t, err, "23514")
	// Approved gates may still be consumed; rejected ones may not.
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET decision='approved', decided_at=now() WHERE id=$1`, rec.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, rec.ID)
}
