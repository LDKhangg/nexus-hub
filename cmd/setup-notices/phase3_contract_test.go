package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

type c23Transport func(*http.Request) (*http.Response, error)

func (f c23Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// P3-CLI-04 (REQ-7): actual CLI, encryption and Telegram client with an in-process
// transport: registration persists the generated secret but never prints it or the bot token.
func TestC23WebhookRegistrationEncryptedSecretAndSafeStatus(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, target := initWorkspace(t, ctx, pool, "--bot-token-ref", "telegram")
	lookup := keyEnv(t, nil)
	withStdin(t, "123456:obviously-fake-telegram-token-abcdefghijklmno\n")
	var out bytes.Buffer
	if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "telegram", "--kind", "telegram_bot"}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_settings(key,value) VALUES('public_base_url','"https://hub.example.com"')`); err != nil {
		t.Fatal(err)
	}
	var generated, url string
	requests := 0
	old := telegramHTTP
	t.Cleanup(func() { telegramHTTP = old })
	telegramHTTP = func() *http.Client {
		return &http.Client{Transport: c23Transport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.telegram.org" {
				t.Fatal("unexpected host in fake transport")
			}
			requests++
			var response string
			switch {
			case strings.HasSuffix(r.URL.Path, "/setWebhook"):
				var in struct {
					URL     string   `json:"url"`
					Secret  string   `json:"secret_token"`
					Allowed []string `json:"allowed_updates"`
				}
				if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Secret) < 32 || len(in.Allowed) != 2 {
					t.Fatal("invalid registration payload")
				}
				generated, url = in.Secret, in.URL
				response = `{"ok":true,"result":true}`
			case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
				raw, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"url": url, "pending_update_count": 0, "allowed_updates": []string{"message", "callback_query"}}})
				response = string(raw)
			default:
				t.Fatal("unexpected Telegram fake method")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
		})}
	}
	for _, action := range []string{"set", "status"} {
		out.Reset()
		if err := telegramCommand(ctx, pool, []string{"webhook", action, "--workspace-id", ws, "--integration-id", target}, &out, lookup); err != nil {
			t.Fatal(err)
		}
		if generated == "" || strings.Contains(out.String(), generated) || strings.Contains(out.String(), "obviously-fake-telegram-token") {
			t.Fatal("CLI printed generated webhook secret or bot token")
		}
		if !strings.Contains(out.String(), "https://hub.example.com/webhook/telegram/"+target) {
			t.Fatal("CLI registered wrong public endpoint")
		}
		if action == "status" && (!strings.Contains(out.String(), `"secret_stored":true`) || !strings.Contains(out.String(), `"url_matches":true`)) {
			t.Fatal("status missing secret/url confirmation")
		}
	}
	if requests != 2 {
		t.Fatalf("unexpected Telegram call count=%d", requests)
	}
	var cipher []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM credentials WHERE workspace_id=$1 AND name=$2`, ws, behavior.CredentialTelegramWebhookSecret).Scan(&cipher); err != nil || len(cipher) == 0 || bytes.Contains(cipher, []byte(generated)) {
		t.Fatal("generated secret not encrypted")
	}
	resolver, _, err := openResolver(pool, lookup)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := resolver.Resolve(ctx, ws, behavior.CredentialTelegramWebhookSecret)
	if err != nil || plain != generated {
		t.Fatal("registered secret and encrypted credential differ")
	}
}

