package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 9 Batch A against PostgreSQL: migrations 00035/00036, the rule set
// store, the engine's audit rows, policy_gate approvals and the grant check
// in the tool run transaction (REQ-1..REQ-3, REQ-6, REQ-7).

var p9Known = policy.NewKnownActions("code_search", "github_pr_comment", "github_pr_merge", "git_push")

func p9Sha(c string) string { return strings.Repeat(c, 64) }

func p9Parse(t *testing.T, src string) policy.RuleSet {
	t.Helper()
	rs, err := policy.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func p9RuleSet(t *testing.T, writeEffect string, ttlHours int) policy.RuleSet {
	t.Helper()
	return p9Parse(t, "schema: policy/v1\ndefaults:\n  read: allow\n  write: "+writeEffect+"\n  dangerous: approval_required\nactions:\n  github_pr_merge: approval_required\napproval_ttl_hours: "+strconv.Itoa(ttlHours)+"\n")
}

func p9TwoWorkspaces(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'B')`, workspaceB)
	return ctx, pool
}

func p9Count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TC-M1 (REQ-1): migration 00035 seeds v1 active for each existing workspace; the hash is the engine's canonical hash.
func TestP9Migration35_SeedsV1ForEveryWorkspace(t *testing.T) {
	ctx, pool := p9TwoWorkspaces(t)
	mustExec(t, ctx, pool, downSection(t, "00036_policy_gate_approvals.sql"))
	mustExec(t, ctx, pool, downSection(t, "00035_policy_rule_sets.sql"))
	if n := p9Count(t, ctx, pool, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('policy_rule_sets','policy_decisions')`); n != 0 {
		t.Fatalf("Down left %d tables", n)
	}
	if n := p9Count(t, ctx, pool, `SELECT count(*) FROM pg_constraint WHERE conname='tool_runs_workspace_id_id_key'`); n != 0 {
		t.Fatal("Down left the tool_runs composite key")
	}
	mustExec(t, ctx, pool, upSection(t, "00035_policy_rule_sets.sql"))
	mustExec(t, ctx, pool, upSection(t, "00036_policy_gate_approvals.sql"))

	_, canonicalSum, _ := policy.Canonical(policy.DefaultV1())
	for _, ws := range []string{workspaceA, workspaceB} {
		var version int32
		var schema, sha, by string
		var active bool
		var body []byte
		var activated *time.Time
		if err := pool.QueryRow(ctx, `SELECT version,schema,body,body_sha256,created_by,is_active,activated_at FROM policy_rule_sets WHERE workspace_id=$1`, ws).
			Scan(&version, &schema, &body, &sha, &by, &active, &activated); err != nil {
			t.Fatalf("workspace %s has no seeded rule set: %v", ws, err)
		}
		if version != 1 || schema != policy.SchemaV1 || by != "migration" || !active || activated == nil {
			t.Fatalf("%d %s %s %v %v", version, schema, by, active, activated)
		}
		if sha != canonicalSum || sha != policy.SHA256Hex([]byte(policy.DefaultV1JSON)) {
			t.Fatalf("seeded hash %s != Canonical(DefaultV1()) %s", sha, canonicalSum)
		}
		stored := policy.StoredRuleSet{Schema: schema, Body: body, BodySHA256: sha}
		if _, err := policy.LoadStored(stored, policy.NewKnownActions("github_pr_merge")); err != nil {
			t.Fatalf("the seeded body does not load: %v", err)
		}
	}
	if n := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_rule_sets`); n != 2 {
		t.Fatalf("%d rows for 2 workspaces", n)
	}
	// A second active row for the same workspace is refused; a second version, inactive, is fine.
	_, err := pool.Exec(ctx, `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by,is_active,activated_at) VALUES($1,2,'policy/v1','{}',$2,'cli',true,now())`, workspaceA, p9Sha("a"))
	requirePGCode(t, err, "23505")
	mustExec(t, ctx, pool, `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,2,'policy/v1','{}',$2,'cli')`, workspaceA, p9Sha("a"))
	// Constraint shapes.
	for name, sql := range map[string]string{
		"duplicate version": `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,1,'policy/v1','{}','` + p9Sha("b") + `','cli')`,
		"bad schema":        `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,3,'policy/v2','{}','` + p9Sha("b") + `','cli')`,
		"bad hash":          `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,3,'policy/v1','{}','zz','cli')`,
		"bad created_by":    `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,3,'policy/v1','{}','` + p9Sha("b") + `','root')`,
		"body not object":   `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,3,'policy/v1','[]','` + p9Sha("b") + `','cli')`,
		"active w/o time":   `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by,is_active) VALUES($1,3,'policy/v1','{}','` + p9Sha("b") + `','cli',true)`,
	} {
		if _, err := pool.Exec(ctx, sql, workspaceB); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Down is clean once more.
	mustExec(t, ctx, pool, downSection(t, "00036_policy_gate_approvals.sql"))
	mustExec(t, ctx, pool, downSection(t, "00035_policy_rule_sets.sql"))
	mustExec(t, ctx, pool, upSection(t, "00035_policy_rule_sets.sql"))
	mustExec(t, ctx, pool, upSection(t, "00036_policy_gate_approvals.sql"))
}

// TC-M2 (REQ-1): apply, same-hash apply, rollback, history, workspace checks.
func TestP9PolicyStore_ApplyRollbackHistory(t *testing.T) {
	ctx, pool := p9TwoWorkspaces(t)
	s, err := NewPolicyStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPolicyStore(nil); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, ok, err := s.ActiveRuleSet(ctx, workspaceA); err != nil || ok {
		t.Fatalf("a new workspace has no rule set yet: %v %v", ok, err)
	}
	v1 := policy.DefaultV1()
	got, changed, err := s.ApplyRuleSet(ctx, workspaceA, v1, policy.CreatedByInit, 0)
	if err != nil || !changed || got.Version != 1 || !got.Active || got.CreatedBy != "init" {
		t.Fatalf("%+v %v %v", got, changed, err)
	}
	same, changed, err := s.ApplyRuleSet(ctx, workspaceA, v1, policy.CreatedByCLI, 0)
	if err != nil || changed || same.Version != 1 {
		t.Fatalf("same hash must change nothing: %+v %v %v", same, changed, err)
	}
	v2 := p9RuleSet(t, "deny", 6)
	got2, changed, err := s.ApplyRuleSet(ctx, workspaceA, v2, policy.CreatedByCLI, 0)
	if err != nil || !changed || got2.Version != 2 || got2.RestoredFrom != 0 {
		t.Fatalf("%+v %v %v", got2, changed, err)
	}
	rb, changed, err := s.ApplyRuleSet(ctx, workspaceA, v1, policy.CreatedByCLI, 1)
	if err != nil || !changed || rb.Version != 3 || rb.RestoredFrom != 1 || !rb.Active {
		t.Fatalf("rollback must add version 3 restored from 1: %+v %v %v", rb, changed, err)
	}
	if again, changed, _ := s.ApplyRuleSet(ctx, workspaceA, v1, policy.CreatedByCLI, 1); changed || again.Version != 3 {
		t.Fatalf("rollback to the active content changes nothing: %+v %v", again, changed)
	}
	act, ok, err := s.ActiveRuleSet(ctx, workspaceA)
	if err != nil || !ok || act.Version != 3 || act.BodySHA256 != rb.BodySHA256 {
		t.Fatalf("%+v %v %v", act, ok, err)
	}
	hist, err := s.RuleSetHistory(ctx, workspaceA, 10)
	if err != nil || len(hist) != 3 || hist[0].Version != 3 || hist[1].Version != 2 || hist[2].Version != 1 {
		t.Fatalf("%+v %v", hist, err)
	}
	active := 0
	for _, h := range hist {
		if h.Active {
			active++
		}
	}
	if active != 1 || hist[1].Active || hist[2].Active {
		t.Fatalf("exactly the newest must be active: %+v", hist)
	}
	if limited, _ := s.RuleSetHistory(ctx, workspaceA, 2); len(limited) != 2 {
		t.Fatalf("limit not applied: %d", len(limited))
	}
	if old, ok, err := s.RuleSetVersion(ctx, workspaceA, 2); err != nil || !ok || old.Version != 2 {
		t.Fatalf("%+v %v %v", old, ok, err)
	}
	if _, ok, _ := s.RuleSetVersion(ctx, workspaceA, 99); ok {
		t.Fatal("missing version found")
	}
	// Workspaces never see each other's versions.
	if _, ok, _ := s.RuleSetVersion(ctx, workspaceB, 2); ok {
		t.Fatal("workspace B read workspace A's version")
	}
	if _, ok, _ := s.ActiveRuleSet(ctx, workspaceB); ok {
		t.Fatal("workspace B has A's active set")
	}
	// Bad inputs.
	if _, _, err := s.ApplyRuleSet(ctx, workspaceA, v1, policy.CreatedByMigration, 0); err == nil {
		t.Fatal("created_by migration accepted from code")
	}
	if _, _, err := s.ApplyRuleSet(ctx, workspaceA, v1, "root", 0); err == nil {
		t.Fatal("unknown created_by accepted")
	}
	if _, _, err := s.ApplyRuleSet(ctx, workspaceA, v2, policy.CreatedByCLI, -1); err == nil {
		t.Fatal("negative restored version accepted")
	}
	if _, _, err := s.ApplyRuleSet(ctx, "not-a-uuid", v1, policy.CreatedByCLI, 0); err == nil {
		t.Fatal("bad workspace id accepted")
	}
	if _, _, err := s.ApplyRuleSet(ctx, missingID, v1, policy.CreatedByCLI, 0); !errors.Is(err, policy.ErrWorkspaceNotFound) {
		t.Fatalf("unknown workspace: %v", err)
	}
	if n := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_rule_sets WHERE workspace_id=$1`, workspaceA); n != 3 {
		t.Fatalf("a refused apply left rows: %d", n)
	}
}

