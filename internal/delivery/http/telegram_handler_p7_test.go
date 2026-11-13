package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// failingInboxFake fails every RecordUpdate.
type failingInboxFake struct{ calls int }

func (f *failingInboxFake) RecordUpdate(context.Context, string, behavior.InboundUpdate) (bool, error) {
	f.calls++
	return false, errors.New("db down")
}
func (*failingInboxFake) ClaimUpdate(context.Context, time.Time, time.Duration) (*behavior.InboundUpdate, error) {
	return nil, nil
}
func (*failingInboxFake) FinishUpdate(context.Context, int64, string, string) error { return nil }

// p7ErrResolver fails every Resolve with err.
type p7ErrResolver struct{ err error }

func (r p7ErrResolver) Resolve(context.Context, string, string) (string, error) { return "", r.err }

const p7Workspace = "019a6690-abcd-7000-8000-000000000001"

func p7Integration() tenant.WebhookIntegration {
	return tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: p7Workspace, Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}
}

func p7Lookup() lookupFunc {
	return lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return p7Integration(), nil })
}

func p7Post(t *testing.T, h *TelegramHandler, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.POST("/webhook/telegram/:integrationID", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+testIntegrationID, bytes.NewReader(body))
	if secret != "" {
		req.Header.Set(TelegramSecretHeader, secret)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func p7Body(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

const (
	p7OwnerText  = `{"update_id":701,"message":{"message_id":1,"chat":{"id":77},"from":{"id":77},"text":"/status"}}`
	p7OtherChat  = `{"update_id":702,"message":{"message_id":2,"chat":{"id":88},"from":{"id":88},"text":"tin nhắn lạ BÍ-MẬT"}}`
	p7OwnerPress = `{"update_id":703,"callback_query":{"id":"cb1","from":{"id":77},"message":{"message_id":5,"chat":{"id":77}},"data":"ok"}}`
)

// TC-P7-1: HandleUpdate gives the webhook's outcome for the same body:
// queued, duplicate, ignored and invalid. Each case runs once through the
// webhook and once through HandleUpdate on separate, identical handlers.
func TestTelegramHandleUpdateMatchesWebhook(t *testing.T) {
	type outcome struct {
		code   int
		status string
	}
	webhookOutcome := map[string]outcome{
		UpdateQueued:    {200, "queued"},
		UpdateDuplicate: {200, "duplicate"},
		UpdateIgnored:   {200, "ignored"},
		UpdateInvalid:   {400, ""},
	}
	for _, tc := range []struct {
		name   string
		bodies []string
		want   []string
	}{
		{"owner command queued then duplicate", []string{p7OwnerText, p7OwnerText}, []string{UpdateQueued, UpdateDuplicate}},
		{"owner button press queued", []string{p7OwnerPress}, []string{UpdateQueued}},
		{"non-owner chat ignored", []string{p7OtherChat}, []string{UpdateIgnored}},
		{"owner chat, other sender ignored", []string{`{"update_id":704,"message":{"message_id":3,"chat":{"id":77},"from":{"id":88},"text":"hi"}}`}, []string{UpdateIgnored}},
		{"not json", []string{`{not json`}, []string{UpdateInvalid}},
		{"no update_id", []string{`{"message":{"message_id":1,"chat":{"id":77},"from":{"id":77},"text":"x"}}`}, []string{UpdateInvalid}},
		{"zero update_id", []string{`{"update_id":0,"message":{"message_id":1,"chat":{"id":77},"from":{"id":77},"text":"x"}}`}, []string{UpdateInvalid}},
		{"negative update_id", []string{`{"update_id":-5}`}, []string{UpdateInvalid}},
		{"update_id as string", []string{`{"update_id":"9"}`}, []string{UpdateInvalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookInbox, pollInbox := &telegramInboxFake{}, &telegramInboxFake{}
			hook := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, hookInbox, answerFake{}, 77)
			poll := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, pollInbox, answerFake{}, 77)
			for i, body := range tc.bodies {
				got, err := poll.HandleUpdate(context.Background(), testIntegrationID, []byte(body))
				if err != nil || got != tc.want[i] {
					t.Fatalf("body %d: HandleUpdate=%q err=%v want %q", i, got, err, tc.want[i])
				}
				rec := p7Post(t, hook, "webhook-secret", []byte(body))
				w := webhookOutcome[tc.want[i]]
				if rec.Code != w.code {
					t.Fatalf("body %d: webhook code=%d want %d (%s)", i, rec.Code, w.code, rec.Body)
				}
				if w.code == 200 && p7Body(t, rec)["status"] != w.status {
					t.Fatalf("body %d: webhook body=%s", i, rec.Body)
				}
				if w.code == 400 && p7Body(t, rec)["error"] != "invalid update" {
					t.Fatalf("body %d: webhook body=%s", i, rec.Body)
				}
			}
			if len(hookInbox.updates) != len(pollInbox.updates) {
				t.Fatalf("stored webhook=%d poll=%d", len(hookInbox.updates), len(pollInbox.updates))
			}
			for i := range pollInbox.updates {
				if hookInbox.updates[i] != pollInbox.updates[i] {
					t.Fatalf("stored update differs:\nwebhook=%+v\npoll   =%+v", hookInbox.updates[i], pollInbox.updates[i])
				}
				if u := pollInbox.updates[i]; u.WorkspaceID != p7Workspace || u.IntegrationID != testIntegrationID || u.UpdateID <= 0 {
					t.Fatalf("stored update ids=%+v", u)
				}
			}
		})
	}
}

// TC-P7-2: an update over the webhook body limit is invalid for polling
// (skipped, never an error) and 413 for the webhook; nothing is stored.
func TestTelegramHandleUpdateOversize(t *testing.T) {
	text := strings.Repeat("a", maxBodyBytes)
	body := []byte(`{"update_id":705,"message":{"message_id":1,"chat":{"id":77},"from":{"id":77},"text":"` + text + `"}}`)
	inbox := &telegramInboxFake{}
	h := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)
	got, err := h.HandleUpdate(context.Background(), testIntegrationID, body)
	if err != nil || got != UpdateInvalid {
		t.Fatalf("oversize: %q err=%v", got, err)
	}
	rec := p7Post(t, h, "webhook-secret", body)
	if rec.Code != http.StatusRequestEntityTooLarge || p7Body(t, rec)["error"] != "body too large" {
		t.Fatalf("webhook oversize: %d %s", rec.Code, rec.Body)
	}
	if len(inbox.updates) != 0 {
		t.Fatalf("oversize stored: %d", len(inbox.updates))
	}
	// Exactly at the limit is not oversize: it goes through the normal path.
	pad := maxBodyBytes - len(`{"update_id":706,"x":""}`)
	exact := []byte(`{"update_id":706,"x":"` + strings.Repeat("b", pad) + `"}`)
	if len(exact) != maxBodyBytes {
		t.Fatalf("test body size %d", len(exact))
	}
	if got, err := h.HandleUpdate(context.Background(), testIntegrationID, exact); err != nil || got == UpdateInvalid {
		t.Fatalf("body at the limit: %q err=%v", got, err)
	}
}

