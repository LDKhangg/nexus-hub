package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Only the provider boundary is fake: parsing, routing, tool execution, durable
// job/outbox transitions and Telegram delivery are production components.
type approvedJourneyLLM struct {
	mu              sync.Mutex
	mode            string
	triages, actors int
	requests        []string
	secret          string
}

func (l *approvedJourneyLLM) Generate(_ context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(req)
	l.requests = append(l.requests, string(b))
	l.triages++
	if l.mode == "retry" && l.triages == 1 {
		return nil, errors.New("fake provider unavailable " + l.secret)
	}
	if l.mode == "fallback" {
		return nil, domain.ErrLLMRefused
	}
	category := "investigate"
	if l.mode == "ignore" || l.mode == "notify" {
		category = l.mode
	}
	b, _ = json.Marshal(map[string]any{"category": category, "urgency": "high", "confidence": .85, "summary": "Lỗi đăng nhập 🧠 @alice " + l.secret, "goal": "Tìm lỗi 🔎 " + l.secret})
	return &domain.LLMResponse{Text: string(b), Usage: domain.Usage{Input: 10, Output: 2, CacheRead: 3, CacheWrite: 4}}, nil
}

func (l *approvedJourneyLLM) GenerateWithTools(_ context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(req)
	l.requests = append(l.requests, string(b))
	l.actors++
	if l.actors == 1 {
		input, _ := json.Marshal(map[string]string{"repo": "o/r", "query": "Tìm lỗi " + l.secret})
		return &domain.ToolResponse{Stop: domain.StopToolUse, Text: "Đang đọc " + l.secret, Usage: domain.Usage{Input: 20, Output: 3}, Calls: []domain.ToolCall{
			{ID: "read", Name: "approved_read", Input: input},
			{ID: "write", Name: "approved_write", Input: input},
			{ID: "error", Name: "approved_error", Input: input},
			{ID: "denied", Name: "invented_tool", Input: json.RawMessage(`{}`)},
		}}, nil
	}
	return &domain.ToolResponse{Stop: domain.StopEndTurn, Text: "- Đã tìm nguyên nhân 🔎 @alice " + l.secret, Usage: domain.Usage{Input: 30, Output: 4}}, nil
}