// TC-M3 (REQ-1): concurrent applies keep versions unique and exactly one active.
func TestP9PolicyStore_ConcurrentApplyUniqueVersions(t *testing.T) {
	ctx, pool := p9TwoWorkspaces(t)
	s, _ := NewPolicyStore(pool)
	const n = 8
	versions := make(chan int32, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(ttl int) {
			defer wg.Done()
			got, changed, err := s.ApplyRuleSet(ctx, workspaceA, p9RuleSet(t, "allow", ttl), policy.CreatedByCLI, 0)
			if err == nil && !changed {
				err = errors.New("distinct rule set reported unchanged")
			}
			errs <- err
			versions <- got.Version
		}(i)
	}
	wg.Wait()
	close(errs)
	close(versions)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int32]bool{}
	for v := range versions {
		if v < 1 || v > n || seen[v] {
			t.Fatalf("version %d duplicated or out of range", v)
		}
		seen[v] = true
	}
	if a := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_rule_sets WHERE workspace_id=$1 AND is_active`, workspaceA); a != 1 {
		t.Fatalf("%d active rows", a)
	}
	var maxVer int32
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM policy_rule_sets WHERE workspace_id=$1 AND is_active`, workspaceA).Scan(&maxVer); err != nil || maxVer != n {
		t.Fatalf("the active version must be the latest: %d %v", maxVer, err)
	}
	// The same content applied concurrently stores one version only.
	same := p9RuleSet(t, "deny", 3)
	var wg2 sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, _, err := s.ApplyRuleSet(ctx, workspaceA, same, policy.CreatedByCLI, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg2.Wait()
	if c := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_rule_sets WHERE workspace_id=$1`, workspaceA); c != n+1 {
		t.Fatalf("identical concurrent applies stored %d versions, want %d", c-n, 1)
	}
}

// TC-M4 (REQ-2, REQ-3): the engine over the real store: one decisions row per call, safe fallbacks, blanked invalid request.
func TestP9Engine_DecisionsRowsAndFallbacks(t *testing.T) {
	ctx, pool := p9TwoWorkspaces(t)
	s, _ := NewPolicyStore(pool)
	e := policy.NewEngine(s, p9Known)
	req := func(action string, tier policy.Tier) policy.Request {
		return policy.Request{WorkspaceID: workspaceA, ActorKind: policy.ActorAgent, Action: action, Tier: tier, ResourceKey: "o/r#7", InputSHA256: p9Sha("c")}
	}
	// No rule set yet: safe set.
	d, _ := e.Decide(ctx, req("github_pr_comment", policy.TierWrite))
	if d.Effect != policy.EffectApprovalRequired || d.Reason != policy.ReasonFallbackNoSet || d.RuleSetVersion != 0 {
		t.Fatalf("%+v", d)
	}
	if _, _, err := s.ApplyRuleSet(ctx, workspaceA, policy.DefaultV1(), policy.CreatedByInit, 0); err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		action string
		tier   policy.Tier
		want   policy.Effect
	}{
		{"code_search", policy.TierRead, policy.EffectAllow},
		{"github_pr_comment", policy.TierWrite, policy.EffectAllow},
		{"github_pr_merge", policy.TierDangerous, policy.EffectApprovalRequired},
		{"unknown_tool", policy.TierWrite, policy.EffectDeny},
	}
	for _, c := range calls {
		if d, err := e.Decide(ctx, req(c.action, c.tier)); err != nil || d.Effect != c.want {
			t.Fatalf("%s: %+v %v", c.action, d, err)
		}
	}
	if n := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_decisions WHERE workspace_id=$1`, workspaceA); n != 1+len(calls) {
		t.Fatalf("%d decision rows for %d calls", n, 1+len(calls))
	}
	rows, err := s.ListDecisions(ctx, workspaceA, 10)
	if err != nil || len(rows) != 5 {
		t.Fatalf("%d %v", len(rows), err)
	}
	if rows[0].Action != "unknown_tool" || rows[0].Reason != policy.ReasonUnknownAction || rows[2].Action != "github_pr_comment" ||
		rows[2].RuleSetVersion != 1 || rows[2].ResourceKey != "o/r#7" || rows[2].InputSHA256 != p9Sha("c") || rows[2].ActorKind != "agent" {
		t.Fatalf("%+v", rows)
	}
	if rows[4].RuleSetVersion != 0 || rows[4].Reason != policy.ReasonFallbackNoSet {
		t.Fatalf("%+v", rows[4])
	}
	if lim, _ := s.ListDecisions(ctx, workspaceA, 2); len(lim) != 2 || lim[0].ID != rows[0].ID {
		t.Fatalf("limit/newest-first: %+v", lim)
	}
	if other, _ := s.ListDecisions(ctx, workspaceB, 10); len(other) != 0 {
		t.Fatal("workspace B sees A's decisions")
	}
	// A body edited in the database outside the CLI is invalid: safe set, with the reason.
	mustExec(t, ctx, pool, `UPDATE policy_rule_sets SET body = jsonb_set(body,'{defaults,write}','"deny"') WHERE workspace_id=$1 AND is_active`, workspaceA)
	d, err = e.Decide(ctx, req("github_pr_comment", policy.TierWrite))
	if d.Effect != policy.EffectApprovalRequired || d.Reason != policy.ReasonFallbackInvalid || err == nil {
		t.Fatalf("%+v %v", d, err)
	}
	if d, _ := e.Decide(ctx, req("github_pr_merge", policy.TierDangerous)); d.Effect != policy.EffectDeny {
		t.Fatalf("dangerous must be denied under the safe set: %+v", d)
	}
	// Invalid request: denied, and the refusal itself is recorded with the bad fields blanked.
	before := p9Count(t, ctx, pool, `SELECT count(*) FROM policy_decisions`)
	bad := policy.Request{WorkspaceID: workspaceA, ActorKind: "root", Action: "Bad Action", Tier: "x", ResourceKey: "a b", InputSHA256: "nope", ExecutionID: -1}
	if d, _ := e.Decide(ctx, bad); d.Effect != policy.EffectDeny || d.Reason != policy.ReasonInvalidRequest {
		t.Fatalf("%+v", d)
	}
	rows, _ = s.ListDecisions(ctx, workspaceA, 1)
	if p9Count(t, ctx, pool, `SELECT count(*) FROM policy_decisions`) != before+1 || rows[0].Reason != policy.ReasonInvalidRequest || rows[0].Action != "" || rows[0].ActorKind != "" || rows[0].ResourceKey != "" || rows[0].InputSHA256 != "" {
		t.Fatalf("%+v", rows[0])
	}
	// A record that the DB refuses (unknown workspace: FK) turns the decision into deny.
	d, err = e.Decide(ctx, policy.Request{WorkspaceID: missingID, ActorKind: policy.ActorAgent, Action: "code_search", Tier: policy.TierRead})
	if d.Effect != policy.EffectDeny || d.Reason != policy.ReasonRecordFailed || err == nil {
		t.Fatalf("a decision that cannot be recorded must deny: %+v %v", d, err)
	}
}

