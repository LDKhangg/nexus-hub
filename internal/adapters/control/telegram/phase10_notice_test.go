package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 10 Batch A, R1 (REQ-1): the notice text of an assignee event and the "open task topic" button.

func p10NoticeServer(t *testing.T) (*Client, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	t.Cleanup(s.Close)
	c := NewClient(s.Client(), envResolver(func(string) (string, bool) { return "fake-token", true }))
	c.baseURL = s.URL
	return c, &bodies
}

var p10Target = tenant.TelegramTargetConfig{ChatID: "-1001234567890", BotTokenRef: "env:TOKEN"}

// TC-R1a (REQ-1): an assignee notice shows who was added; the name is clipped; unknown people have a fallback; no event text leaks in.
func TestP10Notice_AssigneeEventText(t *testing.T) {
	base := domain.NormalizedEvent{EventType: domain.EventAssigneeChanged, ExternalID: "86abc", Title: "Login", SenderName: "Lan",
		Content: "SECRET-CONTENT-NOT-SHOWN", Metadata: map[string]string{}}
	cases := []struct {
		name string
		meta map[string]string
		want string
		deny string
	}{
		{"directory name", map[string]string{"assignee_added": "9", "assignee_name": "Nguyễn Văn Nam"}, "+ Nguyễn Văn Nam", "+ 9"},
		{"id fallback", map[string]string{"assignee_added": "9"}, "+ 9", ""},
		{"unknown user", map[string]string{}, "+ Unknown user", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := base
			e.Metadata = c.meta
			got := FormatNotice(e)
			if !strings.Contains(got, "Assignee added") || !strings.Contains(got, c.want) || !strings.Contains(got, "86abc") {
				t.Fatalf("notice %q", got)
			}
			if c.deny != "" && strings.Contains(got, c.deny) {
				t.Fatalf("notice %q shows %q", got, c.deny)
			}
			if strings.Contains(got, "SECRET-CONTENT-NOT-SHOWN") {
				t.Fatalf("event content shown: %q", got)
			}
		})
	}
	long := base
	long.Metadata = map[string]string{"assignee_name": strings.Repeat("Đ", 500)}
	if got := FormatNotice(long); len(got) > 4096 || strings.Count(got, "Đ") > 200 {
		t.Fatalf("long name not clipped: %d bytes, %d runes", len(got), strings.Count(got, "Đ"))
	}
	// A comment and a status change keep their old text.
	cm := domain.NormalizedEvent{EventType: domain.EventCommandPosted, ExternalID: "t1", Content: "xin chào"}
	if got := FormatNotice(cm); !strings.Contains(got, "Comment") || !strings.Contains(got, "xin chào") {
		t.Fatalf("comment notice %q", got)
	}
}

// TC-TD-NT-1 (REQ-1): a notice to General carries one callback button whose text and data come from the hub;
// without a button, or with data over Telegram's limit, no keyboard is sent.
func TestP10Notice_ButtonInReplyMarkup(t *testing.T) {
	c, bodies := p10NoticeServer(t)
	e := domain.NormalizedEvent{ExternalID: "86abc", Content: "chào", Metadata: map[string]string{}}
	if _, err := c.SendNotice(context.Background(), p10Target, e, &relay.NoticeButton{Text: "📌 Mở topic task", Data: "tt:86abc"}); err != nil {
		t.Fatal(err)
	}
	kb, _ := (*bodies)[0]["reply_markup"].(map[string]any)
	rows, _ := kb["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("keyboard %v", (*bodies)[0]["reply_markup"])
	}
	btn := rows[0].([]any)[0].(map[string]any)
	if btn["text"] != "📌 Mở topic task" || btn["callback_data"] != "tt:86abc" {
		t.Fatalf("button %v", btn)
	}
	for name, b := range map[string]*relay.NoticeButton{
		"nil":          nil,
		"empty data":   {Text: "x", Data: ""},
		"empty text":   {Text: "", Data: "tt:1"},
		"data too big": {Text: "x", Data: "tt:" + strings.Repeat("a", 62)},
	} {
		*bodies = nil
		if _, err := c.SendNotice(context.Background(), p10Target, e, b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, has := (*bodies)[0]["reply_markup"]; has {
			t.Errorf("%s: a keyboard was sent", name)
		}
	}
	// The plain Send path is unchanged: no keyboard.
	*bodies = nil
	if _, err := c.Send(context.Background(), p10Target, e); err != nil {
		t.Fatal(err)
	}
	if _, has := (*bodies)[0]["reply_markup"]; has {
		t.Fatal("Send added a keyboard")
	}
}
