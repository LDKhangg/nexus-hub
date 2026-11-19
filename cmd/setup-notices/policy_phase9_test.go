package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 9 Batch A: the owner's `nh policy` commands (REQ-1, REQ-3, REQ-5).

const p9File = `schema: policy/v1
defaults:
  read: allow
  write: %WRITE%
  dangerous: approval_required
actions:
  github_pr_merge: approval_required
approval_ttl_hours: 24
`

func p9Write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func p9Run(ctx context.Context, pool *pgxpool.Pool, args ...string) (string, error) {
	var out bytes.Buffer
	err := runCommand(ctx, pool, args, &out, nil)
	return out.String(), err
}

func p9Lines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		rows = append(rows, m)
	}
	return rows
}

// TC-C1 (REQ-1, REQ-5): nh init gives the workspace v1; show prints it as a policy/v1 file.
func TestP9CLI_InitSeedsV1AndShow(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	var n int
	var by string
	if err := pool.QueryRow(ctx, `SELECT count(*), min(created_by) FROM policy_rule_sets WHERE workspace_id=$1 AND version=1 AND is_active`, ws).Scan(&n, &by); err != nil || n != 1 || by != "init" {
		t.Fatalf("init did not seed v1: n=%d by=%s %v", n, by, err)
	}
	out, err := p9Run(ctx, pool, "policy", "show", "--workspace-id", ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# rule set version 1") || !strings.Contains(out, "github_pr_merge: approval_required") || !strings.Contains(out, "dangerous: approval_required") {
		t.Fatalf("%s", out)
	}
	// What show prints is a valid file with the same hash.
	file := p9Write(t, out[strings.Index(out, "\n")+1:])
	val, err := p9Run(ctx, pool, "policy", "validate", file)
	if err != nil {
		t.Fatal(err)
	}
	rows := p9Lines(t, val)
	_, sum, _ := policy.Canonical(policy.DefaultV1())
	if len(rows) != 1 || rows[0]["valid"] != true || rows[0]["body_sha256"] != sum {
		t.Fatalf("%v", rows)
	}
}