// TC-M5 (REQ-6): migration 00036 shape: kinds, binding columns, one open gate per binding, consumed_at only on approved gates.
func TestP9Migration36_GateApprovalsConstraints(t *testing.T) {
	ctx, pool := p9TwoWorkspaces(t)
	s, _ := NewPolicyStore(pool)
	mustExec(t, ctx, pool, `INSERT INTO users(id,display_name) VALUES('019a6690-abcd-7000-8000-000000000301','M')`)
	member := "019a6690-abcd-7000-8000-000000000301"
	mustExec(t, ctx, pool, `INSERT INTO workspace_members(user_id,workspace_id,role) VALUES($1,$2,'owner')`, member, workspaceA)
	exp := time.Now().Add(time.Hour)
	req := policy.GateRequest{WorkspaceID: workspaceA, Action: "github_pr_merge", ResourceKey: "o/r#7", InputSHA256: p9Sha("a"), ExpiresAt: exp}
	g1, err := s.CreateGate(ctx, req)
	if err != nil || !g1.Created || g1.ID == 0 || len(g1.Nonce) < 16 {
		t.Fatalf("%+v %v", g1, err)
	}
	// Dedupe while open.
	g1b, err := s.CreateGate(ctx, req)
	if err != nil || g1b.Created || g1b.ID != g1.ID || g1b.Nonce != g1.Nonce {
		t.Fatalf("an open gate must be returned, not duplicated: %+v %v", g1b, err)
	}
	// A different binding is a different gate.
	for name, mod := range map[string]func(r *policy.GateRequest){
		"hash":      func(r *policy.GateRequest) { r.InputSHA256 = p9Sha("b") },
		"resource":  func(r *policy.GateRequest) { r.ResourceKey = "o/r#8" },
		"action":    func(r *policy.GateRequest) { r.Action = "git_push" },
		"workspace": func(r *policy.GateRequest) { r.WorkspaceID = workspaceB },
	} {
		r2 := req
		mod(&r2)
		g, err := s.CreateGate(ctx, r2)
		if err != nil || !g.Created || g.ID == g1.ID {
			t.Fatalf("%s: %+v %v", name, g, err)
		}
	}
	// Nonces are unique and well-formed.
	if p9Count(t, ctx, pool, `SELECT count(DISTINCT nonce) FROM approvals WHERE kind='policy_gate'`) != 5 {
		t.Fatal("nonce collision")
	}
	// After the owner decides, the next request is a new gate.
	mustExec(t, ctx, pool, `UPDATE approvals SET decision='rejected', decided_at=now() WHERE id=$1`, g1.ID)
	g2, err := s.CreateGate(ctx, req)
	if err != nil || !g2.Created || g2.ID == g1.ID {
		t.Fatalf("after a decision a new gate is needed: %+v %v", g2, err)
	}
	// consumed_at only on approved policy gates.
	_, err = pool.Exec(ctx, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, g2.ID)
	requirePGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, g1.ID)
	requirePGCode(t, err, "23514")
	mustExec(t, ctx, pool, `UPDATE approvals SET decision='approved', decided_at=now() WHERE id=$1`, g2.ID)
	mustExec(t, ctx, pool, `UPDATE approvals SET consumed_at=now() WHERE id=$1`, g2.ID)
	mustExec(t, ctx, pool, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at,decision,decided_at) VALUES($1,'confirm_action','a','r','{}','nonce-0123456789abcdef',now()+interval '1 hour','approved',now())`, workspaceA)
	_, err = pool.Exec(ctx, `UPDATE approvals SET consumed_at=now() WHERE kind='confirm_action'`)
	requirePGCode(t, err, "23514")
	// A policy gate always names action, resource and hash.
	_, err = pool.Exec(ctx, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at) VALUES($1,'policy_gate','a','r','{}','nonce-0123456789abcdeg',now()+interval '1 hour')`, workspaceA)
	requirePGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at,policy_action,resource_key,input_sha256) VALUES($1,'policy_gate','a','r','{}','nonce-0123456789abcdeh',now()+interval '1 hour','Bad Name','k','')`, workspaceA)
	requirePGCode(t, err, "23514")
	// Invalid requests are refused before the DB.
	for name, mod := range map[string]func(r *policy.GateRequest){
		"bad hash":      func(r *policy.GateRequest) { r.InputSHA256 = "xyz" },
		"empty hash":    func(r *policy.GateRequest) { r.InputSHA256 = "" },
		"bad action":    func(r *policy.GateRequest) { r.Action = "Bad Action" },
		"long resource": func(r *policy.GateRequest) { r.ResourceKey = strings.Repeat("a", 257) },
		"space":         func(r *policy.GateRequest) { r.ResourceKey = "a b" },
		"no expiry":     func(r *policy.GateRequest) { r.ExpiresAt = time.Time{} },
		"neg execution": func(r *policy.GateRequest) { r.ExecutionID = -1 },
		"bad user":      func(r *policy.GateRequest) { r.RequestedUserID = "nope" },
		"bad workspace": func(r *policy.GateRequest) { r.WorkspaceID = "nope" },
	} {
		r2 := req
		r2.ResourceKey = "fresh/" + name
		mod(&r2)
		if _, err := s.CreateGate(ctx, r2); !errors.Is(err, ErrInvalidGateRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// FK to workspace_members: a member works, a stranger is refused by the database.
	r3 := req
	r3.ResourceKey, r3.RequestedUserID = "member/1", member
	if g, err := s.CreateGate(ctx, r3); err != nil || !g.Created {
		t.Fatalf("%+v %v", g, err)
	}
	mustExec(t, ctx, pool, `INSERT INTO users(id,display_name) VALUES('019a6690-abcd-7000-8000-000000000302','Stranger')`)
	r3.ResourceKey, r3.RequestedUserID = "member/2", "019a6690-abcd-7000-8000-000000000302"
	if _, err := s.CreateGate(ctx, r3); err == nil || errors.Is(err, ErrInvalidGateRequest) {
		t.Fatalf("a user who is not a member of the workspace was accepted: %v", err)
	}
	// Down removes policy gates, columns and the kind; Up restores them.
	mustExec(t, ctx, pool, downSection(t, "00036_policy_gate_approvals.sql"))
	if p9Count(t, ctx, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='approvals' AND column_name IN ('policy_action','resource_key','input_sha256','execution_id','requested_user_id','decided_user_id','consumed_at')`) != 0 {
		t.Fatal("Down left columns")
	}
	if p9Count(t, ctx, pool, `SELECT count(*) FROM approvals WHERE kind='policy_gate'`) != 0 {
		t.Fatal("Down left policy gates")
	}
	_, err = pool.Exec(ctx, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at) VALUES($1,'policy_gate','a','r','{}','nonce-0123456789abcdei',now()+interval '1 hour')`, workspaceA)
	requirePGCode(t, err, "23514")
	mustExec(t, ctx, pool, upSection(t, "00036_policy_gate_approvals.sql"))
	if p9Count(t, ctx, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='approvals' AND column_name='consumed_at'`) != 1 {
		t.Fatal("Up did not restore consumed_at")
	}
}

