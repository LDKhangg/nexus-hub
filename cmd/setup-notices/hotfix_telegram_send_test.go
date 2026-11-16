package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Hotfix H1 (H1-4): `telegram send`. Telegram is a local fake; the bot token
// and message text are fake values the tests look for in every output.

const (
	hfToken = "fake-bot-token-123456"
	hfChat  = "-1001234567890"
)

type hfCall struct {
	method string
	body   map[string]any
}

type hfTelegram struct {
	mu      sync.Mutex
	calls   []hfCall
	nextMsg int64
	thread  int64
	// reply overrides: method -> (status, body)
	status map[string]int
	body   map[string]string
}

func (f *hfTelegram) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	idx := strings.LastIndex(r.URL.Path, "/")
	method := r.URL.Path[idx+1:]
	if !strings.Contains(r.URL.Path, "/bot"+hfToken+"/") {
		http.Error(w, "bad token", 401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hfCall{method, body})
	if st := f.status[method]; st != 0 {
		w.WriteHeader(st)
		w.Write([]byte(f.body[method]))
		return
	}
	switch method {
	case "createForumTopic":
		f.thread++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_thread_id": 700 + f.thread}})
	default:
		f.nextMsg++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 5000 + f.nextMsg}})
	}
}

func (f *hfTelegram) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func (f *hfTelegram) last(t *testing.T, method string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i].body
		}
	}
	t.Fatalf("no %s call", method)
	return nil
}

type hfRewrite struct{ to *url.URL }

func (h hfRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = h.to.Scheme, h.to.Host
	return http.DefaultTransport.RoundTrip(r2)
}

type hfEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	ws     string
	lookup func(string) (string, bool)
	tg     *hfTelegram
	logs   *bytes.Buffer
}

// newHFEnv builds a workspace with a Telegram control integration (bot token
// stored encrypted, owner group set when withGroup) and a fake Telegram.
func newHFEnv(t *testing.T, withGroup bool) *hfEnv {
	t.Helper()
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool, "--bot-token-ref", "telegram")
	path := filepath.Join(t.TempDir(), "master.key")
	var out bytes.Buffer
	if err := run([]string{"master-key", "init", "--path", path}, &out, nil); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) { return path, name == "NEXUS_MASTER_KEY_FILE" }
	withStdin(t, hfToken)
	if err := runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "telegram", "--kind", "telegram_bot"}, &out, lookup); err != nil {
		t.Fatal(err)
	}
	if withGroup {
		var tgID string
		if err := pool.QueryRow(ctx, `SELECT id FROM integrations WHERE workspace_id=$1 AND platform='telegram'`, ws).Scan(&tgID); err != nil {
			t.Fatal(err)
		}
		if err := runCommand(ctx, pool, []string{"telegram", "group", "set", "--workspace-id", ws, "--integration-id", tgID, "--chat-id", hfChat}, &out, lookup); err != nil {
			t.Fatal(err)
		}
	}
	tg := &hfTelegram{status: map[string]int{}, body: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(tg.serve))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	old := telegramHTTP
	telegramHTTP = func() *http.Client { return &http.Client{Transport: hfRewrite{u}} }
	t.Cleanup(func() { telegramHTTP = old })
	logs := &bytes.Buffer{}
	oldW := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(oldW) })
	return &hfEnv{t: t, ctx: ctx, pool: pool, ws: ws, lookup: lookup, tg: tg, logs: logs}
}

// send runs `telegram send` with text on stdin and returns stdout and error.
func (e *hfEnv) send(text string, args ...string) (string, error) {
	e.t.Helper()
	withStdin(e.t, text)
	var out bytes.Buffer
	full := append([]string{"telegram", "send", "--workspace-id", e.ws}, args...)
	err := runCommand(e.ctx, e.pool, full, &out, e.lookup)
	return out.String(), err
}

func (e *hfEnv) noLeak(out string, err error, secrets ...string) {
	e.t.Helper()
	all := out + e.logs.String()
	if err != nil {
		all += err.Error()
	}
	for _, s := range append(secrets, hfToken) {
		if s != "" && strings.Contains(all, s) {
			e.t.Errorf("%q leaked into output, error or log: %q", s, all)
		}
	}
}