// TC-P7-3: a failed lookup, an unknown or non-control integration, a bad
// integration id or a store failure return ErrUpdateUnavailable (the
// update is kept for later), and the webhook still answers as before.
func TestTelegramHandleUpdateUnavailable(t *testing.T) {
	ctx := context.Background()
	failing := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) {
		return tenant.WebhookIntegration{}, errors.New("db down")
	})
	notFound := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) {
		return tenant.WebhookIntegration{}, tenant.ErrIntegrationNotFound
	})
	source := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) {
		in := p7Integration()
		in.Platform, in.Role = "clickup", tenant.IntegrationRoleSource
		return in, nil
	})
	disabled := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) {
		in := p7Integration()
		in.IsEnabled = false
		return in, nil
	})
	for _, tc := range []struct {
		name     string
		lookup   lookupFunc
		id       string
		hookCode int
	}{
		{"lookup failure", failing, testIntegrationID, 503},
		{"unknown integration", notFound, testIntegrationID, 404},
		{"source integration", source, testIntegrationID, 404},
		{"disabled integration", disabled, testIntegrationID, 404},
		{"bad integration id", p7Lookup(), "not-a-uuid", 0},
		{"empty integration id", p7Lookup(), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := &telegramInboxFake{}
			h := NewTelegramHandler(tc.lookup, testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)
			got, err := h.HandleUpdate(ctx, tc.id, []byte(p7OwnerText))
			if !errors.Is(err, ErrUpdateUnavailable) || got != "" {
				t.Fatalf("HandleUpdate=%q err=%v", got, err)
			}
			if len(inbox.updates) != 0 {
				t.Fatal("stored despite failure")
			}
			if tc.hookCode != 0 {
				if rec := p7Post(t, h, "webhook-secret", []byte(p7OwnerText)); rec.Code != tc.hookCode {
					t.Fatalf("webhook code=%d want %d", rec.Code, tc.hookCode)
				}
			}
		})
	}

	t.Run("store failure", func(t *testing.T) {
		inbox := &failingInboxFake{}
		h := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)
		got, err := h.HandleUpdate(ctx, testIntegrationID, []byte(p7OwnerText))
		if !errors.Is(err, ErrUpdateUnavailable) || got != "" || inbox.calls != 1 {
			t.Fatalf("HandleUpdate=%q err=%v calls=%d", got, err, inbox.calls)
		}
		rec := p7Post(t, h, "webhook-secret", []byte(p7OwnerText))
		if rec.Code != 503 || p7Body(t, rec)["error"] != "service unavailable" {
			t.Fatalf("webhook store failure: %d %s", rec.Code, rec.Body)
		}
		// Ignored updates never reach the store, so a broken store does not
		// turn them into errors.
		if got, err := h.HandleUpdate(ctx, testIntegrationID, []byte(p7OtherChat)); err != nil || got != UpdateIgnored {
			t.Fatalf("ignored with broken store: %q err=%v", got, err)
		}
	})
}