// ---- grants -------------------------------------------------------------

type grantEnvDB struct {
	ctx  context.Context
	pool *pgxpool.Pool
	runs *ToolRunStore
	gate *PolicyStore
	job  int64
}

func newGrantDB(t *testing.T) grantEnvDB {
	t.Helper()
	ctx, pool, runs, job := newToolRunDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'B')`, workspaceB)
	gate, err := NewPolicyStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return grantEnvDB{ctx: ctx, pool: pool, runs: runs, gate: gate, job: job}
}

// approvedGate creates and approves a gate for github_pr_merge on o/r#7.
func (g grantEnvDB) approvedGate(t *testing.T, resource, hash string) int64 {
	t.Helper()
	gate, err := g.gate.CreateGate(g.ctx, policy.GateRequest{WorkspaceID: workspaceA, Action: "github_pr_merge", ResourceKey: resource, InputSHA256: hash, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, g.ctx, g.pool, `UPDATE approvals SET decision='approved', decided_at=now() WHERE id=$1`, gate.ID)
	return gate.ID
}

func (g grantEnvDB) check(id int64) tools.GrantCheck {
	return tools.GrantCheck{ApprovalID: id, WorkspaceID: workspaceA, Action: "github_pr_merge", ResourceKey: "o/r#7", InputSHA256: p9Sha("a")}
}

func (g grantEnvDB) consumed(t *testing.T, id int64) bool {
	t.Helper()
	var c *time.Time
	if err := g.pool.QueryRow(g.ctx, `SELECT consumed_at FROM approvals WHERE id=$1`, id).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c != nil
}

// TC-GR1 (REQ-7): a valid grant is consumed in the Begin transaction and records the started run.
func TestP9BeginGranted_ValidConsumesOnce(t *testing.T) {
	g := newGrantDB(t)
	id := g.approvedGate(t, "o/r#7", p9Sha("a"))
	runID, prior, err := g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "k1"), g.check(id))
	if err != nil || prior != nil || runID == 0 {
		t.Fatalf("%d %+v %v", runID, prior, err)
	}
	if !g.consumed(t, id) {
		t.Fatal("grant not consumed")
	}
	var st string
	if err := g.pool.QueryRow(g.ctx, `SELECT status FROM tool_runs WHERE id=$1`, runID).Scan(&st); err != nil || st != tools.StatusStarted {
		t.Fatalf("%s %v", st, err)
	}
	// A different call with the used grant is refused and writes nothing.
	rows := countRows(t, g.ctx, g.pool, "tool_runs")
	if _, _, err := g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "k2"), g.check(id)); !errors.Is(err, tools.ErrGrantInvalid) {
		t.Fatalf("consumed grant accepted: %v", err)
	}
	if countRows(t, g.ctx, g.pool, "tool_runs") != rows {
		t.Fatal("a refused grant wrote a tool run")
	}
}

// TC-GR2 (REQ-7): every binding mismatch refuses the grant, consumes nothing and writes nothing.
func TestP9BeginGranted_MismatchRefused(t *testing.T) {
	g := newGrantDB(t)
	mustExec(t, g.ctx, g.pool, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at,decision,decided_at) VALUES($1,'confirm_action','a','r','{}','nonce-0123456789abcdef',now()+interval '1 hour','approved',now())`, workspaceA)
	var otherKind int64
	if err := g.pool.QueryRow(g.ctx, `SELECT id FROM approvals WHERE kind='confirm_action'`).Scan(&otherKind); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart){
		"other action": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.Action = "git_push"
			return id, c, start(g.job, "github_pr_merge", tenant.TierDangerous, "m1")
		},
		"other resource key": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.ResourceKey = "o/r#8"
			return id, c, start(g.job, "github_pr_merge", tenant.TierDangerous, "m2")
		},
		"other input hash": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.InputSHA256 = p9Sha("b")
			return id, c, start(g.job, "github_pr_merge", tenant.TierDangerous, "m3")
		},
		"other execution": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.ExecutionID = 5
			return id, c, start(g.job, "github_pr_merge", tenant.TierDangerous, "m4")
		},
		"expired": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			mustExec(t, g.ctx, g.pool, `UPDATE approvals SET expires_at=now()-interval '1 minute' WHERE id=$1`, id)
			return id, g.check(id), start(g.job, "github_pr_merge", tenant.TierDangerous, "m5")
		},
		"not approved (open)": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			gate, err := g.gate.CreateGate(g.ctx, policy.GateRequest{WorkspaceID: workspaceA, Action: "github_pr_merge", ResourceKey: "o/r#7", InputSHA256: p9Sha("a"), ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			return gate.ID, g.check(gate.ID), start(g.job, "github_pr_merge", tenant.TierDangerous, "m6")
		},
		"rejected": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			mustExec(t, g.ctx, g.pool, `UPDATE approvals SET decision='rejected' WHERE id=$1`, id)
			return id, g.check(id), start(g.job, "github_pr_merge", tenant.TierDangerous, "m7")
		},
		"other workspace": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.WorkspaceID = workspaceB
			s := start(g.job, "github_pr_merge", tenant.TierDangerous, "m8")
			s.WorkspaceID = workspaceB
			return id, c, s
		},
		"grant workspace differs from run": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			c := g.check(id)
			c.WorkspaceID = workspaceB
			return id, c, start(g.job, "github_pr_merge", tenant.TierDangerous, "m9")
		},
		"approval of another kind": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			return otherKind, g.check(otherKind), start(g.job, "github_pr_merge", tenant.TierDangerous, "m10")
		},
		"no approval id": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			return 0, g.check(0), start(g.job, "github_pr_merge", tenant.TierDangerous, "m11")
		},
		"unknown approval id": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			return 0, g.check(987654), start(g.job, "github_pr_merge", tenant.TierDangerous, "m12")
		},
		"read tier": func(t *testing.T) (int64, tools.GrantCheck, tools.RunStart) {
			id := g.approvedGate(t, "o/r#7", p9Sha("a"))
			return id, g.check(id), start(g.job, "code_search", tenant.TierRead, "m13")
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			id, check, run := mk(t)
			rows := countRows(t, g.ctx, g.pool, "tool_runs")
			runID, prior, err := g.runs.BeginGranted(g.ctx, run, check)
			if !errors.Is(err, tools.ErrGrantInvalid) || runID != 0 || prior != nil {
				t.Fatalf("%d %+v %v", runID, prior, err)
			}
			if countRows(t, g.ctx, g.pool, "tool_runs") != rows {
				t.Fatal("a refused grant wrote a tool run")
			}
			if id > 0 && g.consumed(t, id) {
				t.Fatal("a refused grant was consumed")
			}
		})
	}
}

