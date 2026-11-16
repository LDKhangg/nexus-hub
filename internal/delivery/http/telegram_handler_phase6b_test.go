package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

func p6bPost(t *testing.T, h *TelegramHandler, integration tenant.WebhookIntegration, body string) {
	t.Helper()
	_ = integration
	e := echo.New()
	e.POST("/webhook/telegram/:integrationID", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+testIntegrationID, strings.NewReader(body))
	req.Header.Set(TelegramSecretHeader, "webhook-secret")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func p6bHandler(cfg map[string]any, globalChat, ownerUser int64) (*TelegramHandler, *telegramInboxFake, tenant.WebhookIntegration) {
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: p7Workspace, Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true, Config: cfg}
	lookup := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil })
	inbox := &telegramInboxFake{}
	h := NewTelegramHandler(lookup, testResolver{value: "webhook-secret", ok: true}, inbox, noOpenAnswerFake{}, globalChat)
	if ownerUser != 0 {
		h.WithOwnerUser(ownerUser)
	}
	return h, inbox, integration
}

func p6bMsg(id, chat, from, thread int64, text string) string {
	th := ""
	if thread != 0 {
		th = `,"message_thread_id":` + itoa(thread) + `,"is_topic_message":true`
	}
	return `{"update_id":` + itoa(id) + `,"message":{"message_id":9` + th + `,"chat":{"id":` + itoa(chat) + `},"from":{"id":` + itoa(from) + `},"text":"` + text + `"}}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TC-A3.4 (REQ-3): the owner group is the integration's owner_chat_id, then
// chat_id, then the global setting; other chats and other users are ignored
// and store nothing.
func TestPhase6bOwnerGroupSelection(t *testing.T) {
	const group, other, owner = int64(-100777), int64(-100888), int64(42)
	t.Run("owner_chat_id with owner user accepted, topic stored", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
		p6bPost(t, h, in, p6bMsg(1, group, owner, 55, "/status"))
		if len(inbox.updates) != 1 || inbox.updates[0].ChatID != group || inbox.updates[0].ThreadID != 55 || inbox.updates[0].Command != "/status" {
			t.Fatalf("updates=%+v", inbox.updates)
		}
	})
	t.Run("chat_id used when owner_chat_id absent", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"chat_id": "-100777"}, 77, owner)
		p6bPost(t, h, in, p6bMsg(2, group, owner, 0, "/status"))
		if len(inbox.updates) != 1 || inbox.updates[0].Command != "/status" || inbox.updates[0].ThreadID != 0 {
			t.Fatalf("updates=%+v", inbox.updates)
		}
	})
	t.Run("global chat is the fallback", func(t *testing.T) {
		h, inbox, in := p6bHandler(nil, 77, 0)
		p6bPost(t, h, in, p6bMsg(3, 77, 77, 0, "/status"))
		if len(inbox.updates) != 1 || inbox.updates[0].Command != "/status" {
			t.Fatalf("updates=%+v", inbox.updates)
		}
	})
	t.Run("global chat is ignored when the workspace has its own group", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
		p6bPost(t, h, in, p6bMsg(4, 77, owner, 0, "/status"))
		for _, u := range inbox.updates {
			if u.Command != "" || u.Kind == behavior.UpdateCommand {
				t.Fatalf("command from the global chat accepted: %+v", u)
			}
		}
	})
	t.Run("another group is ignored", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
		p6bPost(t, h, in, p6bMsg(5, other, owner, 3, "/status"))
		for _, u := range inbox.updates {
			if u.Command != "" || u.Kind == behavior.UpdateCommand {
				t.Fatalf("command from another group accepted: %+v", u)
			}
		}
	})
	t.Run("another user in the owner group is ignored", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
		p6bPost(t, h, in, p6bMsg(6, group, 4242, 3, "/status"))
		for _, u := range inbox.updates {
			if u.Command != "" || u.Kind == behavior.UpdateCommand || u.Args != "" {
				t.Fatalf("command from another user accepted: %+v", u)
			}
		}
	})
	t.Run("group without an owner user id never matches", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, 0)
		p6bPost(t, h, in, p6bMsg(7, group, 99, 0, "/status"))
		for _, u := range inbox.updates {
			if u.Command != "" || u.Kind == behavior.UpdateCommand {
				t.Fatalf("group command accepted without owner user: %+v", u)
			}
		}
	})
	t.Run("a forged text update from a stranger stores no text", func(t *testing.T) {
		h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
		p6bPost(t, h, in, p6bMsg(8, group, 4242, 5, "SECRET-STRANGER-TEXT"))
		for _, u := range inbox.updates {
			if strings.Contains(u.Args+u.Note+u.Command, "SECRET-STRANGER-TEXT") {
				t.Fatalf("text stored: %+v", u)
			}
		}
	})
}

// TC-A3.5 (REQ-3): the topic id of messages and of callback messages is
// parsed; absent, zero and negative values are stored as 0 (General).
func TestPhase6bThreadIDParsing(t *testing.T) {
	const group, owner = int64(-100777), int64(42)
	cfg := map[string]any{"owner_chat_id": float64(group)}
	h, inbox, in := p6bHandler(cfg, 77, owner)
	p6bPost(t, h, in, p6bMsg(11, group, owner, 0, "/status"))
	p6bPost(t, h, in, p6bMsg(12, group, owner, 31, "/status"))
	p6bPost(t, h, in, `{"update_id":13,"message":{"message_id":9,"message_thread_id":-5,"is_topic_message":true,"chat":{"id":-100777},"from":{"id":42},"text":"/status"}}`)
	cb := behavior.CallbackData{Kind: behavior.CallbackApprove, Nonce: "abcdefghijklmnop0123456789", Option: -1}
	data, err := cb.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p6bPost(t, h, in, `{"update_id":14,"callback_query":{"id":"cb14","from":{"id":42},"message":{"message_id":8,"message_thread_id":66,"is_topic_message":true,"chat":{"id":-100777}},"data":"`+data+`"}}`)
	p6bPost(t, h, in, `{"update_id":15,"callback_query":{"id":"cb15","from":{"id":42},"message":{"message_id":8,"chat":{"id":-100777}},"data":"`+data+`"}}`)
	want := map[int64]int64{11: 0, 12: 31, 13: 0, 14: 66, 15: 0}
	got := map[int64]int64{}
	for _, u := range inbox.updates {
		got[u.UpdateID] = u.ThreadID
	}
	for id, th := range want {
		if g, ok := got[id]; !ok || g != th {
			t.Errorf("update %d: thread=%d present=%v, want %d", id, g, ok, th)
		}
	}
	// A callback from a stranger in the right topic carries no effect.
	h2, inbox2, in2 := p6bHandler(cfg, 77, owner)
	p6bPost(t, h2, in2, `{"update_id":21,"callback_query":{"id":"cb21","from":{"id":4242},"message":{"message_id":8,"message_thread_id":66,"is_topic_message":true,"chat":{"id":-100777}},"data":"`+data+`"}}`)
	for _, u := range inbox2.updates {
		if u.Kind == behavior.UpdateCallback && u.Command != "" {
			t.Fatalf("stranger callback accepted: %+v", u)
		}
	}
}

// TC-A13.1 (R1): message_thread_id names a topic only with is_topic_message
// true. A reply chain in a non-forum group (flag false or missing) stays in
// thread 0, so it shares General's conversation and gets a reply without a
// thread; messages and callback messages behave the same.
func TestPhase6bThreadIDNeedsIsTopicMessage(t *testing.T) {
	const group, owner = int64(-100777), int64(42)
	cfg := map[string]any{"owner_chat_id": float64(group)}
	h, inbox, in := p6bHandler(cfg, 77, owner)
	cb := behavior.CallbackData{Kind: behavior.CallbackApprove, Nonce: "abcdefghijklmnop0123456789", Option: -1}
	data, err := cb.Encode()
	if err != nil {
		t.Fatal(err)
	}
	msg := func(id int64, flag string) string {
		return `{"update_id":` + itoa(id) + `,"message":{"message_id":9,"message_thread_id":88` + flag + `,"chat":{"id":-100777},"from":{"id":42},"text":"/status"}}`
	}
	press := func(id int64, flag string) string {
		return `{"update_id":` + itoa(id) + `,"callback_query":{"id":"cb` + itoa(id) + `","from":{"id":42},"message":{"message_id":8,"message_thread_id":88` + flag + `,"chat":{"id":-100777}},"data":"` + data + `"}}`
	}
	p6bPost(t, h, in, msg(31, `,"is_topic_message":false`))
	p6bPost(t, h, in, msg(32, ``))
	p6bPost(t, h, in, msg(33, `,"is_topic_message":true`))
	p6bPost(t, h, in, press(34, `,"is_topic_message":false`))
	p6bPost(t, h, in, press(35, ``))
	p6bPost(t, h, in, press(36, `,"is_topic_message":true`))
	want := map[int64]int64{31: 0, 32: 0, 33: 88, 34: 0, 35: 0, 36: 88}
	got := map[int64]behavior.InboundUpdate{}
	for _, u := range inbox.updates {
		got[u.UpdateID] = u
	}
	for id, th := range want {
		u, ok := got[id]
		if !ok || u.ThreadID != th {
			t.Errorf("update %d: thread=%d present=%v, want %d", id, u.ThreadID, ok, th)
		}
		if ok && id < 33 && (u.Command != "/status" || u.ChatID != group) {
			t.Errorf("update %d lost its command: %+v", id, u)
		}
	}
}
