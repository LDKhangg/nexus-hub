package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

const p7Token = "123456:P7-SECRET-TOKEN"

func p7Client(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c := NewClient(s.Client(), &fakeResolver{value: p7Token})
	c.baseURL = s.URL
	return c, s
}

// TC-P7-30: getUpdates posts timeout, limit and allowed_updates, omits the
// offset when it is 0, and returns every update raw.
func TestGetUpdatesRequest(t *testing.T) {
	for _, offset := range []int64{0, 42} {
		t.Run(fmt.Sprint("offset ", offset), func(t *testing.T) {
			var body map[string]any
			c, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/bot"+p7Token+"/getUpdates" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Write([]byte(`{"ok":true,"result":[{"update_id":42,"message":{"text":"a"}},{"update_id":43,"callback_query":{"id":"x"}}]}`))
			})
			got, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", offset, 50, PollAllowedUpdates)
			if err != nil || len(got) != 2 {
				t.Fatalf("updates=%d err=%v", len(got), err)
			}
			if string(got[0]) != `{"update_id":42,"message":{"text":"a"}}` || string(got[1]) != `{"update_id":43,"callback_query":{"id":"x"}}` {
				t.Fatalf("raw updates %s %s", got[0], got[1])
			}
			if body["timeout"] != float64(50) || body["limit"] != float64(100) {
				t.Fatalf("body=%v", body)
			}
			allowed, _ := body["allowed_updates"].([]any)
			if len(allowed) != 2 || allowed[0] != "message" || allowed[1] != "callback_query" {
				t.Fatalf("allowed_updates=%v", body["allowed_updates"])
			}
			v, has := body["offset"]
			if offset == 0 && has {
				t.Fatalf("offset 0 sent: %v", v)
			}
			if offset != 0 && v != float64(offset) {
				t.Fatalf("offset=%v want %d", v, offset)
			}
		})
	}
	t.Run("empty result", func(t *testing.T) {
		c, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true,"result":[]}`)) })
		got, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", 7, 50, PollAllowedUpdates)
		if err != nil || len(got) != 0 {
			t.Fatalf("updates=%v err=%v", got, err)
		}
	})
	t.Run("token not resolved: no request", func(t *testing.T) {
		var called atomic.Bool
		s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
		defer s.Close()
		c := NewClient(s.Client(), &fakeResolver{err: errors.New("missing_credentials")})
		c.baseURL = s.URL
		if _, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", 0, 50, PollAllowedUpdates); err == nil || called.Load() {
			t.Fatalf("err=%v called=%v", err, called.Load())
		}
	})
}

// TC-P7-31: HTTP 409 (or error_code 409) is "conflict" for getUpdates only;
// other methods keep "rejected". Errors never carry the token.
func TestGetUpdatesConflict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http 409", 409, `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active"}`},
		{"envelope 409", 200, `{"ok":false,"error_code":409,"description":"Conflict"}`},
		{"http 409 no body", 409, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := p7Client(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) })
			_, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", 0, 50, PollAllowedUpdates)
			var de *relay.DeliveryError
			if !errors.As(err, &de) || de.Code != "conflict" || ErrorCode(err) != "conflict" {
				t.Fatalf("err=%v", err)
			}
			if strings.Contains(err.Error(), p7Token) || strings.Contains(err.Error(), s.URL) || strings.Contains(err.Error(), "Conflict") {
				t.Fatalf("unsafe error %q", err)
			}
			if err := c.DeleteWebhook(context.Background(), "ws", "env:TOKEN"); ErrorCode(err) != "rejected" {
				t.Fatalf("deleteWebhook 409 code=%s", ErrorCode(err))
			}
			if _, err := c.SendMessage(context.Background(), "ws", "env:TOKEN", "77", "hi", MessageOptions{}); ErrorCode(err) != "rejected" {
				t.Fatalf("sendMessage 409 code=%s", ErrorCode(err))
			}
		})
	}
	t.Run("429 keeps retry_after", func(t *testing.T) {
		c, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			w.Write([]byte(`{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`))
		})
		_, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", 0, 50, PollAllowedUpdates)
		var de *relay.DeliveryError
		if !errors.As(err, &de) || de.Code != "rate_limited" || de.RetryAfter != 7*time.Second {
			t.Fatalf("err=%v", err)
		}
	})
}

// TC-P7-32: a getUpdates response far over 64 KiB is read fully; one over
// the 8 MiB cap fails safely; other methods keep the 64 KiB cap.
func TestGetUpdatesResponseSize(t *testing.T) {
	big := strings.Repeat("x", 3000)
	var sb strings.Builder
	sb.WriteString(`{"ok":true,"result":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"update_id":%d,"message":{"text":"%s"}}`, 1000+i, big)
	}
	sb.WriteString(`]}`)
	payload := sb.String()
	if len(payload) <= 64<<10 {
		t.Fatalf("test payload only %d bytes", len(payload))
	}
	c, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(payload)) })
	got, err := c.GetUpdates(context.Background(), "ws", "env:TOKEN", 0, 50, PollAllowedUpdates)
	if err != nil || len(got) != 100 || rawUpdateID(got[99]) != 1099 {
		t.Fatalf("large response: n=%d err=%v", len(got), err)
	}

	huge := `{"ok":true,"result":[{"update_id":1,"message":{"text":"` + strings.Repeat("y", 9<<20) + `"}}]}`
	c2, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(huge)) })
	if got, err := c2.GetUpdates(context.Background(), "ws", "env:TOKEN", 0, 50, PollAllowedUpdates); ErrorCode(err) != "invalid_response" || got != nil {
		t.Fatalf("over the cap: n=%d err=%v", len(got), err)
	}

	info := `{"ok":true,"result":{"url":"https://h/` + strings.Repeat("z", 70<<10) + `"}}`
	c3, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(info)) })
	if _, err := c3.GetWebhookInfo(context.Background(), "ws", "env:TOKEN"); ErrorCode(err) != "invalid_response" {
		t.Fatalf("getWebhookInfo over 64 KiB: %v", err)
	}
}

// TC-P7-33: BotFingerprint is stable per token, differs between tokens,
// never contains the token and makes no request.
func TestBotFingerprint(t *testing.T) {
	var called atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	defer s.Close()
	a := &fakeResolver{value: p7Token}
	c := NewClient(s.Client(), a)
	c.baseURL = s.URL
	fp1, err1 := c.BotFingerprint(context.Background(), "ws1", "env:A")
	fp2, err2 := c.BotFingerprint(context.Background(), "ws2", "env:B")
	if err1 != nil || err2 != nil || fp1 == "" || fp1 != fp2 {
		t.Fatalf("same token: %q %q %v %v", fp1, fp2, err1, err2)
	}
	if strings.Contains(fp1, p7Token) || strings.Contains(fp1, "P7-SECRET") {
		t.Fatalf("fingerprint leaks token: %q", fp1)
	}
	a.value = "999:OTHER"
	fp3, err := c.BotFingerprint(context.Background(), "ws1", "env:A")
	if err != nil || fp3 == fp1 {
		t.Fatalf("other token: %q err=%v", fp3, err)
	}
	a.err, a.value = errors.New("missing_credentials"), ""
	if fp, err := c.BotFingerprint(context.Background(), "ws1", "env:A"); err == nil || fp != "" {
		t.Fatalf("unresolved token: %q err=%v", fp, err)
	}
	if called.Load() {
		t.Fatal("fingerprint called the API")
	}
}