// TC-GR3 (REQ-7): the same idempotency key replays without consuming a second grant.
func TestP9BeginGranted_RetryReplays(t *testing.T) {
	g := newGrantDB(t)
	id := g.approvedGate(t, "o/r#7", p9Sha("a"))
	runID, _, err := g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "same"), g.check(id))
	if err != nil {
		t.Fatal(err)
	}
	spare := g.approvedGate(t, "o/r#7", p9Sha("a")) // the first gate is decided, so this is a new one
	if spare == id {
		t.Fatal("expected a second gate")
	}
	// Crash before Finish: the retry reports unknown_outcome and consumes nothing.
	_, prior, err := g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "same"), g.check(spare))
	if err != nil || prior == nil || prior.Status != tools.StatusUnknownOutcome || g.consumed(t, spare) {
		t.Fatalf("%+v %v consumed=%v", prior, err, g.consumed(t, spare))
	}
	if err := g.runs.Finish(g.ctx, tools.RunFinish{RunID: runID, Status: tools.StatusOK, Summary: "merged #7"}); err != nil {
		t.Fatal(err)
	}
	_, prior, err = g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "same"), g.check(spare))
	if err != nil || prior == nil || prior.Status != tools.StatusOK || prior.Summary != "merged #7" || g.consumed(t, spare) {
		t.Fatalf("%+v %v", prior, err)
	}
	// Replay needs no valid grant at all (the earlier run is the answer).
	if _, prior, err = g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "same"), g.check(id)); err != nil || prior == nil {
		t.Fatalf("%+v %v", prior, err)
	}
}

