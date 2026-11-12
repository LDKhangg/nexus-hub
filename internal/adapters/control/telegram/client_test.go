package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

func TestSendRequest(t *testing.T) {
	for _, topic := range []*int64{nil, ptr(int64(12))} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/botfake-token/sendMessage" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["chat_id"] != "-1001234567890" || body["parse_mode"] != nil || !strings.Contains(body["text"].(string), "Tiếng Việt <>& 😀") {
				t.Errorf("wrong body: %v", body)
			}
			if topic == nil && body["message_thread_id"] != nil {
				t.Error("unexpected topic")
			}
			if topic != nil && body["message_thread_id"] != float64(*topic) {
				t.Error("missing topic")
			}
			w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
		}))
		c := NewClient(s.Client(), envResolver(func(string) (string, bool) { return "fake-token", true }))
		c.baseURL = s.URL
		receipt, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-1001234567890", MessageThreadID: topic, BotTokenRef: "env:TOKEN"}, domain.NormalizedEvent{ExternalID: "t1", Content: "Tiếng Việt <>& 😀"})
		if err != nil || receipt.MessageID != 42 {
			t.Fatalf("receipt=%v err=%v", receipt, err)
		}
		s.Close()
	}
}

func TestSendErrorsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		retry  bool
	}{
		{"envelope", 200, `{"ok":false,"error_code":403,"description":"fake-token"}`, false},
		{"malformed", 200, `not-json fake-token`, true},
		{"no receipt", 200, `{"ok":true,"result":{}}`, true},
		{"unauthorized", 401, `{}`, false}, {"forbidden", 403, `{}`, false},
		{"flood", 429, `{"ok":false,"parameters":{"retry_after":99}}`, true},
		{"server", 503, `fake-token`, true},
		{"migration", 400, `{"ok":false,"parameters":{"migrate_to_chat_id":-10042}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer s.Close()
			c := NewClient(s.Client(), envResolver(func(string) (string, bool) { return "fake-token", true }))
			c.baseURL = s.URL
			_, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenRef: "env:TOKEN"}, domain.NormalizedEvent{})
			var delivery *relay.DeliveryError
			if !errors.As(err, &delivery) || delivery.Retryable != tc.retry || strings.Contains(err.Error(), "fake-token") {
				t.Fatalf("unsafe/wrong error %v", err)
			}
			if tc.name == "flood" && delivery.RetryAfter.Seconds() != 99 {
				t.Fatal("retry_after lost")
			}
		})
	}
	called := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer s.Close()
	c := NewClient(s.Client(), envResolver(func(string) (string, bool) { return "", false }))
	c.baseURL = s.URL
	if _, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenRef: "env:TOKEN"}, domain.NormalizedEvent{}); err == nil || called {
		t.Fatal("missing token must not send")
	}
	c = NewClient(s.Client(), envResolver(func(string) (string, bool) { return "fake-token", true }))
	c.baseURL = s.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Send(ctx, tenant.TelegramTargetConfig{ChatID: "-42", BotTokenRef: "env:TOKEN"}, domain.NormalizedEvent{})
	if err == nil || strings.Contains(err.Error(), "fake-token") {
		t.Fatalf("cancellation error %v", err)
	}
}

func TestFormatBounded(t *testing.T) {
	e := domain.NormalizedEvent{ExternalID: "task-1", Content: strings.Repeat("😀 Việt", 2000), Metadata: map[string]string{"task_url": "https://app.clickup.com/t/task-1"}}
	text := FormatNotice(e)
	if len(utf16.Encode([]rune(text))) > 4000 || !strings.Contains(text, e.Metadata["task_url"]) || !strings.Contains(text, "Unknown user") || !strings.Contains(text, "task-1") || !strings.Contains(text, "[truncated]") {
		t.Fatalf("bad bounded notice (%d)", len(text))
	}
}
func ptr[T any](v T) *T { return &v }

type fakeResolver struct {
	ws, ref string
	value   string
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, ws, ref string) (string, error) {
	f.ws, f.ref = ws, ref
	return f.value, f.err
}

// envResolver adapts the old lookup-style fakes.
func envResolver(lookup func(string) (string, bool)) *fakeResolver {
	v, ok := lookup("TOKEN")
	r := &fakeResolver{value: v}
	if !ok || v == "" {
		r.err = errors.New("missing_credentials")
	}
	return r
}

func TestTelegramClient_UsesResolver(t *testing.T) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer s.Close()
	res := &fakeResolver{value: "db-token"}
	c := NewClient(s.Client(), res)
	c.baseURL = s.URL
	if _, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenRef: "telegram"}, domain.NormalizedEvent{WorkspaceID: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	if res.ws != "ws-1" || res.ref != "telegram" || path != "/botdb-token/sendMessage" {
		t.Fatalf("resolver %+v path %s", res, path)
	}
	failing := &fakeResolver{err: errors.New("secrets: decrypt_failed")}
	c = NewClient(s.Client(), failing)
	c.baseURL = s.URL
	_, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenRef: "telegram"}, domain.NormalizedEvent{WorkspaceID: "ws-1"})
	var de *relay.DeliveryError
	if !errors.As(err, &de) || de.Retryable {
		t.Fatalf("err %v", err)
	}
}

func TestSendText_ClipsPlainText(t *testing.T) {
	var body map[string]any
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer s.Close()
	res := &fakeResolver{value: "bot-token"}
	c := NewClient(s.Client(), res)
	c.baseURL = s.URL
	topic := int64(5)
	id, err := c.SendText(context.Background(), "ws-9", "telegram", "-100", &topic, strings.Repeat("😀", 3000)+"<b>x</b>")
	if err != nil || id != 99 {
		t.Fatalf("%d %v", id, err)
	}
	text := body["text"].(string)
	if res.ws != "ws-9" || res.ref != "telegram" || path != "/botbot-token/sendMessage" || body["parse_mode"] != nil || body["message_thread_id"] != float64(5) {
		t.Fatalf("%+v %s %v", res, path, body)
	}
	if len(utf16.Encode([]rune(text))) > 4000 || !strings.HasSuffix(text, "[truncated]") {
		t.Fatalf("len %d", len(utf16.Encode([]rune(text))))
	}
	c = NewClient(s.Client(), &fakeResolver{err: errors.New("x")})
	if _, err := c.SendText(context.Background(), "ws", "telegram", "-1", nil, "hi"); err == nil {
		t.Fatal("missing token accepted")
	}
}
