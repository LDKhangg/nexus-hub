package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TC-25 (REQ-12), TC-31 (REQ-15): run the documented Phase 2 CLI argv in
// process, substituting only IDs, fake values and a temporary master-key path.
func TestApproved_RunbookPhase2CommandsWithFakes(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, _ := initWorkspace(t, ctx, pool, "--bot-token-ref", "telegram", "--webhook-secret-ref", "clickup-webhook")
	path := filepath.Join(t.TempDir(), "master.key")
	var out bytes.Buffer
	if err := run([]string{"master-key", "init", "--path", path}, &out, nil); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) { return path, name == "NEXUS_MASTER_KEY_FILE" }
	for _, item := range []struct{ name, kind string }{{"telegram", "telegram_bot"}, {"anthropic", "anthropic_key"}, {"clickup", "clickup_token"}, {"gh_personal", "github_pat"}} {
		withStdin(t, "fake-runbook-private-"+item.name)
		if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", item.name, "--kind", item.kind}, &out, lookup); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{
		{"secret", "list", "--workspace-id", ws},
		{"settings", "list"},
		{"settings", "set", "max_input_tokens", "40000"},
		{"settings", "set", "listen_addr", "127.0.0.1:8080"},
		{"github-account", "add", "--source-id", source, "--name", "personal", "--pat-ref", "gh_personal", "--login", "alice", "--commit-name", "Nexus Bot", "--commit-email", "nexus@example.invalid"},
		{"github-account", "add", "--source-id", source, "--name", "workbot", "--app-id", "123", "--installation-id", "456", "--key-ref", "gh_app_key", "--commit-name", "nexus[bot]", "--commit-email", "nexus@example.invalid"},
		{"github-repo", "set", "--source-id", source, "--repo", "owner/name", "--account", "personal", "--managed", "--allow", "github_pr_comment", "--allow", "git_sync", "--allow", "git_push", "--push-branch", "nexus/*", "--merge-method", "squash", "--merge-base", "develop"},
		{"enable-agent", "--source-id", source},
		{"inspect"},
	}
	for _, args := range commands {
		if err := runCommand(ctx, pool, args, &out, lookup); err != nil {
			t.Fatalf("documented %s command failed: %v", args[0], err)
		}
	}
	if strings.Contains(out.String(), "fake-runbook-private-") {
		t.Error("documented commands printed secret")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Error("runbook master key not private")
	}
	runbook, err := os.ReadFile("../../docs/runbooks/clickup-telegram-local.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{"settings set max_input_tokens 40000", "settings set listen_addr 127.0.0.1:8080", "--push-branch 'nexus/*'", "--app-id <id> --installation-id <id> --key-ref gh_app_key", "master-key init --path"} {
		if !strings.Contains(string(runbook), contract) {
			t.Error("tested CLI argv no longer matches runbook")
		}
	}
}

// TC-26 (REQ-12), TC-32 (REQ-15): malformed user input must not update the
// wrong object or overwrite settings/credentials; errors are safe/actionable.
func TestApproved_CLIInvalidInputLeavesStateUnchanged(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, _ := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, nil)
	base := []string{"secret", "set", "--workspace-id", ws, "--name", "safe", "--kind", "github_pat"}
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('credentials',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]') FROM credentials c),'integrations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM integrations i),'settings',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY key),'[]') FROM app_settings s))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	cases := []struct {
		args  []string
		input string
	}{
		{[]string{"secret", "set"}, "fake-private-cli-value"},
		{[]string{"secret", "set", "--workspace-id", "typo"}, "fake-private-cli-value"},
		{[]string{"secret", "set", "--workspace-id", "019a6690-abcd-7000-8000-000000000099", "--name", "safe", "--kind", "github_pat"}, "fake-private-cli-value"},
		{append(append([]string{}, base...), "--unknown"), "fake-private-cli-value"},
		{base, ""},
		{base, strings.Repeat("x", maxSecretBytes+1)},
		{[]string{"settings", "set", "unknown_setting", "1"}, ""},
		{[]string{"settings", "set", "debounce_window", "5m"}, ""},
		{[]string{"github-repo", "set", "--source-id", source, "--repo", "../escape", "--account", "personal"}, ""},
		{[]string{"enable-agent", "--source-id", "typo"}, ""},
	}
	for _, tc := range cases {
		withStdin(t, tc.input)
		var out bytes.Buffer
		err := runCommand(ctx, pool, tc.args, &out, lookup)
		if err == nil || err.Error() == "" {
			t.Errorf("invalid %s input not rejected with an error", tc.args[0])
		}
		if snapshot() != before {
			t.Errorf("rejected %s command changed state", tc.args[0])
		}
		if strings.Contains(out.String(), "fake-private-cli-value") || (err != nil && strings.Contains(err.Error(), "fake-private-cli-value")) {
			t.Error("CLI error echoed input secret")
		}
	}
	var out bytes.Buffer
	if err := runCommand(ctx, pool, base, &out, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "NEXUS_MASTER_KEY_FILE") {
		t.Error("missing master key error does not identify required setting")
	}
	if snapshot() != before {
		t.Error("missing key changed state")
	}
}