// TC-GR4 (REQ-7): concurrent calls under one grant: exactly one wins.
func TestP9BeginGranted_ConcurrentOneWinner(t *testing.T) {
	g := newGrantDB(t)
	id := g.approvedGate(t, "o/r#7", p9Sha("a"))
	const n = 6
	var wins, refused int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, prior, err := g.runs.BeginGranted(g.ctx, start(g.job, "github_pr_merge", tenant.TierDangerous, "race-"+strconv.Itoa(i)), g.check(id))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && prior == nil:
				wins++
			case errors.Is(err, tools.ErrGrantInvalid):
				refused++
			default:
				t.Errorf("unexpected: %+v %v", prior, err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || refused != n-1 {
		t.Fatalf("wins=%d refused=%d", wins, refused)
	}
	if got := p9Count(t, g.ctx, g.pool, `SELECT count(*) FROM tool_runs WHERE tool='github_pr_merge'`); got != 1 {
		t.Fatalf("%d tool runs for one grant", got)
	}
}

// TC-GR5 (REQ-7): through the executor, the real store and the real engine: dangerous refused without a grant, runs once with one.
func TestP9Executor_EndToEndWithRealEngineAndStore(t *testing.T) {
	g := newGrantDB(t)
	if _, _, err := g.gate.ApplyRuleSet(g.ctx, workspaceA, policy.DefaultV1(), policy.CreatedByInit, 0); err != nil {
		t.Fatal(err)
	}
	engine := policy.NewEngine(g.gate, p9Known)
	tool := &p9MergeTool{}
	reg, err := tools.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	pol := tenant.ToolPolicyConfig{MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"p": {Name: "p"}}, Repos: map[string]tenant.RepoGrant{"o/r": {Repo: "o/r", Account: "p", Allow: []string{"github_pr_merge"}}}}
	input := []byte(`{"repo":"o/r","number":7}`)
	newExec := func() *tools.Executor {
		return tools.NewExecutor(reg, pol, tools.DefaultLimits(), g.runs, p9San{}, g.job, workspaceA, tools.WithPolicy(engine, policy.ActorAgent))
	}
	r := newExec().Call(g.ctx, 1, "github_pr_merge", input)
	if r.Status != tools.StatusDenied || !strings.Contains(r.Content, "approval_required") || tool.ran != 0 {
		t.Fatalf("%+v", r)
	}
	var code string
	if err := g.pool.QueryRow(g.ctx, `SELECT error_code FROM tool_runs WHERE tool='github_pr_merge' AND status='denied'`).Scan(&code); err != nil || code != tools.DenyApprovalRequired {
		t.Fatalf("%q %v", code, err)
	}
	rk := tools.ResourceKey(tool, input)
	gate, err := g.gate.CreateGate(g.ctx, policy.GateRequest{WorkspaceID: workspaceA, Action: "github_pr_merge", ResourceKey: rk, InputSHA256: tools.InputSHA256(input), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Unapproved: refused.
	grantCtx := tools.WithApprovalGrant(g.ctx, tools.ApprovalGrant{ApprovalID: gate.ID})
	if r := newExec().Call(grantCtx, 2, "github_pr_merge", input); r.Status != tools.StatusDenied || !strings.Contains(r.Content, tools.DenyRevalidateDenied) || tool.ran != 0 {
		t.Fatalf("%+v", r)
	}
	mustExec(t, g.ctx, g.pool, `UPDATE approvals SET decision='approved', decided_at=now() WHERE id=$1`, gate.ID)
	if r := newExec().Call(grantCtx, 3, "github_pr_merge", input); r.IsError || r.Status != tools.StatusOK || tool.ran != 1 {
		t.Fatalf("%+v ran=%d", r, tool.ran)
	}
	if !g.consumed(t, gate.ID) {
		t.Fatal("grant not consumed")
	}
	// A retry replays; the tool does not run again.
	if r := newExec().Call(grantCtx, 4, "github_pr_merge", input); r.IsError || tool.ran != 1 {
		t.Fatalf("%+v ran=%d", r, tool.ran)
	}
	// Every call left exactly one decisions row; the revalidation row names the approval and the run.
	rows, _ := g.gate.ListDecisions(g.ctx, workspaceA, 50)
	var reval int
	for _, d := range rows {
		if d.ApprovalID == gate.ID && d.ToolRunID > 0 {
			reval++
		}
	}
	if reval != 1 {
		t.Fatalf("%d revalidation rows: %+v", reval, rows)
	}
}

type p9MergeTool struct {
	mu  sync.Mutex
	ran int
}

func (p *p9MergeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "github_pr_merge", Tier: tenant.TierDangerous, InputSchema: []byte(`{"type":"object"}`)}
}
func (p *p9MergeTool) Repo(json.RawMessage) (string, error) { return "o/r", nil }
func (p *p9MergeTool) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ran++
	return tools.ToolOutput{Text: "merged", Summary: "merged"}, nil
}

type p9San struct{}

func (p9San) CleanOutbound(s string, _ bool) string { return s }
