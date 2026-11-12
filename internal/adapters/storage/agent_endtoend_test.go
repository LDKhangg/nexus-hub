package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
)

// e2eLLM plays Tier-1 (Generate) and a two-step Tier-2 (GenerateWithTools).
type e2eLLM struct {
	mu     sync.Mutex
	steps  int
	report string
}

func (l *e2eLLM) Generate(context.Context, *domain.LLMRequest) (*domain.LLMResponse, error) {
	return &domain.LLMResponse{Text: `{"category":"investigate","urgency":"high","confidence":0.92,"summary":"Login returns 500","goal":"find the login handler and check CI"}`,
		Usage: domain.Usage{Input: 400, Output: 50}}, nil
}

func (l *e2eLLM) GenerateWithTools(context.Context, *domain.ToolRequest) (*domain.ToolResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps++
	if l.steps == 1 {
		return &domain.ToolResponse{Stop: domain.StopToolUse, Usage: domain.Usage{Input: 2000, Output: 120}, Calls: []domain.ToolCall{
			{ID: "c1", Name: "git_sync", Input: json.RawMessage(`{"repo":"acme/app"}`)},
			{ID: "c2", Name: "code_search", Input: json.RawMessage(`{"repo":"acme/app","query":"func Login"}`)},
			{ID: "c3", Name: "github_pr_checks", Input: json.RawMessage(`{"repo":"acme/app","number":1}`)},
		}}, nil
	}
	return &domain.ToolResponse{Stop: domain.StopEndTurn, Text: l.report, Usage: domain.Usage{Input: 2600, Output: 90}}, nil
}

