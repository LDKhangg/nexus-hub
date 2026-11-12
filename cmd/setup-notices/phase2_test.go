package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// keyEnv writes a master key and returns a lookup exposing it plus extra vars.
func keyEnv(t *testing.T, extra map[string]string) func(string) (string, bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	var out bytes.Buffer
	if err := masterKeyInit([]string{"init", "--path", path}, &out); err != nil {
		t.Fatal(err)
	}
	return func(name string) (string, bool) {
		if name == "NEXUS_MASTER_KEY_FILE" {
			return path, true
		}
		v, ok := extra[name]
		return v, ok
	}
}

func withStdin(t *testing.T, value string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(value)
	t.Cleanup(func() { stdin = old })
}

func initWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, extra ...string) (ws, source, target string) {
	t.Helper()
	var out bytes.Buffer
	args := append([]string{"init", "--workspace-name", "P", "--chat-id", "-10042", "--team-id", "team"}, extra...)
	if err := runCommand(ctx, pool, args, &out, nil); err != nil {
		t.Fatal(err)
	}
	var ids struct{ WorkspaceID, SourceID, TargetID string }
	var raw map[string]string
	_ = json.Unmarshal(out.Bytes(), &raw)
	ids.WorkspaceID, ids.SourceID, ids.TargetID = raw["workspace_id"], raw["source_id"], raw["target_id"]
	return ids.WorkspaceID, ids.SourceID, ids.TargetID
}

func TestMasterKeyInit_NoOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	var out bytes.Buffer
	if err := masterKeyInit([]string{"init", "--path", path}, &out); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	key, err := secrets.LoadMasterKey(path)
	if err != nil || len(key) != 32 {
		t.Fatalf("%v", err)
	}
	before, _ := os.ReadFile(path)
	if err := masterKeyInit([]string{"init", "--path", path}, &out); err == nil {
		t.Fatal("overwrote key")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("key changed")
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(before))) {
		t.Fatal("key printed")
	}
	if err := run([]string{"master-key", "init", "--path", "relative"}, &out, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestSecretSet_ReadsStdinNotArgs(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, nil)
	value := "123456:telegram-secret-token-value-abcdefghijklmno"
	withStdin(t, value+"\n")
	args := []string{"secret", "set", "--workspace-id", ws, "--name", "telegram", "--kind", "telegram_bot"}
	for _, a := range args {
		if strings.Contains(a, value) {
			t.Fatal("value in args")
		}
	}
	var out bytes.Buffer
	if err := runCommand(ctx, pool, args, &out, lookup); err != nil {
		t.Fatal(err)
	}
	var ct []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM credentials WHERE workspace_id=$1 AND name='telegram'`, ws).Scan(&ct); err != nil || bytes.Contains(ct, []byte(value)) || len(ct) == 0 {
		t.Fatalf("stored %v", err)
	}
	store, _ := storage.NewCredentialStore(pool)
	key, _ := secrets.LoadMasterKey(func() string { p, _ := lookup("NEXUS_MASTER_KEY_FILE"); return p }())
	if v, err := secrets.NewResolver(store, key, nil, nil, time.Minute, time.Now).Resolve(ctx, ws, "telegram"); err != nil || v != value {
		t.Fatalf("resolve %q %v", v, err)
	}
	// A positional value is refused.
	withStdin(t, "")
	if err := runCommand(ctx, pool, append(args, value), &out, lookup); err == nil {
		t.Fatal("positional secret accepted")
	}
	if err := runCommand(ctx, pool, []string{"secret", "list", "--workspace-id", ws}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), value) || !strings.Contains(out.String(), `"telegram_bot"`) {
		t.Fatalf("list output %s", out.String())
	}
}

func TestMigrateSecrets_RewritesRefs(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, target := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, map[string]string{"NEXUS_TELEGRAM_BOT_TOKEN": "bot-token-value-123456", "NEXUS_CLICKUP_WEBHOOK_SECRET": "webhook-secret-value"})
	missing := keyEnv(t, map[string]string{"NEXUS_TELEGRAM_BOT_TOKEN": "x-token-value"})
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"migrate-secrets", "--workspace-id", ws}, &out, missing); err == nil {
		t.Fatal("partial migration accepted")
	}
	if n := countCreds(t, ctx, pool); n != 0 {
		t.Fatalf("partial migration stored %d", n)
	}
	if err := runCommand(ctx, pool, []string{"migrate-secrets", "--workspace-id", ws}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	cfg := func(id string) map[string]any {
		var data []byte
		_ = pool.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1`, id).Scan(&data)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		return m
	}
	tc := cfg(target)
	if _, legacy := tc["bot_token_env"]; legacy || tc["bot_token_ref"] != "nexus_telegram_bot_token" {
		t.Fatalf("target %v", tc)
	}
	sc := cfg(source)
	if _, legacy := sc["webhook_secret_env"]; legacy || sc["webhook_secret_ref"] != "nexus_clickup_webhook_secret" {
		t.Fatalf("source %v", sc)
	}
	// Delivery now resolves from the DB with no environment at all.
	parsed, err := tenant.ParseTelegramTargetConfig(tc)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := storage.NewCredentialStore(pool)
	p, _ := lookup("NEXUS_MASTER_KEY_FILE")
	key, _ := secrets.LoadMasterKey(p)
	noEnv := secrets.NewResolver(store, key, nil, func(string) (string, bool) { return "", false }, time.Minute, time.Now)
	if v, err := noEnv.Resolve(ctx, ws, parsed.BotTokenRef); err != nil || v != "bot-token-value-123456" {
		t.Fatalf("%q %v", v, err)
	}
	if strings.Contains(out.String(), "bot-token-value") || strings.Contains(out.String(), "webhook-secret-value") {
		t.Fatal("secret printed")
	}
}

