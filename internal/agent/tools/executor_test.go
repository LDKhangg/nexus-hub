package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type fakeTool struct {
	spec    ToolSpec
	repo    bool
	network bool
	escal   func(json.RawMessage, *tenant.RepoGrant) tenant.Tier
	run     func(ctx context.Context, c Call) (ToolOutput, error)
	mu      sync.Mutex
	calls   []Call
}

func (f *fakeTool) Spec() ToolSpec { return f.spec }
func (f *fakeTool) Run(ctx context.Context, c Call) (ToolOutput, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, c)
	}
	return ToolOutput{Text: "ran " + f.spec.Name, Summary: "ran"}, nil
}
func (f *fakeTool) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type repoTool struct{ *fakeTool }

func (r repoTool) Repo(in json.RawMessage) (string, error) {
	var v struct{ Repo string }
	if err := json.Unmarshal(in, &v); err != nil || v.Repo == "" {
		return "", errors.New("bad")
	}
	return v.Repo, nil
}

type netTool struct{ repoTool }

func (n netTool) UsesNetwork() bool { return true }

type escTool struct{ repoTool }

func (e escTool) EffectiveTier(in json.RawMessage, g *tenant.RepoGrant) tenant.Tier {
	return e.escal(in, g)
}

type fakeStore struct {
	mu     sync.Mutex
	rows   []*fakeRow
	nextID int64
}

type fakeRow struct {
	ID      int64
	Start   RunStart
	Status  string
	Code    string
	Summary string
}

func (s *fakeStore) Begin(_ context.Context, r RunStart) (int64, *PriorRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Tier != tenant.TierRead {
		for _, row := range s.rows {
			if row.Start.Key == r.Key && row.Start.Tier != tenant.TierRead && row.Status != "denied" && row.Status != "error" && row.Status != "dry_run" {
				st := row.Status
				if st != "ok" {
					st = "unknown_outcome"
				}
				return 0, &PriorRun{Status: st, Summary: row.Summary}, nil
			}
		}
	}
	s.nextID++
	s.rows = append(s.rows, &fakeRow{ID: s.nextID, Start: r, Status: "started"})
	return s.nextID, nil, nil
}
func (s *fakeStore) Finish(_ context.Context, f RunFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.ID == f.RunID {
			row.Status, row.Code, row.Summary = f.Status, f.ErrorCode, f.Summary
		}
	}
	return nil
}
func (s *fakeStore) Deny(_ context.Context, r RunStart, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.rows = append(s.rows, &fakeRow{ID: s.nextID, Start: r, Status: "denied", Code: code})
	return nil
}
func (s *fakeStore) last() *fakeRow { s.mu.Lock(); defer s.mu.Unlock(); return s.rows[len(s.rows)-1] }

type upperSan struct{}

func (upperSan) CleanOutbound(s string, _ bool) string {
	return strings.ReplaceAll(s, "SECRET", "[REDACTED]")
}

const repoName = "o/r"

func policy() tenant.ToolPolicyConfig {
	return tenant.ToolPolicyConfig{
		Enabled:         []string{"read_a", "read_repo", "net_read"},
		MaxWritesPerJob: 2,
		Accounts:        map[string]tenant.GitHubAccount{"acc": {Name: "acc", Type: "pat", Login: "me"}},
		Repos: map[string]tenant.RepoGrant{repoName: {
			Repo: repoName, Account: "acc", Managed: true,
			Allow: []string{"write_a", "write_b", "write_c", "danger_a", "danger_b", "push"},
		}},
	}
}