func gitRun(t *testing.T, gitPath, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitPath, args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// bareRemote creates <root>/acme/app.git with one commit.
func bareRemote(t *testing.T, gitPath string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "acme", "app.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, gitPath, bare, "init", "--bare", "-b", "main")
	work := t.TempDir()
	gitRun(t, gitPath, work, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "auth.go"), []byte("package app\n\nfunc Login() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, gitPath, work, "add", "auth.go")
	gitRun(t, gitPath, work, "commit", "-m", "init")
	gitRun(t, gitPath, work, "push", bare, "main")
	return root
}

func TestAgentEndToEnd(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("requires git")
	}
	gitPath, _ = filepath.Abs(gitPath)
	ctx, pool := testDB(t)
	agentCfg := `{"enabled":true,"clickup_token_ref":"clickup","tools":{"enabled":["code_search","github_pr_checks"],
	  "github":{"accounts":{"personal":{"type":"pat","token_ref":"gh_personal","login":"LDKhangg","commit_name":"Nexus","commit_email":"n@example.invalid"}},
	  "repos":{"acme/app":{"account":"personal","managed":true,"allow":["git_sync"]}}}}}`
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES
		($1,$2,'clickup','source',jsonb_build_object('clickup_team_id','team','webhook_id','w1','webhook_secret_ref','clickup-webhook','target_integration_id',($3::uuid)::text,'agent',$4::jsonb)),
		($3::uuid,$2,'telegram','control','{"chat_id":"-10042","bot_token_ref":"telegram"}')`, integrationA, workspaceA, integrationB, agentCfg)

	// Real resolver over encrypted DB credentials.
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	creds, _ := NewCredentialStore(pool)
	redactor := agent.NewRedactor(nil)
	resolver := secrets.NewResolver(creds, key, redactor, nil, time.Minute, time.Now)
	ghToken := "ghp_" + strings.Repeat("Z", 36)
	botToken := "123456789:" + strings.Repeat("B", 35)
	for _, s := range []struct{ name, kind, value string }{{"gh_personal", "github_pat", ghToken}, {"telegram", "telegram_bot", botToken}} {
		if err := resolver.SetSecret(ctx, workspaceA, s.name, s.kind, s.value); err != nil {
			t.Fatal(err)
		}
	}

	// Fake GitHub API.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ghToken {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/user":
			io.WriteString(w, `{"login":"LDKhangg"}`)
		case "/repos/acme/app/pulls/1":
			io.WriteString(w, `{"number":1,"title":"t","state":"open","head":{"ref":"nexus/x","sha":"abcdef1234"},"base":{"ref":"develop"}}`)
		case "/repos/acme/app/commits/abcdef1234/check-runs":
			io.WriteString(w, `{"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
		case "/repos/acme/app/commits/abcdef1234/status":
			io.WriteString(w, `{"state":"success","statuses":[{"context":"ci","state":"success"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer gh.Close()
	ghCreds := githubapi.NewCredentialsAt(gh.URL, gh.Client(), resolver, redactor, nil)
	ghClient := githubapi.NewClientAt(gh.URL, gh.Client(), ghCreds)

	// Real git runner against a local bare remote.
	data := t.TempDir()
	reposDir, tmpDir := filepath.Join(data, "repos"), filepath.Join(data, "tmp")
	_ = os.MkdirAll(reposDir, 0o700)
	runner, err := execrunner.New(gitPath, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	remote := bareRemote(t, gitPath)
	all := append([]tools.Tool{}, codetools.NewReadTools(runner, reposDir)...)
	all = append(all, codetools.NewWorkspaceToolsWithLocalRemote(runner, ghCreds, reposDir, 1<<30, redactor, "file://"+remote+"/")...)
	all = append(all, githubtools.NewReadTools(ghClient)...)
	reg, err := tools.NewRegistry(all...)
	if err != nil {
		t.Fatal(err)
	}

	// 1-2. Two comments in a burst become one job.
	var n atomic.Int64
	notices, _ := NewNoticeRepository(pool, func() (string, error) { return fmt.Sprintf("019a6690-abcd-7000-8000-%012d", 3000+n.Add(1)), nil }, DebounceSettings{Window: 200 * time.Millisecond, MaxWait: time.Second})
	notices.SetKnownTools(reg.Tiers())
	for _, k := range []string{"e1", "e2"} {
		ev := noticeEvent(k, "task-9")
		ev.Content = "login returns 500 since this morning (" + k + ")"
		if _, err := notices.Accept(ctx, []domain.NormalizedEvent{ev}); err != nil {
			t.Fatal(err)
		}
	}
	if c := countRows(t, ctx, pool, "agent_jobs"); c != 1 {
		t.Fatalf("jobs %d", c)
	}

	// 3-4. Orchestrator with the real Tier-1/Tier-2 over a fake LLM.
	llm := &e2eLLM{report: "- Login handler is in auth.go\n- CI is green\n- leaked " + ghToken}
	store, _ := NewAgentStore(pool)
	store.SetKnownTools(reg.Tiers())
	runs, _ := NewToolRunStore(pool)
	orch := agent.NewOrchestrator(store, func(context.Context, string) (agent.Triager, agent.Investigator, error) {
		return agent.NewTier1Router(llm, "triage"), agent.NewTier2Actor(llm, "actor"), nil
	}, reg, runs, redactor, nil, agent.OrchestratorSettings{PollInterval: 20 * time.Millisecond, Limits: tools.DefaultLimits(), MaxInputTokens: 60000, MaxOutputTokens: 6000})

	// 6. Fake Telegram behind the action worker.
	var tgMu sync.Mutex
	var sent []string
	var tgPaths []string
	tg := telegram.NewClient(&http.Client{Transport: fakeTelegramTransport(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		tgMu.Lock()
		sent = append(sent, body.Text)
		tgPaths = append(tgPaths, req.URL.Path)
		tgMu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":9}}`)), Header: make(http.Header)}, nil
	})}, resolver)
	actions, _ := NewActionStore(pool)
	worker := relay.NewActionWorker(actions, tg, relay.WorkerSettings{PollInterval: 20 * time.Millisecond, RequestTimeout: 5 * time.Second})

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range []interface{ Run(context.Context) error }{orch, worker} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Run(runCtx) }()
	}
	for {
		tgMu.Lock()
		got := len(sent)
		tgMu.Unlock()
		if got > 0 || runCtx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a duplicate send would show up here
	cancel()
	wg.Wait()

	if len(sent) != 1 {
		var status, code string
		_ = pool.QueryRow(ctx, `SELECT status, COALESCE(last_error_code,'') FROM agent_jobs`).Scan(&status, &code)
		t.Fatalf("telegram messages %d (job %s %s)", len(sent), status, code)
	}
	text := sent[0]
	for _, want := range []string{"🔎 Nexus", "high · Login returns 500", "Login handler is in auth.go", "git_sync ✓ (write)", "code_search ✓", "github_pr_checks ✓", "Tokens: 5000/260"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(tgPaths[0], botToken) {
		t.Fatal("bot token not resolved from the DB")
	}
	// 5. Three successful audited tool runs.
	if c := scalar[int](t, ctx, pool, `SELECT count(*) FROM tool_runs WHERE status='ok'`); c != 3 {
		rows, _ := pool.Query(ctx, `SELECT tool, status, COALESCE(error_code,'') FROM tool_runs ORDER BY id`)
		var b bytes.Buffer
		for rows.Next() {
			var a, s, e string
			_ = rows.Scan(&a, &s, &e)
			fmt.Fprintf(&b, "%s %s %s\n", a, s, e)
		}
		t.Fatalf("ok tool runs %d:\n%s", c, b.String())
	}
	// 7. No secret in any persisted text column.
	for _, q := range []string{
		`SELECT string_agg(content, ' ') FROM message_logs`,
		`SELECT string_agg(COALESCE(input_redacted::text,'') || COALESCE(result_summary,'') || COALESCE(error_code,''), ' ') FROM tool_runs`,
		`SELECT string_agg(payload::text || COALESCE(last_error_code,''), ' ') FROM outbound_actions`,
		`SELECT string_agg(COALESCE(last_error_code,''), ' ') FROM agent_jobs`,
		`SELECT string_agg(rolling_summary || title || metadata::text, ' ') FROM hub_threads`,
		`SELECT string_agg(config::text, ' ') FROM integrations`,
	} {
		var s *string
		if err := pool.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s != nil && (strings.Contains(*s, ghToken) || strings.Contains(*s, botToken)) {
			t.Fatalf("secret persisted by %s", q)
		}
	}
	if strings.Contains(text, ghToken) {
		t.Fatal("secret sent to Telegram")
	}
}