// TC-7 (REQ-3), TC-9 (REQ-4), TC-21/22 (REQ-10), TC-23/24 (REQ-11),
// TC-41/42 (REQ-F5): wrong routing, missing delivery retry, re-investigation,
// leaked derived text or dry-run writes each breaks a separate assertion.
func TestApproved_DurableVietnameseRoutesAndDeliveryRecovery(t *testing.T) {
	for _, mode := range []string{"ignore", "notify", "investigate", "dry_run", "retry", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			oldLog := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(oldLog)
			cfg := `{"enabled":true,"persona":"Kỹ sư Việt Nam","tools":{"enabled":["approved_read","approved_write","approved_error"],"dry_run":DRY,"github":{"accounts":{"p":{"type":"pat","token_ref":"gh","login":"alice","commit_name":"Nexus","commit_email":"n@example.invalid"}},"repos":{"o/r":{"account":"p","managed":true,"allow":["approved_write"]}}}}}`
			e := newAgentEnv(t, strings.Replace(cfg, "DRY", map[bool]string{true: "true", false: "false"}[mode == "dry_run"], 1))
			red := agent.NewRedactor(nil)
			cs, _ := NewCredentialStore(e.pool)
			resolver := secrets.NewResolver(cs, []byte(strings.Repeat("K", 32)), red, nil, time.Minute, time.Now)
			values := []string{}
			for _, kind := range []string{"telegram_bot", "clickup_token", "clickup_webhook", "github_pat", "github_app_key", "anthropic_key"} {
				name := kind
				if kind == "telegram_bot" {
					name = "telegram"
				}
				value := "fake-private-system-" + kind
				if kind == "telegram_bot" {
					value = "123456789:" + strings.Repeat("B", 35)
				}
				values = append(values, value)
				if err := resolver.SetSecret(e.ctx, workspaceA, name, kind, value); err != nil {
					t.Fatal("cannot seed encrypted credential")
				}
				if _, err := resolver.Resolve(e.ctx, workspaceA, name); err != nil {
					t.Fatal("cannot register credential redaction")
				}
			}
			pasted := "ghp_" + strings.Repeat("P", 36)
			original := "Nhờ kiểm tra lỗi đăng nhập 🧠 @alice " + strings.Join(values, " ") + " " + pasted
			messageID := e.message(t, "original", "external_user", original)
			e.job(t, time.Now())
			read := &approvedEffectTool{name: "approved_read", tier: tenant.TierRead}
			write := &approvedEffectTool{name: "approved_write", tier: tenant.TierWrite}
			bad := &approvedEffectTool{name: "approved_error", tier: tenant.TierRead, fail: &tools.ToolError{Code: "not_found"}}
			reg, err := tools.NewRegistry(read, write, bad)
			if err != nil {
				t.Fatal(err)
			}
			e.store.SetKnownTools(reg.Tiers())
			runs, _ := NewToolRunStore(e.pool)
			llm := &approvedJourneyLLM{mode: mode, secret: values[3]}
			orch := agent.NewOrchestrator(e.store, func(context.Context, string) (agent.Triager, agent.Investigator, error) {
				return agent.NewTier1Router(llm, "triage"), agent.NewTier2Actor(llm, "actor"), nil
			}, reg, runs, red, nil, agent.OrchestratorSettings{PollInterval: 5 * time.Millisecond, Limits: tools.DefaultLimits(), MaxInputTokens: 60000, MaxOutputTokens: 6000})
			var mu sync.Mutex
			var texts []string
			sender := telegram.NewClient(&http.Client{Transport: fakeTelegramTransport(func(req *http.Request) (*http.Response, error) {
				var body struct {
					Text string `json:"text"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				mu.Lock()
				texts = append(texts, body.Text)
				n := len(texts)
				mu.Unlock()
				status, response := 200, `{"ok":true,"result":{"message_id":77}}`
				if mode == "retry" && n == 1 {
					status = 429
					response = `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
			})}, resolver)
			worker := relay.NewActionWorker(e.actions, sender, relay.WorkerSettings{PollInterval: 5 * time.Millisecond, RequestTimeout: time.Second})
			runCtx, cancel := context.WithTimeout(e.ctx, 12*time.Second)
			done := make(chan error, 2)
			go func() { done <- orch.Run(runCtx) }()
			go func() { done <- worker.Run(runCtx) }()
			// Always await both DB users, including assertion failures.
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-done
					<-done
				}
			}()
			retried := false
			for runCtx.Err() == nil {
				if mode == "retry" && !retried {
					var n int
					if err := e.pool.QueryRow(runCtx, `SELECT count(*) FROM agent_jobs WHERE last_error_code='llm_error' AND status='processing'`).Scan(&n); err != nil {
						t.Fatal("cannot inspect retry state")
					}
					if n == 1 {
						mustExec(t, runCtx, e.pool, `UPDATE agent_jobs SET locked_until=now() WHERE last_error_code='llm_error' AND claim_token IS NULL`)
						retried = true
					}
				}
				var terminal int
				if err := e.pool.QueryRow(runCtx, `SELECT count(*) FROM agent_jobs WHERE status IN ('done','failed')`).Scan(&terminal); err != nil {
					t.Fatal("cannot inspect job state")
				}
				if terminal == 1 {
					if mode == "ignore" {
						break
					}
					var sent int
					if err := e.pool.QueryRow(runCtx, `SELECT count(*) FROM outbound_actions WHERE status='sent'`).Scan(&sent); err != nil {
						t.Fatal("cannot inspect delivery state")
					}
					if sent == 1 {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if runCtx.Err() != nil {
				t.Fatal("durable journey did not finish before deadline")
			}
			cancel()
			<-done
			<-done
			joined = true
			llm.mu.Lock()
			triages, actors, requests := llm.triages, llm.actors, append([]string(nil), llm.requests...)
			llm.mu.Unlock()
			mu.Lock()
			delivered := append([]string(nil), texts...)
			mu.Unlock()
			wantT, wantA, wantS := 1, 2, 1
			if mode == "ignore" {
				wantA = 0
				wantS = 0
			}
			if mode == "notify" || mode == "fallback" {
				wantA = 0
			}
			if mode == "retry" {
				wantT = 2
				wantS = 2
			}
			if triages != wantT || actors != wantA || len(delivered) != wantS {
				t.Errorf("provider/delivery calls=%d/%d/%d want=%d/%d/%d", triages, actors, len(delivered), wantT, wantA, wantS)
			}
			if mode == "retry" && (!retried || delivered[0] != delivered[1]) {
				t.Error("delivery retry changed report or provider retry was not observed")
			}
			if wantA == 2 {
				writes := int32(1)
				mark := "✓"
				if mode == "dry_run" {
					writes = 0
					mark = "🧪"
				}
				if read.effects.Load() != 1 || write.effects.Load() != writes || bad.effects.Load() != 1 {
					t.Error("wrong tool effects, or delivery retry re-investigated")
				}
				for _, want := range []string{"🔎 Nexus", "Lỗi đăng nhập 🧠 @alice", "Đã tìm nguyên nhân 🔎 @alice", "approved_read ✓", "approved_write " + mark + " (write)", "approved_error ✗", "invalid_tool ⛔", "Tokens: 67/9"} {
					if len(delivered) == 0 || !strings.Contains(delivered[0], want) {
						t.Errorf("report missing %q", want)
					}
				}
				if countRows(t, e.ctx, e.pool, "tool_runs") != 4 {
					t.Error("tool audit count changed")
				}
			} else if read.effects.Load()+write.effects.Load()+bad.effects.Load() != 0 {
				t.Error("non-investigate route executed tools")
			}
			if mode == "notify" && !strings.HasPrefix(delivered[0], "🧠 Nexus") {
				t.Error("notify has wrong user-visible route")
			}
			if mode == "fallback" && (!strings.Contains(delivered[0], "⚠️ AI triage unavailable (llm_refused)") || !strings.Contains(delivered[0], "Nhờ kiểm tra lỗi đăng nhập 🧠 @alice")) {
				t.Error("fallback lost original business text")
			}
			// Raw original is allowed and must remain exact; derived text is not.
			var stored string
			if err := e.pool.QueryRow(e.ctx, `SELECT content FROM message_logs WHERE id=$1`, messageID).Scan(&stored); err != nil || stored != original {
				t.Error("raw original comment was altered")
			}
			for _, q := range []string{
				`SELECT coalesce(string_agg(content,' '),'') FROM message_logs WHERE sender_role NOT IN ('external_user','human_owner')`,
				`SELECT coalesce(string_agg(to_jsonb(t)::text,' '),'') FROM tool_runs t`,
				`SELECT coalesce(string_agg(to_jsonb(a)::text,' '),'') FROM outbound_actions a`,
				`SELECT coalesce(string_agg(to_jsonb(j)::text,' '),'') FROM agent_jobs j`,
				`SELECT coalesce(string_agg(rolling_summary,' '),'') FROM hub_threads`,
				`SELECT coalesce(string_agg(config::text,' '),'') FROM integrations`,
			} {
				var s string
				if err := e.pool.QueryRow(e.ctx, q).Scan(&s); err != nil {
					t.Fatal("sink scan failed")
				}
				requests = append(requests, s)
			}
			requests = append(requests, delivered...)
			requests = append(requests, logs.String())
			for _, sink := range requests {
				for _, secret := range append(values, pasted) {
					if strings.Contains(sink, secret) {
						t.Error("known secret reached a derived sink")
					}
				}
			}
			var encrypted int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM credentials WHERE octet_length(nonce)=12 AND octet_length(ciphertext)>16`).Scan(&encrypted); err != nil || encrypted != 6 {
				t.Error("system credentials were not encrypted")
			}
		})
	}
}

// TC-20 (REQ-9): an unavailable or wrong master key, or a missing selected
// PAT, never uses another valid account. The real encrypted DB resolver feeds
// the real credential/client path; no API call is allowed on these failures.
func TestApproved_BrokenSelectedCredentialNeverUsesBackupAccount(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'credential owner')`, workspaceA)
	store, _ := NewCredentialStore(pool)
	red := agent.NewRedactor(nil)
	key := []byte(strings.Repeat("A", 32))
	seed := secrets.NewResolver(store, key, red, nil, time.Minute, time.Now)
	for _, name := range []string{"selected", "backup"} {
		if err := seed.SetSecret(ctx, workspaceA, name, "github_pat", "fake-private-"+name+"-pat"); err != nil {
			t.Fatal("cannot seed PAT")
		}
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, `{"login":"alice"}`) }))
	defer srv.Close()
	for _, mode := range []string{"wrong_key", "missing_key", "missing_credential"} {
		t.Run(mode, func(t *testing.T) {
			k, ref, want := []byte(strings.Repeat("B", 32)), "selected", "decrypt_failed"
			if mode == "missing_key" {
				k = nil
				want = "master_key_unavailable"
			}
			if mode == "missing_credential" {
				k = key
				ref = "absent"
				want = "missing_credentials"
			}
			res := secrets.NewResolver(store, k, red, nil, time.Minute, time.Now)
			creds := githubapi.NewCredentialsAt(srv.URL, srv.Client(), res, red, time.Now)
			client := githubapi.NewClientAt(srv.URL, srv.Client(), creds)
			err := client.Do(ctx, workspaceA, tenant.GitHubAccount{Name: "personal", Type: "pat", TokenRef: ref, Login: "alice"}, "o/r", nil, "GET", "/repos/o/r", nil, nil)
			var ge *githubapi.GitHubError
			if !errors.As(err, &ge) || ge.Code != want {
				t.Errorf("safe error=%v want=%s", err, want)
			}
			if calls != 0 {
				t.Error("credential failure reached API or used backup")
			}
			if err != nil && strings.Contains(err.Error(), "fake-private-") {
				t.Error("credential error leaked plaintext")
			}
		})
	}
}
