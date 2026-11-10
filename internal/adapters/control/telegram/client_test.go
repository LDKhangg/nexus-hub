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
		c := NewClient(s.Client(), func(string) (string, bool) { return "fake-token", true })
		c.baseURL = s.URL
		receipt, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-1001234567890", MessageThreadID: topic, BotTokenEnv: "TOKEN"}, domain.NormalizedEvent{ExternalID: "t1", Content: "Tiếng Việt <>& 😀"})
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
			c := NewClient(s.Client(), func(string) (string, bool) { return "fake-token", true })
			c.baseURL = s.URL
			_, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenEnv: "TOKEN"}, domain.NormalizedEvent{})
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
	c := NewClient(s.Client(), func(string) (string, bool) { return "", false })
	c.baseURL = s.URL
	if _, err := c.Send(context.Background(), tenant.TelegramTargetConfig{ChatID: "-42", BotTokenEnv: "TOKEN"}, domain.NormalizedEvent{}); err == nil || called {
		t.Fatal("missing token must not send")
	}
	c = NewClient(s.Client(), func(string) (string, bool) { return "fake-token", true })
	c.baseURL = s.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Send(ctx, tenant.TelegramTargetConfig{ChatID: "-42", BotTokenEnv: "TOKEN"}, domain.NormalizedEvent{})
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