// TC-H1-20 (H1-4): a message goes to General of the owner chat; stdout is
// the status JSON; text and token are in no output or log.
func TestHotfixH1_TelegramSendGeneral(t *testing.T) {
	e := newHFEnv(t, true)
	const text = "Báo cáo ngày 🚀 PRIVATE-REPORT-BODY"
	out, err := e.send(text + "\n\n")
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if json.Unmarshal([]byte(out), &res) != nil || res["status"] != "sent" || res["message_id"] != float64(5001) || res["topic"] != nil {
		t.Fatalf("out=%q", out)
	}
	if got := e.tg.methods(); len(got) != 1 || got[0] != "sendMessage" {
		t.Fatalf("calls=%v", got)
	}
	body := e.tg.last(t, "sendMessage")
	if body["chat_id"] != hfChat || body["text"] != text || body["message_thread_id"] != nil || body["parse_mode"] != nil || body["reply_markup"] != nil {
		t.Fatalf("body=%v", body)
	}
	e.noLeak(out, nil, "PRIVATE-REPORT-BODY")
	// "--topic general" is the same as no topic.
	if _, err := e.send("again", "--topic", "general"); err != nil || len(e.tg.methods()) != 2 || e.tg.last(t, "sendMessage")["message_thread_id"] != nil {
		t.Fatalf("--topic general: %v %v", err, e.tg.methods())
	}
	// A text of exactly 4000 characters (multi-byte) is accepted.
	if _, err := e.send(strings.Repeat("ệ", 4000)); err != nil {
		t.Fatalf("4000 runes: %v", err)
	}
}

// TC-H1-21 (H1-4, human/abuse): bad input is refused before any Telegram call
// and the error never repeats the text.
func TestHotfixH1_TelegramSendValidation(t *testing.T) {
	e := newHFEnv(t, true)
	cases := map[string]struct {
		text string
		args []string
	}{
		"empty":            {"", nil},
		"newlines only":    {"\n\n", nil},
		"spaces only":      {"   \t ", nil},
		"4001 runes":       {strings.Repeat("a", 4000) + "SECRET-TAIL", nil},
		"4001 multibyte":   {strings.Repeat("ệ", 4001), nil},
		"huge":             {strings.Repeat("SECRET-HUGE ", 20000), nil},
		"invalid utf8":     {"ok SECRET-BAD \xff\xfe", nil},
		"topic task":       {"SECRET-T", []string{"--topic", "task"}},
		"topic workflow":   {"SECRET-T", []string{"--topic", "workflow"}},
		"topic empty":      {"SECRET-T", []string{"--topic", ""}},
		"topic blank":      {"SECRET-T", []string{"--topic", "   "}},
		"topic 65 bytes":   {"SECRET-T", []string{"--topic", strings.Repeat("a", 65)}},
		"topic bad utf8":   {"SECRET-T", []string{"--topic", "a\xff"}},
		"positional text":  {"", []string{"SECRET-ARGV"}},
		"unknown flag":     {"SECRET-T", []string{"--text", "SECRET-ARGV"}},
		"bad workspace id": {"SECRET-T", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			withStdin(t, c.text)
			var out bytes.Buffer
			ws := e.ws
			if name == "bad workspace id" {
				ws = "not-a-uuid"
			}
			err := runCommand(e.ctx, e.pool, append([]string{"telegram", "send", "--workspace-id", ws}, c.args...), &out, e.lookup)
			if err == nil || out.Len() != 0 {
				t.Fatalf("accepted: out=%q err=%v", out.String(), err)
			}
			e.noLeak(out.String(), err, "SECRET-TAIL", "SECRET-HUGE", "SECRET-BAD", "SECRET-ARGV", "SECRET-T")
		})
	}
	// Missing --workspace-id altogether.
	withStdin(t, "x")
	if err := runCommand(e.ctx, e.pool, []string{"telegram", "send"}, io.Discard, e.lookup); err == nil {
		t.Error("accepted without --workspace-id")
	}
	if got := e.tg.methods(); len(got) != 0 {
		t.Fatalf("Telegram was called for invalid input: %v", got)
	}
	// The usage text lists send.
	if !strings.Contains(telegramUsage, "telegram send --workspace-id") {
		t.Error("usage does not mention send")
	}
}