// TC-61 (REQ-F18), TC-62 (REQ-F18): compare entire durable state, not just row counts.
func TestApproved_MigrateSecretsAtomicPreflight(t *testing.T) {
	for _, mode := range []string{"unique", "shared_same_kind", "case_collision", "truncated_collision", "existing", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool := setupDB(t)
			ws, source, target := initWorkspace(t, ctx, pool)
			botEnv, webEnv := "BOT_TOKEN", "WEBHOOK_TOKEN"
			if mode == "case_collision" {
				webEnv = "bot_token"
			}
			if mode == "truncated_collision" {
				botEnv = strings.Repeat("A", 63) + "B"
				webEnv = strings.Repeat("A", 63) + "C"
			}
			if _, err := pool.Exec(ctx, `UPDATE integrations SET config=jsonb_set(config,'{bot_token_env}',to_jsonb($2::text)) WHERE id=$1`, target, botEnv); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE integrations SET config=jsonb_set(config,'{webhook_secret_env}',to_jsonb($2::text)) WHERE id=$1`, source, webEnv); err != nil {
				t.Fatal(err)
			}
			if mode == "shared_same_kind" {
				if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,workspace_id,platform,role,config) SELECT gen_random_uuid(),workspace_id,platform,role,config FROM integrations WHERE id=$1`, target); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{botEnv: "fake-bot-private-password", webEnv: "fake-webhook-private-password"}
			if mode == "missing" {
				delete(env, webEnv)
			}
			lookup := keyEnv(t, env)
			var out bytes.Buffer
			if mode == "existing" {
				withStdin(t, "fake-existing-secret")
				if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "bot_token", "--kind", "telegram_bot"}, &out, lookup); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() string {
				var s string
				if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('credentials',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]') FROM credentials c),'integrations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM integrations i))::text`).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := snapshot()
			err := runCommand(ctx, pool, []string{"migrate-secrets", "--workspace-id", ws}, &out, lookup)
			if mode == "unique" || mode == "shared_same_kind" {
				if err != nil {
					t.Fatal(err)
				}
				if countCreds(t, ctx, pool) != 2 {
					t.Error("migration did not deduplicate shared reference")
				}
				var cfg []byte
				if err := pool.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1`, target).Scan(&cfg); err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(cfg, &m); err != nil {
					t.Fatal(err)
				}
				if m["bot_token_ref"] != "bot_token" || m["bot_token_env"] != nil {
					t.Error("migration did not rewrite target")
				}
			} else {
				if err == nil {
					t.Error("unsafe migration accepted")
				}
				if snapshot() != before {
					t.Error("preflight failure changed durable state")
				}
			}
			for _, v := range env {
				if strings.Contains(out.String(), v) || (err != nil && strings.Contains(err.Error(), v)) {
					t.Error("migration leaked secret")
				}
			}
		})
	}
}

// TC-45 (REQ-F7), TC-46 (REQ-F7): inspect is metadata-only even with legacy hostile identifiers.
func TestApproved_InspectSanitizesLegacyIdentifiers(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, _ := initWorkspace(t, ctx, pool)
	var thread string
	if err := pool.QueryRow(ctx, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES(gen_random_uuid(),$1,$2,'task','inspect') RETURNING id`, ws, source).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	var job int64
	if err := pool.QueryRow(ctx, `INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at) VALUES($1,$2,$3,now()) RETURNING id`, ws, source, thread).Scan(&job); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tool_runs(workspace_id,agent_job_id,step,tool,tier,repo,idempotency_key,input_redacted,status,result_summary) VALUES($1,$2,1,'private-secret-tool-name/invalid','read','private-secret-repo/invalid/extra','inspect-hostile','{"input":"PRIVATE-INPUT"}','denied','PRIVATE-RESULT')`, ws, job); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"inspect"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"private-secret-tool-name", "private-secret-repo", "PRIVATE-INPUT", "PRIVATE-RESULT"} {
		if strings.Contains(out.String(), leak) {
			t.Error("unsafe legacy audit metadata printed")
		}
	}
	// D4: owner-approved oracle: legacy inspect uses "invalid"; only new
	// executor audit rows use "invalid_tool" (covered in storage tests).
	if !strings.Contains(out.String(), `"tool":"invalid"`) {
		t.Error("safe legacy invalid tool placeholder missing")
	}
}