func readTool(name string) *fakeTool {
	return &fakeTool{spec: ToolSpec{Name: name, Tier: tenant.TierRead, InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func tierTool(name string, tier tenant.Tier) repoTool {
	return repoTool{&fakeTool{spec: ToolSpec{Name: name, Tier: tier, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
}

type env struct {
	exec  *Executor
	store *fakeStore
	tools map[string]*fakeTool
}

func newEnv(t *testing.T, p tenant.ToolPolicyConfig, allowDangerous bool, limits Limits) env {
	t.Helper()
	ra := readTool("read_a")
	rr := tierTool("read_repo", tenant.TierRead)
	nr := netTool{tierTool("net_read", tenant.TierRead)}
	wa, wb, wc := tierTool("write_a", tenant.TierWrite), tierTool("write_b", tenant.TierWrite), tierTool("write_c", tenant.TierWrite)
	da, db := tierTool("danger_a", tenant.TierDangerous), tierTool("danger_b", tenant.TierDangerous)
	push := escTool{tierTool("push", tenant.TierWrite)}
	push.escal = func(in json.RawMessage, g *tenant.RepoGrant) tenant.Tier {
		if strings.Contains(string(in), `"main"`) {
			return tenant.TierDangerous
		}
		return tenant.TierWrite
	}
	reg, err := NewRegistry(ra, rr, nr, wa, wb, wc, da, db, push)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	e := NewExecutor(reg, p, limits, allowDangerous, store, upperSan{}, 42, "ws-1")
	return env{exec: e, store: store, tools: map[string]*fakeTool{
		"read_a": ra, "read_repo": rr.fakeTool, "net_read": nr.fakeTool, "write_a": wa.fakeTool, "write_b": wb.fakeTool,
		"write_c": wc.fakeTool, "danger_a": da.fakeTool, "danger_b": db.fakeTool, "push": push.fakeTool,
	}}
}

const repoIn = `{"repo":"o/r"}`

func expectDenied(t *testing.T, e env, r CallResult, code string) {
	t.Helper()
	if !r.IsError || r.Status != "denied" || !strings.Contains(r.Content, code) {
		t.Fatalf("want denied %s, got %+v", code, r)
	}
	if row := e.store.last(); row.Status != "denied" || row.Code != code {
		t.Fatalf("audit row %+v", row)
	}
}

func TestExecutor_UnknownTool(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "rm_rf", json.RawMessage(`{}`)), "unknown_tool")
}

func TestExecutor_NotEnabled(t *testing.T) {
	p := policy()
	p.Enabled = []string{"read_repo"}
	e := newEnv(t, p, false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`)), "tool_disabled")
	if e.tools["read_a"].count() != 0 {
		t.Fatal("ran")
	}
}

func TestExecutor_InputTooLarge(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	big := `{"x":"` + strings.Repeat("a", 17*1024) + `"}`
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(big)), "input_too_large")
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`not json`)), "invalid_input")
}

func TestExecutor_RepoNotConfigured(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_repo", json.RawMessage(`{"repo":"x/y"}`)), "repo_not_configured")
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_repo", json.RawMessage(`{}`)), "invalid_input")
}

func TestExecutor_WriteNotAllowedForRepo(t *testing.T) {
	p := policy()
	g := p.Repos[repoName]
	g.Allow = []string{"write_b"}
	p.Repos[repoName] = g
	p.Enabled = append(p.Enabled, "write_a") // enabled globally, but not allowed for this repo
	e := newEnv(t, p, false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)), "tool_not_allowed")
	if r := e.exec.Call(context.Background(), 1, "write_b", json.RawMessage(repoIn)); r.IsError || r.Status != "ok" || r.Tier != tenant.TierWrite {
		t.Fatalf("allowed write failed %+v", r)
	}
}

func TestExecutor_DangerousNeedsSwitch(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn)), "dangerous_disabled")
	on := newEnv(t, policy(), true, Limits{})
	if r := on.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn)); r.IsError {
		t.Fatalf("%+v", r)
	}
}

func TestExecutor_EscalatedTierGated(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	if r := e.exec.Call(context.Background(), 1, "push", json.RawMessage(`{"repo":"o/r","branch":"nexus/x"}`)); r.IsError || r.Tier != tenant.TierWrite {
		t.Fatalf("%+v", r)
	}
	r := e.exec.Call(context.Background(), 1, "push", json.RawMessage(`{"repo":"o/r","branch":"main"}`))
	expectDenied(t, e, r, "dangerous_disabled")
	if r.Tier != tenant.TierDangerous || e.store.last().Start.Tier != tenant.TierDangerous {
		t.Fatalf("tier not escalated %+v", r)
	}
	if e.tools["push"].count() != 1 {
		t.Fatal("escalated push ran")
	}
}

func TestExecutor_DangerousOnePerJob(t *testing.T) {
	e := newEnv(t, policy(), true, Limits{})
	if r := e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn)); r.IsError {
		t.Fatalf("%+v", r)
	}
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "danger_b", json.RawMessage(repoIn)), "dangerous_budget")
}

func TestExecutor_CallBudget(t *testing.T) {
	l := DefaultLimits()
	l.MaxCalls = 3
	e := newEnv(t, policy(), false, l)
	for i := 0; i < 3; i++ {
		if r := e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{"i":`+string(rune('0'+i))+`}`)); r.IsError {
			t.Fatalf("%+v", r)
		}
	}
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`)), "call_budget")
}

func TestExecutor_WriteBudget(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	for _, n := range []string{"write_a", "write_b"} {
		if r := e.exec.Call(context.Background(), 1, n, json.RawMessage(repoIn)); r.IsError {
			t.Fatalf("%+v", r)
		}
	}
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_c", json.RawMessage(repoIn)), "write_budget")
}

func TestExecutor_IdempotentReplayOK(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	first := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	// A retry builds a new executor for the same job with the same store.
	again := NewExecutor(e.exec.reg, policy(), DefaultLimits(), false, e.store, upperSan{}, 42, "ws-1")
	second := again.Call(context.Background(), 2, "write_a", json.RawMessage(`{ "repo" : "o/r" }`))
	if first.IsError || second.IsError || second.Status != "ok" || !strings.Contains(second.Content, "ran") {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if e.tools["write_a"].count() != 1 {
		t.Fatalf("write re-executed %d", e.tools["write_a"].count())
	}
}

func TestExecutor_IdempotentReplayStarted(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	_, _, _ = e.store.Begin(context.Background(), RunStart{Tier: tenant.TierWrite, Key: CanonicalKey(42, "write_a", json.RawMessage(repoIn))})
	r := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	if !r.IsError || r.Status != "unknown_outcome" || e.tools["write_a"].count() != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestExecutor_DryRun(t *testing.T) {
	p := policy()
	p.DryRun = true
	e := newEnv(t, p, false, Limits{})
	r := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	if r.IsError || r.Status != "dry_run" || !strings.HasPrefix(r.Content, "DRY RUN: would") || e.tools["write_a"].count() != 0 {
		t.Fatalf("%+v", r)
	}
	if e.store.last().Status != "dry_run" {
		t.Fatalf("%+v", e.store.last())
	}
	if r := e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`)); r.Status != "ok" || e.tools["read_a"].count() != 1 {
		t.Fatalf("read under dry run %+v", r)
	}
}

