package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/labstack/echo/v4"
)

type fakeTelegramTransport func(*http.Request) (*http.Response, error)

func (f fakeTelegramTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Advance only the repository claim clock so transient retries need no wall-clock sleep.
type endToEndStore struct {
	*NoticeRepository
	ticks atomic.Int64
	sent  chan int64
}

func (s *endToEndStore) Claim(ctx context.Context, now time.Time) (*relay.NoticeJob, error) {
	return s.NoticeRepository.Claim(ctx, now.Add(time.Duration(s.ticks.Add(1))*time.Minute))
}
func (s *endToEndStore) MarkSent(ctx context.Context, job relay.NoticeJob, receipt relay.DeliveryReceipt) error {
	if err := s.NoticeRepository.MarkSent(ctx, job, receipt); err != nil {
		return err
	}
	s.sent <- job.ID
	return nil
}

func TestNoticeEndToEnd(t *testing.T) {
	ctx, pool, repo := newNoticeDB(t)
	lookup := func(name string) (string, bool) {
		switch name {
		case "SECRET":
			return "local-test-signature", true
		case "TOKEN":
			return "local-test-token", true
		}
		return "", false
	}
	integrations, _ := NewIntegrationRepository(pool)
	e := echo.New()
	handler := httpdelivery.NewWebhookHandler(integrations, httpdelivery.NewVerifiedParserFactory(lookup), relay.NewNoticeService(repo))
	e.POST("/webhook/:source/:integrationID", handler.HandleWebhook)
	post := func(id string, body []byte) int {
		t.Helper()
		mac := hmac.New(sha256.New, []byte("local-test-signature"))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/webhook/clickup/"+id, bytes.NewReader(body))
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	comment, err := os.ReadFile("../sources/clickup/testdata/comment.json")
	if err != nil {
		t.Fatal(err)
	}
	comment = bytes.ReplaceAll(comment, []byte("Xin chào 👋"), []byte(strings.Repeat("Việt 👋", 2000)))
	status, err := os.ReadFile("../sources/clickup/testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	// Independent source histories must have distinct IDs across event kinds.
	status = bytes.ReplaceAll(bytes.ReplaceAll(status, []byte(`"h1"`), []byte(`"s1"`)), []byte(`"h2"`), []byte(`"s2"`))
	// Callback commits while no worker is running; application restart must retain it.
	for _, body := range [][]byte{comment, status, comment} {
		if code := post(integrationA, body); code != 200 {
			t.Fatalf("callback: %d", code)
		}
	}
	if code := post(missingID, comment); code != 404 {
		t.Fatalf("unknown integration: %d", code)
	}
	if countRows(t, ctx, pool, "notification_outbox") != 4 || countRows(t, ctx, pool, "message_logs") != 4 {
		t.Fatal("batch/dedup lost originals")
	}
	var original string
	if err := pool.QueryRow(ctx, `SELECT content FROM message_logs WHERE external_message_id='comment:c1'`).Scan(&original); err != nil || len(original) < 10000 {
		t.Fatal("long original truncated in history")
	}
	var calls atomic.Int64
	var commentSeen, statusSeen atomic.Bool
	client := telegram.NewClient(&http.Client{Transport: fakeTelegramTransport(func(req *http.Request) (*http.Response, error) {
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if req.URL.Host != "api.telegram.org" || body.ChatID != "-10042" || len(utf16.Encode([]rune(body.Text))) > 4000 {
			t.Error("incorrect Telegram request")
		}
		if strings.Contains(body.Text, "Comment") {
			commentSeen.Store(true)
		}
		if strings.Contains(body.Text, "Status") {
			statusSeen.Store(true)
		}
		n := calls.Add(1)
		code := 200
		payload := `{"ok":true,"result":{"message_id":42}}`
		if n == 1 {
			code = 503
			payload = `{"ok":false,"error_code":503}`
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}, nil
	})}, lookup)
	restarted, err := NewNoticeRepository(pool, repo.newID, DebounceSettings{})
	if err != nil {
		t.Fatal(err)
	}
	store := &endToEndStore{NoticeRepository: restarted, sent: make(chan int64, 4)}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- relay.NewNoticeWorker(store, client, relay.WorkerSettings{PollInterval: time.Millisecond, RequestTimeout: time.Second}).Run(workerCtx)
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-store.sent:
		case <-ctx.Done():
			t.Fatal("deliveries timed out")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var sent int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE status='sent'`).Scan(&sent); err != nil || sent != 4 || calls.Load() != 5 || !commentSeen.Load() || !statusSeen.Load() {
		t.Fatalf("delivery state: sent=%d calls=%d err=%v", sent, calls.Load(), err)
	}
}