func c23Source(t *testing.T, source []byte, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// P3-CLI-01 (REQ-1, REQ-2, REQ-3, REQ-20, REQ-24): literal owner CLI workflow,
// byte-for-byte version exports, vars file, enable/disable and invalid draft publish rejection.
func TestC23BehaviorExampleLifecycleAndVars(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	source, err := os.ReadFile("../../examples/behaviors/spec-review.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := c23Source(t, source, "review.yaml")
	vars := c23Source(t, []byte("review_status: kiểm tra\noq_heading: Câu hỏi\n"), "values.yaml")
	call := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := behaviorCommand(ctx, pool, append(args, "--workspace-id", ws), &out)
		return out.String(), err
	}
	for _, args := range [][]string{{"import", path, "--vars", vars}, {"validate", "spec-review"}, {"publish", "spec-review"}, {"enable", "spec-review"}} {
		if _, err := call(args...); err != nil {
			t.Fatalf("%s failed: %v", args[0], err)
		}
	}
	if got, err := call("export", "spec-review", "--version", "1"); err != nil || got != string(source) {
		t.Fatalf("published source not exact: %v", err)
	}
	s, _ := storage.NewBehaviorStore(pool)
	v, err := s.GetVersion(ctx, ws, "spec-review", 1)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := behavior.ResolveVersion(v)
	if err != nil || !strings.Contains(doc.Instructions, "Câu hỏi") || strings.Contains(doc.Instructions, "{{vars.") {
		t.Fatalf("vars not substituted: %v", err)
	}
	if got := doc.Triggers[0].ToStatus; len(got) != 1 || got[0] != "kiểm tra" {
		t.Fatal("trigger vars not resolved")
	}
	changed := bytes.Replace(source, []byte("confidence_threshold: 0.8"), []byte("confidence_threshold: 0.95"), 1)
	if _, err := call("import", c23Source(t, changed, "review.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := call("publish", "spec-review"); err != nil {
		t.Fatal(err)
	}
	if got, err := call("export", "spec-review", "--version", "1"); err != nil || got != string(source) {
		t.Fatal("v2 changed v1 export")
	}
	v2, err := s.GetVersion(ctx, ws, "spec-review", 2)
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := behavior.ResolveVersion(v2)
	if err != nil || !strings.Contains(doc2.Instructions, "Câu hỏi") {
		t.Fatal("reimport without vars discarded stored settings")
	}
	if _, err := call("disable", "spec-review"); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.EnabledVersions(ctx, ws); err != nil || len(enabled) != 0 {
		t.Fatal("disabled behavior still triggerable")
	}
	if out, err := call("list"); err != nil || !strings.Contains(out, `"disabled"`) || !strings.Contains(out, `"latest_version":2`) {
		t.Fatal("list does not show lifecycle/version")
	}
	invalid := bytes.Replace(source, []byte("confidence_threshold: 0.8"), []byte("confidence_threshold: 9"), 1)
	invalid = bytes.Replace(invalid, []byte("ask_expiry_hours: 72"), []byte("ask_expiry_hours: -1"), 1)
	if out, err := call("import", c23Source(t, invalid, "invalid.yaml")); err != nil || !strings.Contains(out, `"valid":false`) {
		t.Fatal("invalid draft was not saved with diagnostics")
	}
	if _, err := call("validate", "spec-review"); err == nil || !strings.Contains(err.Error(), "confidence_threshold") || !strings.Contains(err.Error(), "ask_expiry_hours") {
		t.Fatal("validate did not report every schema path")
	}
	if _, err := call("publish", "spec-review"); err == nil {
		t.Fatal("published invalid draft")
	}
	if versions, err := s.ListVersions(ctx, ws, "spec-review"); err != nil || len(versions) != 2 {
		t.Fatal("invalid draft created version")
	}
	unknown := append(append([]byte{}, source...), []byte("\nunknown_owner_field: true\n")...)
	if _, err := call("import", c23Source(t, unknown, "unknown.yaml")); err == nil || !strings.Contains(err.Error(), "unknown_owner_field") {
		t.Fatal("unknown field not rejected with path")
	}
}

// P3-CLI-02 (REQ-18, REQ-19, REQ-25): trace by correlation, subject and item,
// feedback per version and no secret/context snapshot in machine or human output.
func TestC23TraceRedactionCorrelationFeedbackAndStats(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	s, _ := storage.NewBehaviorStore(pool)
	if _, err := s.SaveDraft(ctx, ws, "trace-case", json.RawMessage(`{"schema":1}`), nil, "schema: 1\n"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Publish(ctx, ws, "trace-case", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.CreateRun(ctx, ws, behavior.NewRun{BehaviorID: v.BehaviorID, VersionID: v.ID, Version: 1, TriggerKind: behavior.TriggerTelegramCommand, Subject: behavior.TaskSubject("abc"), Revision: "telegram:1"})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately stored hostile provider text, not a real credential.
	fakeSecret := "ghp_" + strings.Repeat("A", 36)
	it, _, err := s.AddItem(ctx, ws, behavior.NewItem{RunID: r.ID, Key: "oq-1", Text: "Vietnamese 🧪 " + fakeSecret, Proposal: fakeSecret, Citations: []behavior.Citation{{Locator: "clickup:task/abc#desc:L1", Quote: fakeSecret}}, Class: behavior.ClassAsk, Rule: "always_ask_topic", Confidence: .99, Status: behavior.ItemAsked})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RateItem(ctx, ws, it.ID, behavior.RatingDown, "bad "+fakeSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE behavior_runs SET context_snapshot='{"private_context":"DO_NOT_EXPORT_CONTEXT"}' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{r.CorrelationID, "--json"}, {"abc", "--item", "oq-1"}, {fmt.Sprint(r.ID), "--json", "--item", "oq-1"}} {
		var out bytes.Buffer
		if err := traceCommand(ctx, pool, append([]string{"--workspace-id", ws}, args...), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), fakeSecret) || strings.Contains(out.String(), "DO_NOT_EXPORT_CONTEXT") {
			t.Fatal("trace leaked credential or context")
		}
		if !strings.Contains(out.String(), r.CorrelationID) || !strings.Contains(out.String(), "oq-1") || !strings.Contains(out.String(), "always_ask_topic") {
			t.Fatal("trace lost correlation, item or class rule")
		}
	}
	var out bytes.Buffer
	if err := behaviorCommand(ctx, pool, []string{"stats", "trace-case", "--workspace-id", ws, "--version", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Version  int
		Items    map[string]int
		Feedback map[string]int
	}
	if json.Unmarshal(out.Bytes(), &stats) != nil || stats.Version != 1 || stats.Items["ask"] != 1 || stats.Feedback["down"] != 1 {
		t.Fatal("CLI stats missing per-version rating/class")
	}
	if _, err := s.Publish(ctx, ws, "trace-case", nil); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := behaviorCommand(ctx, pool, []string{"stats", "trace-case", "--workspace-id", ws, "--version", "2"}, &out); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(out.Bytes(), &stats) != nil || stats.Version != 2 || stats.Items["ask"] != 0 || stats.Feedback["down"] != 0 {
		t.Fatal("version 1 feedback leaked into version 2")
	}
}

// P3-CLI-03 (REQ-22, REQ-23): partial profile updates survive and forgetting is scoped and auditable.
func TestC23ProfileAndMemoryManagement(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	var out bytes.Buffer
	for _, args := range [][]string{{"set", "--language", "vi", "--tone", "ngắn gọn 🧪", "--quiet-hours", "22:00-07:00", "--digest", "hourly", "--model", "fake-model", "--max-tokens", "5000"}, {"set", "--tone", "thân thiện"}, {"show"}} {
		out.Reset()
		if err := profileCommand(ctx, pool, append(args, "--workspace-id", ws), &out); err != nil {
			t.Fatal(err)
		}
	}
	var profile map[string]any
	if json.Unmarshal(out.Bytes(), &profile) != nil || profile["tone"] != "thân thiện" || profile["quiet_hours"] != "22:00-07:00" || profile["digest"] != "hourly" || profile["max_tokens"] != float64(5000) {
		t.Fatal("partial profile set discarded fields")
	}
	if err := profileCommand(ctx, pool, []string{"set", "--workspace-id", ws, "--quiet-hours", "25:00-07:00"}, &out); err == nil {
		t.Fatal("invalid quiet hours accepted")
	}
	s, _ := storage.NewBehaviorStore(pool)
	d, err := s.AddDecision(ctx, ws, behavior.NewDecision{Topic: "pagination", Question: "cursor or offset?", Answer: "cursor 🧪", Scope: behavior.DecisionScope{Behavior: "review", List: "one", Repo: "acme/app"}, Source: behavior.SourceAskAnswer})
	if err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) (string, error) {
		out.Reset()
		err := memoryCommand(ctx, pool, append(args, "--workspace-id", ws), &out)
		return out.String(), err
	}
	if got, err := call("list", "--behavior", "review"); err != nil || !strings.Contains(got, d.Locator()) || !strings.Contains(got, "cursor") {
		t.Fatal("memory list lost decision/locator")
	}
	if got, err := call("list", "--behavior", "other"); err != nil || got != "" {
		t.Fatal("memory list ignored behavior scope")
	}
	if _, err := call("forget", "--id", fmt.Sprint(d.ID)); err != nil {
		t.Fatal(err)
	}
	if got, err := call("list"); err != nil || got != "" {
		t.Fatal("forgotten memory visible in default list")
	}
	if got, err := call("show", "--id", fmt.Sprint(d.ID)); err != nil || !strings.Contains(got, `"forgotten":true`) || !strings.Contains(got, "cursor") {
		t.Fatal("forget erased audit")
	}
	if _, err := call("forget", "--id", fmt.Sprint(d.ID)); err == nil {
		t.Fatal("repeat forget accepted")
	}
}