func TestExecutor_Timeouts(t *testing.T) {
	l := DefaultLimits()
	l.ReadTimeout = 20 * time.Millisecond
	l.NetworkTimeout = 300 * time.Millisecond
	e := newEnv(t, policy(), false, l)
	wait := func(ctx context.Context, _ Call) (ToolOutput, error) {
		dl, _ := ctx.Deadline()
		select {
		case <-ctx.Done():
			return ToolOutput{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return ToolOutput{Text: "slow ok " + time.Until(dl).String()}, nil
		}
	}
	e.tools["read_repo"].run = wait
	e.tools["net_read"].run = wait
	if r := e.exec.Call(context.Background(), 1, "read_repo", json.RawMessage(repoIn)); !r.IsError || r.Status != "timeout" {
		t.Fatalf("read %+v", r)
	}
	if r := e.exec.Call(context.Background(), 1, "net_read", json.RawMessage(repoIn)); r.IsError {
		t.Fatalf("network %+v", r)
	}
}

func TestExecutor_OutputSanitizedAndCapped(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	e.tools["read_a"].run = func(context.Context, Call) (ToolOutput, error) {
		return ToolOutput{Text: "SECRET " + strings.Repeat("ữ", 5000)}, nil
	}
	r := e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`))
	if strings.Contains(r.Content, "SECRET") || len(r.Content) > 6144 || !strings.HasSuffix(r.Content, "[truncated]") {
		t.Fatalf("len %d %q", len(r.Content), r.Content[:40])
	}
	e.tools["read_a"].run = func(context.Context, Call) (ToolOutput, error) {
		return ToolOutput{}, &ToolError{Code: "not_found"}
	}
	r = e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{"x":1}`))
	if !r.IsError || r.Status != "error" || !strings.Contains(r.Content, "not_found") {
		t.Fatalf("%+v", r)
	}
	e.tools["read_a"].run = func(context.Context, Call) (ToolOutput, error) {
		return ToolOutput{}, errors.New("raw failure with SECRET token")
	}
	r = e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{"x":2}`))
	if !r.IsError || strings.Contains(r.Content, "SECRET") || strings.Contains(r.Content, "raw failure") {
		t.Fatalf("raw error leaked %+v", r)
	}
}

func TestExecutor_GrantAndAccountPassed(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	e.exec.ClickUpTokenRef = "clickup"
	e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	c := e.tools["write_a"].calls[0]
	if c.Repo == nil || c.Repo.Repo != repoName || c.Account == nil || c.Account.Login != "me" || c.WorkspaceID != "ws-1" || c.ClickUpTokenRef != "clickup" {
		t.Fatalf("call %+v", c)
	}
	row := e.store.last()
	if row.Start.Repo != repoName || row.Start.Account != "acc" || row.Start.JobID != 42 || row.Start.WorkspaceID != "ws-1" {
		t.Fatalf("row %+v", row.Start)
	}
}

func TestExecutor_PanicIsError(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	e.tools["read_a"].run = func(context.Context, Call) (ToolOutput, error) { panic("boom") }
	if r := e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`)); !r.IsError || !strings.Contains(r.Content, "tool_panic") {
		t.Fatalf("%+v", r)
	}
}