// TC-H1-22 (H1-4): a fixed-name topic is created once, then reused; the
// message goes into it.
func TestHotfixH1_TelegramSendFixedTopic(t *testing.T) {
	e := newHFEnv(t, true)
	out, err := e.send("first", "--topic", "Reports")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.tg.methods(); len(got) != 2 || got[0] != "createForumTopic" || got[1] != "sendMessage" {
		t.Fatalf("calls=%v", got)
	}
	if e.tg.last(t, "createForumTopic")["name"] != "Reports" || e.tg.last(t, "createForumTopic")["chat_id"] != hfChat {
		t.Fatalf("create=%v", e.tg.last(t, "createForumTopic"))
	}
	if e.tg.last(t, "sendMessage")["message_thread_id"] != float64(701) || strings.Contains(out, "fallback") {
		t.Fatalf("send=%v out=%q", e.tg.last(t, "sendMessage"), out)
	}
	// Second send reuses the stored topic.
	if _, err := e.send("second", "--topic", "Reports"); err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, m := range e.tg.methods() {
		if m == "createForumTopic" {
			creates++
		}
	}
	if creates != 1 || e.tg.last(t, "sendMessage")["message_thread_id"] != float64(701) {
		t.Fatalf("creates=%d calls=%v", creates, e.tg.methods())
	}
	// A different name gets its own topic.
	if _, err := e.send("third", "--topic", "Ops"); err != nil || e.tg.last(t, "sendMessage")["message_thread_id"] != float64(702) {
		t.Fatalf("Ops: %v %v", err, e.tg.last(t, "sendMessage"))
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM telegram_topics WHERE workspace_id=$1`, e.ws).Scan(&n); err != nil || n != 2 {
		t.Fatalf("topic rows=%d err=%v", n, err)
	}
	e.noLeak(out, nil, "first", "second")
}

// TC-H1-23 (H1-4): when the topic cannot be made the message still goes to
// General and the result carries the fallback marker.
func TestHotfixH1_TelegramSendFallsBackToGeneral(t *testing.T) {
	e := newHFEnv(t, true)
	e.tg.status["createForumTopic"] = 400
	e.tg.body["createForumTopic"] = `{"ok":false,"error_code":400,"description":"Bad Request: the chat is not a forum"}`
	out, err := e.send("PRIVATE-FALLBACK", "--topic", "Reports")
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if json.Unmarshal([]byte(out), &res) != nil || res["status"] != "sent" || res["topic"] != "fallback_general" {
		t.Fatalf("out=%q", out)
	}
	if body := e.tg.last(t, "sendMessage"); body["message_thread_id"] != nil || body["chat_id"] != hfChat || body["text"] != "PRIVATE-FALLBACK" {
		t.Fatalf("body=%v", body)
	}
	e.noLeak(out, nil, "PRIVATE-FALLBACK")
}

// TC-H1-24 (H1-4): Telegram failures give a safe code, never the text, the
// token or Telegram's own description.
func TestHotfixH1_TelegramSendFailureIsSafe(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"rejected":     {400, `{"ok":false,"error_code":400,"description":"Bad Request: PRIVATE-FALLBACK token fake-bot-token-123456"}`, "rejected"},
		"rate limited": {429, `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}`, "rate_limited"},
		"server":       {502, `bad gateway PRIVATE-FALLBACK`, "server"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newHFEnv(t, true)
			e.tg.status["sendMessage"], e.tg.body["sendMessage"] = tc.status, tc.body
			out, err := e.send("PRIVATE-FALLBACK")
			if err == nil || out != "" || !strings.Contains(err.Error(), "Telegram send failed ("+tc.want+")") {
				t.Fatalf("out=%q err=%v", out, err)
			}
			e.noLeak(out, err, "PRIVATE-FALLBACK")
		})
	}
}

// TC-H1-25 (H1-4, human): the owner chat rule: owner_chat_id, else the
// integration's chat_id; with neither (or a disabled integration) the user is told and nothing
// is sent.
func TestHotfixH1_TelegramSendOwnerChatRule(t *testing.T) {
	e := newHFEnv(t, false)
	// No owner_chat_id: the integration's own chat_id (-10042 from init).
	if _, err := e.send("a"); err != nil || e.tg.last(t, "sendMessage")["chat_id"] != "-10042" {
		t.Fatalf("chat_id fallback: %v %v", err, e.tg.methods())
	}
	// Neither id on the integration: refused, nothing sent.
	if _, err := e.pool.Exec(e.ctx, `UPDATE integrations SET config = config - 'chat_id' - 'owner_chat_id' WHERE workspace_id=$1 AND platform='telegram'`, e.ws); err != nil {
		t.Fatal(err)
	}
	before := len(e.tg.methods())
	out, err := e.send("PRIVATE-NO-CHAT")
	if err == nil || out != "" || !strings.Contains(err.Error(), "no owner chat") || len(e.tg.methods()) != before {
		t.Fatalf("out=%q err=%v calls=%v", out, err, e.tg.methods())
	}
	e.noLeak(out, err, "PRIVATE-NO-CHAT")
}

// TC-H1-26 (H1-4): a disabled control integration or an unknown workspace
// sends nothing.
func TestHotfixH1_TelegramSendDisabledOrUnknown(t *testing.T) {
	e2 := newHFEnv(t, true)
	if _, err := e2.pool.Exec(e2.ctx, `UPDATE integrations SET is_enabled=false WHERE workspace_id=$1 AND platform='telegram'`, e2.ws); err != nil {
		t.Fatal(err)
	}
	if out, err := e2.send("x"); err == nil || out != "" {
		t.Fatalf("disabled integration: out=%q err=%v", out, err)
	}
	// Unknown workspace.
	e2.ws = "019a6690-abcd-7000-8000-0000000000ee"
	if out, err := e2.send("x"); err == nil || out != "" {
		t.Fatalf("unknown workspace: out=%q err=%v", out, err)
	}
	if len(e2.tg.methods()) != 0 {
		t.Fatal("Telegram was called")
	}
}
