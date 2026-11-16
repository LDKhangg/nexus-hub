package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type p6bCall struct {
	method string
	body   map[string]any
}

// p6bAPI is a fake Bot API: reply picks the status and body per method.
type p6bAPI struct {
	mu    sync.Mutex
	calls []p6bCall
}

func (a *p6bAPI) client(t *testing.T, reply func(method string, body map[string]any) (int, string)) *Client {
	t.Helper()
	c, _ := p7Client(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		a.mu.Lock()
		a.calls = append(a.calls, p6bCall{method, body})
		a.mu.Unlock()
		status, resp := reply(method, body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	})
	return c
}

// TC-A7.1 (REQ-6): Bot API descriptions map to fixed codes; the description
// text never appears in the error.
func TestPhase6bTopicErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		desc   string
		want   string
	}{
		{"not a forum", 400, "Bad Request: the chat is not a forum", "topic_not_forum"},
		{"not enough rights create", 400, "Bad Request: not enough rights to create a topic", "topic_no_rights"},
		{"forbidden rights", 403, "Forbidden: not enough rights to manage topics", "topic_no_rights"},
		{"topic closed", 400, "Bad Request: TOPIC_CLOSED", "topic_closed"},
		{"thread not found", 400, "Bad Request: message thread not found", "thread_not_found"},
		{"topic id invalid", 400, "Bad Request: TOPIC_ID_INVALID", "thread_not_found"},
		{"not modified", 400, "Bad Request: TOPIC_NOT_MODIFIED", "topic_not_modified"},
		{"unrelated 400", 400, "Bad Request: message is too long", "rejected"},
		{"blocked 403", 403, "Forbidden: bot was blocked by the user", "rejected"},
		{"flood beats topic text", 429, "Too Many Requests: message thread not found", "rate_limited"},
		{"server beats topic text", 502, "message thread not found", "server"},
		{"topic text on 200 ignored", 404, "message thread not found", "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &p6bAPI{}
			c := api.client(t, func(string, map[string]any) (int, string) {
				return tc.status, `{"ok":false,"error_code":` + strconv.Itoa(tc.status) + `,"description":"` + tc.desc + `"}`
			})
			_, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "hello", MessageOptions{})
			if got := ErrorCode(err); got != tc.want {
				t.Fatalf("SendMessage code=%q, want %q (err=%v)", got, tc.want, err)
			}
			_, err2 := c.SendText(context.Background(), "ws", "ref", "-100", ptr(int64(5)), "hello")
			if got := ErrorCode(err2); got != tc.want {
				t.Fatalf("SendText code=%q, want %q", got, tc.want)
			}
			for _, e := range []error{err, err2} {
				if strings.Contains(e.Error(), tc.desc) || strings.Contains(e.Error(), p7Token) {
					t.Fatalf("error leaks text: %v", e)
				}
			}
		})
	}
}

// TC-A7.2 (REQ-6): createForumTopic sends chat_id and a name of at most 128
// units, returns the thread id, and refuses a missing id.
func TestPhase6bCreateForumTopic(t *testing.T) {
	api := &p6bAPI{}
	c := api.client(t, func(_ string, body map[string]any) (int, string) {
		if body["name"] == "no-id" {
			return 200, `{"ok":true,"result":{}}`
		}
		return 200, `{"ok":true,"result":{"message_thread_id":4242,"name":"ignored"}}`
	})
	id, err := c.CreateForumTopic(context.Background(), "ws", "ref", "-100", "T1 · Tiêu đề 🙂")
	if err != nil || id != 4242 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	long := strings.Repeat("😀", 100) // 200 UTF-16 units
	if _, err := c.CreateForumTopic(context.Background(), "ws", "ref", "-100", long); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateForumTopic(context.Background(), "ws", "ref", "-100", "no-id"); ErrorCode(err) != "invalid_response" {
		t.Fatalf("missing thread id err=%v", err)
	}
	if got := api.calls[0]; got.method != "createForumTopic" || got.body["chat_id"] != "-100" || got.body["name"] != "T1 · Tiêu đề 🙂" {
		t.Fatalf("call=%+v", got)
	}
	if n := units(api.calls[1].body["name"].(string)); n > 128 || n == 0 {
		t.Fatalf("name units=%d, want 1..128", n)
	}
}

// TC-A7.3 (REQ-6): close/reopen send the thread id; "not modified" is a
// success; other topic errors keep their code.
func TestPhase6bCloseReopenTopic(t *testing.T) {
	api := &p6bAPI{}
	notModified := false
	c := api.client(t, func(string, map[string]any) (int, string) {
		if notModified {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`
		}
		return 200, `{"ok":true,"result":true}`
	})
	if err := c.ReopenForumTopic(context.Background(), "ws", "ref", "-100", 9); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseForumTopic(context.Background(), "ws", "ref", "-100", 9); err != nil {
		t.Fatal(err)
	}
	if api.calls[0].method != "reopenForumTopic" || api.calls[0].body["message_thread_id"] != float64(9) || api.calls[1].method != "closeForumTopic" {
		t.Fatalf("calls=%+v", api.calls)
	}
	notModified = true
	if err := c.ReopenForumTopic(context.Background(), "ws", "ref", "-100", 9); err != nil {
		t.Fatalf("TOPIC_NOT_MODIFIED must succeed: %v", err)
	}
	gone := api.client(t, func(string, map[string]any) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`
	})
	if err := gone.ReopenForumTopic(context.Background(), "ws", "ref", "-100", 9); ErrorCode(err) != "thread_not_found" {
		t.Fatalf("err=%v", err)
	}
}

// TC-A7.4 (REQ-5): the Messenger sends message_thread_id only for a topic.
func TestPhase6bSendMessageThreadID(t *testing.T) {
	api := &p6bAPI{}
	c := api.client(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{"message_id":7}}` })
	thread := int64(31)
	if _, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "a", MessageOptions{ThreadID: &thread}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "b", MessageOptions{}); err != nil {
		t.Fatal(err)
	}
	if api.calls[0].body["message_thread_id"] != float64(31) {
		t.Fatalf("topic send lost the thread id: %v", api.calls[0].body)
	}
	if _, ok := api.calls[1].body["message_thread_id"]; ok {
		t.Fatalf("General send carries a thread id: %v", api.calls[1].body)
	}
}