func TestCanonicalKey_OrderIndependent(t *testing.T) {
	a := CanonicalKey(1, "t", json.RawMessage(`{"a":1,"b":{"c":2,"d":3}}`))
	b := CanonicalKey(1, "t", json.RawMessage(`{ "b":{"d":3,"c":2}, "a":1 }`))
	if a != b {
		t.Fatal("order dependent")
	}
	if a == CanonicalKey(2, "t", json.RawMessage(`{"a":1,"b":{"c":2,"d":3}}`)) || a == CanonicalKey(1, "u", json.RawMessage(`{"a":1,"b":{"c":2,"d":3}}`)) {
		t.Fatal("key ignores job or tool")
	}
}

func TestRegistry_DuplicateName(t *testing.T) {
	if _, err := NewRegistry(readTool("x"), readTool("x")); err == nil {
		t.Fatal("duplicate accepted")
	}
	reg, err := NewRegistry(readTool("a"), tierTool("w", tenant.TierWrite))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Tiers()["w"] != tenant.TierWrite || len(reg.Specs([]string{"a"})) != 1 || reg.Get("zz") != nil {
		t.Fatal("registry accessors")
	}
}

func TestExecutor_WriteTimeoutNotRepeated(t *testing.T) {
	l := DefaultLimits()
	l.ReadTimeout = 20 * time.Millisecond
	e := newEnv(t, policy(), false, l)
	e.tools["write_a"].run = func(ctx context.Context, _ Call) (ToolOutput, error) {
		<-ctx.Done()
		return ToolOutput{}, ctx.Err()
	}
	r := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	if !r.IsError || r.Status != "unknown_outcome" || e.store.last().Status != "unknown_outcome" {
		t.Fatalf("%+v", r)
	}
	again := NewExecutor(e.exec.reg, policy(), l, false, e.store, upperSan{}, 42, "ws-1")
	if r := again.Call(context.Background(), 2, "write_a", json.RawMessage(repoIn)); r.Status != "unknown_outcome" || e.tools["write_a"].count() != 1 {
		t.Fatalf("retry re-executed: %+v count %d", r, e.tools["write_a"].count())
	}
}