// TC-C2 (REQ-1): validate checks a file and changes nothing; errors name paths, never values.
func TestP9CLI_ValidateNeverWritesAndNeverEchoes(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	before := countRulesets(t, ctx, pool)
	ok := strings.Replace(p9File, "%WRITE%", "allow", 1)
	cases := []struct{ name, src, want string }{
		{"unknown action", strings.Replace(ok, "github_pr_merge", "no_such_tool", 1), "unknown action"},
		{"bad effect", strings.Replace(p9File, "%WRITE%", "hunter2", 1), "defaults.write"},
		{"unknown key", ok + "extra: 1\n", "extra"},
		{"ttl", strings.Replace(ok, "approval_ttl_hours: 24", "approval_ttl_hours: 500", 1), "approval_ttl_hours"},
	}
	for _, c := range cases {
		_, err := p9Run(ctx, pool, "policy", "validate", p9Write(t, c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if _, err := p9Run(ctx, pool, "policy", "validate", filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "cannot read file") {
		t.Fatalf("%v", err)
	}
	big := p9Write(t, strings.Replace(p9File, "%WRITE%", "allow", 1)+"# "+strings.Repeat("x", policy.MaxSourceBytes))
	if _, err := p9Run(ctx, pool, "policy", "validate", big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("%v", err)
	}
	// validate takes no workspace and no extra arguments.
	good := p9Write(t, strings.Replace(p9File, "%WRITE%", "allow", 1))
	for _, args := range [][]string{{"policy", "validate", good, "--workspace-id", ws}, {"policy", "validate"}, {"policy", "validate", good, good}, {"policy", "validate", good, "--limit", "5"}} {
		if _, err := p9Run(ctx, pool, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if countRulesets(t, ctx, pool) != before {
		t.Fatal("validate changed the rule sets")
	}
}

func countRulesets(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM policy_rule_sets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TC-C3 (REQ-1): apply, same-file apply, history, rollback, rollback of an invalid old version.
func TestP9CLI_ApplyHistoryRollback(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	denyWrites := p9Write(t, strings.Replace(p9File, "%WRITE%", "deny", 1))
	out, err := p9Run(ctx, pool, "policy", "apply", denyWrites, "--workspace-id", ws)
	if err != nil {
		t.Fatal(err)
	}
	r := p9Lines(t, out)[0]
	if r["version"] != float64(2) || r["changed"] != true || r["active"] != true {
		t.Fatalf("%v", r)
	}
	out, _ = p9Run(ctx, pool, "policy", "apply", denyWrites, "--workspace-id", ws)
	if r := p9Lines(t, out)[0]; r["version"] != float64(2) || r["changed"] != false {
		t.Fatalf("same file: %v", r)
	}
	// Flags may come before or after the positional argument.
	if _, err := p9Run(ctx, pool, "policy", "apply", "--workspace-id", ws, denyWrites); err != nil {
		t.Fatal(err)
	}
	out, err = p9Run(ctx, pool, "policy", "show", "--workspace-id", ws)
	if err != nil || !strings.Contains(out, "version 2") || !strings.Contains(out, "write: deny") {
		t.Fatalf("%s %v", out, err)
	}
	out, err = p9Run(ctx, pool, "policy", "rollback", "1", "--workspace-id", ws)
	if err != nil {
		t.Fatal(err)
	}
	r = p9Lines(t, out)[0]
	if r["version"] != float64(3) || r["restored_from_version"] != float64(1) || r["changed"] != true {
		t.Fatalf("%v", r)
	}
	if out, _ := p9Run(ctx, pool, "policy", "show", "--workspace-id", ws); !strings.Contains(out, "write: allow") || !strings.Contains(out, "version 3") {
		t.Fatalf("%s", out)
	}
	hist, err := p9Run(ctx, pool, "policy", "history", "--workspace-id", ws)
	if err != nil {
		t.Fatal(err)
	}
	rows := p9Lines(t, hist)
	if len(rows) != 3 || rows[0]["version"] != float64(3) || rows[0]["active"] != true || rows[0]["restored_from_version"] != float64(1) || rows[1]["active"] != false || rows[2]["version"] != float64(1) || rows[2]["created_by"] != "init" || rows[1]["created_by"] != "cli" {
		t.Fatalf("%v", rows)
	}
	if strings.Contains(hist, "approval_required") || strings.Contains(hist, `"body"`) {
		t.Fatal("history must not print rule bodies")
	}
	// A rule set written to the DB that names a tool that no longer exists cannot be restored.
	rs := p9ParseForCLI(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\nactions:\n  vanished_tool: allow\n")
	body, sum, _ := policy.Canonical(rs)
	if _, err := pool.Exec(ctx, `INSERT INTO policy_rule_sets(workspace_id,version,schema,body,body_sha256,created_by) VALUES($1,4,'policy/v1',$2,$3,'cli')`, ws, string(body), sum); err != nil {
		t.Fatal(err)
	}
	if _, err := p9Run(ctx, pool, "policy", "rollback", "4", "--workspace-id", ws); err == nil || !strings.Contains(err.Error(), "not valid with the current tools") {
		t.Fatalf("%v", err)
	}
	// A body edited in the database cannot be restored either.
	if _, err := pool.Exec(ctx, `UPDATE policy_rule_sets SET body=jsonb_set(body,'{defaults,read}','"deny"') WHERE workspace_id=$1 AND version=2`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := p9Run(ctx, pool, "policy", "rollback", "2", "--workspace-id", ws); err == nil {
		t.Fatal("rollback restored a tampered version")
	}
	for _, v := range []string{"0", "-1", "abc", "1.5", "99999999999", "99"} {
		if _, err := p9Run(ctx, pool, "policy", "rollback", v, "--workspace-id", ws); err == nil {
			t.Fatalf("rollback %s accepted", v)
		}
	}
	if countRulesets(t, ctx, pool) != 4 {
		t.Fatal("a refused rollback stored a version")
	}
}

func p9ParseForCLI(t *testing.T, src string) policy.RuleSet {
	t.Helper()
	rs, err := policy.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// TC-C4 (REQ-1): --limit bounds, usage errors, bad workspace ids.
func TestP9CLI_LimitsAndUsage(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	file := p9Write(t, strings.Replace(p9File, "%WRITE%", "deny", 1))
	if _, err := p9Run(ctx, pool, "policy", "apply", file, "--workspace-id", ws); err != nil {
		t.Fatal(err)
	}
	out, err := p9Run(ctx, pool, "policy", "history", "--workspace-id", ws, "--limit", "1")
	if err != nil || len(p9Lines(t, out)) != 1 {
		t.Fatalf("%s %v", out, err)
	}
	for _, sub := range []string{"history", "decisions"} {
		for _, bad := range []string{"0", "-5", "501", "x"} {
			if _, err := p9Run(ctx, pool, "policy", sub, "--workspace-id", ws, "--limit", bad); err == nil {
				t.Fatalf("%s --limit %s accepted", sub, bad)
			}
		}
		for _, ok := range []string{"1", "500"} {
			if _, err := p9Run(ctx, pool, "policy", sub, "--workspace-id", ws, "--limit", ok); err != nil {
				t.Fatalf("%s --limit %s: %v", sub, ok, err)
			}
		}
	}
	for _, args := range [][]string{
		{"policy"}, {"policy", "bogus"}, {"policy", "show"}, {"policy", "show", "--workspace-id", "not-a-uuid"},
		{"policy", "show", "--workspace-id", ws, "--limit", "5"}, {"policy", "show", "extra", "--workspace-id", ws},
		{"policy", "apply", "--workspace-id", ws}, {"policy", "rollback", "--workspace-id", ws},
		{"policy", "history", "--workspace-id", ws, "--nope"},
	} {
		if _, err := p9Run(ctx, pool, args...); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	// A workspace that does not exist.
	if _, err := p9Run(ctx, pool, "policy", "apply", file, "--workspace-id", "019a6690-abcd-7000-8000-0000000000ff"); err == nil || !strings.Contains(err.Error(), "workspace not found") {
		t.Fatalf("%v", err)
	}
	// show on a workspace without a rule set prints the safe set and says so.
	if _, err := pool.Exec(ctx, `UPDATE policy_rule_sets SET is_active=false WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	out, err = p9Run(ctx, pool, "policy", "show", "--workspace-id", ws)
	if err != nil || !strings.Contains(out, "no active rule set") || !strings.Contains(out, "write: approval_required") || !strings.Contains(out, "dangerous: deny") {
		t.Fatalf("%s %v", out, err)
	}
}

// TC-C5 (REQ-3): decisions lists the newest first, within the limit, with ids and names only.
func TestP9CLI_Decisions(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	if out, err := p9Run(ctx, pool, "policy", "decisions", "--workspace-id", ws); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("%q %v", out, err)
	}
	store, err := storage.NewPolicyStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	engine := policy.NewEngine(store, policyKnownActions())
	for _, a := range []struct {
		name string
		tier policy.Tier
	}{{"github_pr_comment", policy.TierWrite}, {"github_pr_merge", policy.TierDangerous}, {"not_a_tool", policy.TierWrite}} {
		engine.Decide(ctx, policy.Request{WorkspaceID: ws, ActorKind: policy.ActorAgent, Action: a.name, Tier: a.tier, ResourceKey: "o/r#9", InputSHA256: strings.Repeat("d", 64)})
	}
	out, err := p9Run(ctx, pool, "policy", "decisions", "--workspace-id", ws, "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	rows := p9Lines(t, out)
	if len(rows) != 2 || rows[0]["action"] != "not_a_tool" || rows[0]["effect"] != "deny" || rows[0]["reason"] != "unknown_action" ||
		rows[1]["action"] != "github_pr_merge" || rows[1]["effect"] != "approval_required" || rows[1]["rule_key"] != "actions.github_pr_merge" ||
		rows[1]["resource_key"] != "o/r#9" || rows[1]["rule_set_version"] != float64(1) || rows[1]["actor_kind"] != "agent" {
		t.Fatalf("%v", rows)
	}
	for _, k := range []string{"input", "body", "text", "prompt", "token"} {
		if _, ok := rows[0][k]; ok {
			t.Fatalf("decision row has a %q field", k)
		}
	}
}

// TC-C6 (REQ-1): the registry the CLI validates against knows the tools and capabilities rule sets name.
func TestP9CLI_KnownActionsCoverToolsAndCapabilities(t *testing.T) {
	known := policyKnownActions()
	for _, name := range []string{"github_pr_merge", "github_pr_comment", "github_pr_approve", "clickup_post_comment", "git_push", "code_search", "github.merge"} {
		if !known[name] {
			t.Errorf("%s is not a known action for rule sets", name)
		}
	}
	for _, name := range []string{"", "rm_rf", "GITHUB_PR_MERGE"} {
		if known[name] {
			t.Errorf("%q is known", name)
		}
	}
}

// TC-C7 (REQ-1, REQ-5): only the policy command (and init's EnsureDefault) write rule sets.
func TestP9OnlyPolicyCLIWritesRuleSets(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		filepath.ToSlash(filepath.Join("cmd", "setup-notices", "policy.go")):                  true,
		filepath.ToSlash(filepath.Join("internal", "adapters", "storage", "policy_store.go")): true,
		filepath.ToSlash(filepath.Join("internal", "policy", "engine.go")):                    true,
	}
	ensureAllowed := map[string]bool{
		filepath.ToSlash(filepath.Join("cmd", "setup-notices", "init.go")):                    true,
		filepath.ToSlash(filepath.Join("internal", "adapters", "storage", "policy_store.go")): true,
	}
	sqlcAllowed := filepath.ToSlash(filepath.Join("internal", "adapters", "storage", "sqlc")) + "/"
	var callers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && (rel == ".git" || rel == ".claude" || rel == ".superpowers" || rel == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(data)
		if strings.Contains(src, "ApplyRuleSet(") && !allowed[rel] {
			callers = append(callers, rel+": ApplyRuleSet")
		}
		if strings.Contains(src, "EnsureDefaultPolicyRuleSet(") && !ensureAllowed[rel] && !strings.HasPrefix(rel, sqlcAllowed) {
			callers = append(callers, rel+": EnsureDefaultPolicyRuleSet")
		}
		if (strings.Contains(src, "InsertPolicyRuleSet(") || strings.Contains(src, "DeactivatePolicyRuleSets(")) &&
			rel != "internal/adapters/storage/policy_store.go" && !strings.HasPrefix(rel, sqlcAllowed) {
			callers = append(callers, rel+": InsertPolicyRuleSet")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 0 {
		t.Fatalf("code outside nh policy writes rule sets: %v", callers)
	}
}

// TC-C8 (REQ-8): `nh settings` shows allow_dangerous as deprecated, and only that key.
func TestP9CLI_SettingsShowsAllowDangerousDeprecated(t *testing.T) {
	ctx, pool := setupDB(t)
	out, err := p9Run(ctx, pool, "settings", "get", "allow_dangerous")
	if err != nil {
		t.Fatal(err)
	}
	row := p9Lines(t, out)[0]
	if note, _ := row["deprecated"].(string); !strings.Contains(note, "deprecated") || !strings.Contains(note, "no effect") {
		t.Fatalf("%v", row)
	}
	out, err = p9Run(ctx, pool, "settings", "set", "allow_dangerous", "true")
	if err != nil {
		t.Fatal(err)
	}
	if row := p9Lines(t, out)[0]; row["deprecated"] == nil || row["value"] != true {
		t.Fatalf("a set must still be stored and shown as deprecated: %v", row)
	}
	out, err = p9Run(ctx, pool, "settings", "list")
	if err != nil {
		t.Fatal(err)
	}
	dep := 0
	for _, r := range p9Lines(t, out) {
		if r["deprecated"] != nil {
			dep++
			if r["key"] != "allow_dangerous" {
				t.Fatalf("%v is marked deprecated", r["key"])
			}
		}
	}
	if dep != 1 {
		t.Fatalf("%d deprecated rows in settings list", dep)
	}
	if out, _ := p9Run(ctx, pool, "settings", "get", "max_input_tokens"); strings.Contains(out, "deprecated") {
		t.Fatalf("%s", out)
	}
}

// TC-C9 (REQ-8): allow_dangerous is read by nothing that decides; only the settings
// package, the startup warning and the retired snapshot field mention it.
func TestP9AllowDangerousDecidesNothing(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		"internal/settings/settings.go":      true,
		"cmd/server/main.go":                 true,
		"internal/agent/workflow_adapter.go": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && (rel == ".git" || rel == ".claude" || rel == ".superpowers") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || allowed[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "AllowDangerous") {
			t.Errorf("%s still uses AllowDangerous", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The one place that reads the setting only logs, and says it has no effect.
	main, err := os.ReadFile(filepath.Join(root, "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(main), "s.AllowDangerous")
	if i < 0 || !strings.Contains(string(main)[i:i+300], "no effect") {
		t.Fatal("the startup line for allow_dangerous is missing or does not say it has no effect")
	}
}