// TC-P7-4: HandleUpdate needs no webhook secret (polling has no header),
// while the webhook still rejects a missing, wrong or unconfigured secret
// with 401 and stores nothing.
func TestTelegramP7WebhookSecretRegression(t *testing.T) {
	inbox := &telegramInboxFake{}
	h := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)
	for _, secret := range []string{"", "wrong-secret", "webhook-secre", "webhook-secret "} {
		rec := p7Post(t, h, secret, []byte(p7OwnerText))
		if rec.Code != http.StatusUnauthorized || p7Body(t, rec)["error"] != "unauthorized" {
			t.Fatalf("secret %q: %d %s", secret, rec.Code, rec.Body)
		}
	}
	if len(inbox.updates) != 0 {
		t.Fatal("unauthenticated update stored")
	}
	// Not configured: an empty value, or the resolver's missing-credentials
	// error, is 401 as before P7.
	for name, r := range map[string]SecretResolver{
		"empty value":         testResolver{value: "", ok: true},
		"missing credentials": p7ErrResolver{&secrets.Error{Code: secrets.CodeMissingCredentials}},
	} {
		if rec := p7Post(t, NewTelegramHandler(p7Lookup(), r, inbox, answerFake{}, 77), "anything", []byte(p7OwnerText)); rec.Code != http.StatusUnauthorized || p7Body(t, rec)["error"] != "unauthorized" {
			t.Fatalf("unconfigured secret (%s): %d %s", name, rec.Code, rec.Body)
		}
	}
	// Any other resolver failure is 503 as before P7.
	unconfigured := NewTelegramHandler(p7Lookup(), testResolver{ok: false}, inbox, answerFake{}, 77)
	if rec := p7Post(t, unconfigured, "anything", []byte(p7OwnerText)); rec.Code != http.StatusServiceUnavailable || p7Body(t, rec)["error"] != "service unavailable" {
		t.Fatalf("resolver failure: %d %s", rec.Code, rec.Body)
	}
	if len(inbox.updates) != 0 {
		t.Fatal("update stored without a usable secret")
	}
	// Polling path: the resolver is never needed.
	if got, err := unconfigured.HandleUpdate(context.Background(), testIntegrationID, []byte(p7OwnerText)); err != nil || got != UpdateQueued {
		t.Fatalf("poll without webhook secret: %q err=%v", got, err)
	}
	// Right secret after the rejections: the same update is queued once
	// (the poll above stored it, so the webhook sees a duplicate).
	rec := p7Post(t, h, "webhook-secret", []byte(p7OwnerText))
	if rec.Code != 200 || p7Body(t, rec)["status"] != "duplicate" {
		t.Fatalf("webhook after poll: %d %s", rec.Code, rec.Body)
	}
}

// TC-P7-5: the polling path's log lines carry no message text.
func TestTelegramHandleUpdateLogsNoText(t *testing.T) {
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	h := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, &telegramInboxFake{}, answerFake{}, 77)
	if got, err := h.HandleUpdate(context.Background(), testIntegrationID, []byte(p7OtherChat)); err != nil || got != UpdateIgnored {
		t.Fatalf("%q err=%v", got, err)
	}
	out := buf.String()
	if !strings.Contains(out, "telegram poll") {
		t.Fatalf("ignored update not logged with the poll label: %q", out)
	}
	if strings.Contains(out, "BÍ-MẬT") || strings.Contains(out, "tin nhắn") {
		t.Fatalf("message text logged: %q", out)
	}
}