func countCreds(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSettingsSet_Validates(t *testing.T) {
	ctx, pool := setupDB(t)
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"settings", "set", "max_input_tokens", "40000"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"settings", "set", "max_input_tokens", "lots"},
		{"settings", "set", "nope", "1"},
		{"settings", "set", "tool_timeout", "0s"},
		{"settings", "set", "debounce_window", "5m"}, // above the 30s max wait
	} {
		if err := runCommand(ctx, pool, bad, &out, nil); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	out.Reset()
	if err := runCommand(ctx, pool, []string{"settings", "get", "max_input_tokens"}, &out, nil); err != nil || !strings.Contains(out.String(), `"value":40000`) || !strings.Contains(out.String(), `"source":"db"`) {
		t.Fatalf("%s %v", out.String(), err)
	}
	out.Reset()
	if err := runCommand(ctx, pool, []string{"settings", "get", "actor_model"}, &out, nil); err != nil || !strings.Contains(out.String(), `"source":"default"`) {
		t.Fatalf("%s %v", out.String(), err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM app_settings`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestGitHubRepoSet_WritesConfig(t *testing.T) {
	ctx, pool := setupDB(t)
	_, source, _ := initWorkspace(t, ctx, pool)
	var out bytes.Buffer
	acct := []string{"github-account", "add", "--source-id", source, "--name", "personal", "--pat-ref", "gh_personal", "--login", "LDKhangg", "--commit-name", "Nexus Bot", "--commit-email", "nexus@example.invalid"}
	if err := runCommand(ctx, pool, acct, &out, nil); err != nil {
		t.Fatal(err)
	}
	repo := []string{"github-repo", "set", "--source-id", source, "--repo", "LDKhangg/nexus-hub", "--account", "personal", "--managed",
		"--allow", "github_pr_comment", "--allow", "git_push", "--push-branch", "nexus/*", "--merge-method", "squash", "--merge-base", "develop"}
	if err := runCommand(ctx, pool, repo, &out, nil); err != nil {
		t.Fatal(err)
	}
	if err := runCommand(ctx, pool, []string{"enable-agent", "--source-id", source}, &out, nil); err != nil {
		t.Fatal(err)
	}
	var data []byte
	_ = pool.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1`, source).Scan(&data)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	cfg, err := tenant.ParseAgentConfig(m, knownTools())
	if err != nil || !cfg.Enabled {
		t.Fatalf("%+v %v", cfg, err)
	}
	g := cfg.Tools.Repos["LDKhangg/nexus-hub"]
	if g.Account != "personal" || !g.Managed || len(g.Allow) != 2 || g.MergeBaseBranches[0] != "develop" || !g.RequireChecksGreen || cfg.Tools.Accounts["personal"].Login != "LDKhangg" {
		t.Fatalf("%+v", g)
	}
	// Invalid edits are rejected and leave the config unchanged.
	bad := []string{"github-repo", "set", "--source-id", source, "--repo", "LDKhangg/nexus-hub", "--account", "missing"}
	if err := runCommand(ctx, pool, bad, &out, nil); err == nil {
		t.Fatal("unknown account accepted")
	}
	var after []byte
	_ = pool.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1`, source).Scan(&after)
	if !bytes.Equal(data, after) {
		t.Fatal("rejected edit changed config")
	}
}

func TestGitHubCheck_ReportsMismatchWithoutToken(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, _ := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, nil)
	token := "ghp_" + strings.Repeat("q", 36)
	withStdin(t, token)
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "gh_personal", "--kind", "github_pat"}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	acct := []string{"github-account", "add", "--source-id", source, "--name", "personal", "--pat-ref", "gh_personal", "--login", "LDKhangg", "--commit-name", "N", "--commit-email", "n@example.invalid"}
	if err := runCommand(ctx, pool, acct, &out, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"someone-else"}`))
	}))
	defer srv.Close()
	oldBase, oldHTTP := githubAPIBase, githubHTTP
	githubAPIBase, githubHTTP = srv.URL, func() *http.Client { return srv.Client() }
	t.Cleanup(func() { githubAPIBase, githubHTTP = oldBase, oldHTTP })
	out.Reset()
	if err := runCommand(ctx, pool, []string{"github-check", "--source-id", source}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"account_mismatch"`) || !strings.Contains(out.String(), `"ok":false`) || strings.Contains(out.String(), token) {
		t.Fatalf("%s", out.String())
	}
}

func TestInspect_NoLeak(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, source, target := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, nil)
	secret := "planted-credential-value-xyz"
	withStdin(t, secret)
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "anthropic", "--kind", "anthropic_key"}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	thread := "019a6690-abcd-7000-8000-0000000000aa"
	for _, q := range []string{
		`INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES('` + thread + `','` + ws + `','` + source + `','task','t1')`,
		`INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at,input_tokens) VALUES('` + ws + `','` + source + `','` + thread + `',now(),321)`,
		`INSERT INTO tool_runs(workspace_id,agent_job_id,step,tool,tier,idempotency_key,input_redacted,status,result_summary) SELECT '` + ws + `',id,1,'code_search','read','k','"PLANTED-INPUT"','ok','PLANTED-OUTPUT' FROM agent_jobs`,
		`INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key) VALUES('` + ws + `','` + target + `','telegram_message','{"text":"PLANTED-REPORT"}','agent_job:1')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if err := runCommand(ctx, pool, []string{"inspect"}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, leak := range []string{secret, "PLANTED-INPUT", "PLANTED-OUTPUT", "PLANTED-REPORT", "ciphertext", "nonce"} {
		if strings.Contains(s, leak) {
			t.Fatalf("inspect leaked %q:\n%s", leak, s)
		}
	}
	for _, want := range []string{`"table":"agent_jobs"`, `"input_tokens":321`, `"table":"tool_runs"`, `"tool":"code_search"`, `"table":"outbound_actions"`, `"table":"credentials"`, `"name":"anthropic"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}
