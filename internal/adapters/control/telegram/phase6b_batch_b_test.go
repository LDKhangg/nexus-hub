package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 6b batch B: URL buttons on inline keyboards (REQ-8). Fake Bot API.

func b1Keyboard(t *testing.T, body map[string]any) []any {
	t.Helper()
	markup, ok := body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_markup in %v", body)
	}
	kb, _ := markup["inline_keyboard"].([]any)
	return kb
}

// TC-B1-50 (REQ-8): callback buttons and URL buttons serialize to the Bot API
// shape: a callback button has callback_data only, a URL button url only.
func TestPhase6bB1_ButtonJSONShape(t *testing.T) {
	api := &p6bAPI{}
	c := api.client(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{"message_id":7}}` })
	_, err := c.SendMessage(context.Background(), "ws", "ref", "-1001234567890", "x", MessageOptions{Buttons: [][]Button{
		{{Text: "Làm", Data: "do:1"}},
		{{Text: "Mở topic", URL: "https://t.me/c/1234567890/55"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	kb := b1Keyboard(t, api.calls[0].body)
	if len(kb) != 2 {
		t.Fatalf("rows=%v", kb)
	}
	cb := kb[0].([]any)[0].(map[string]any)
	if cb["callback_data"] != "do:1" || cb["text"] != "Làm" {
		t.Fatalf("callback button=%v", cb)
	}
	if _, has := cb["url"]; has {
		t.Fatalf("callback button has a url: %v", cb)
	}
	link := kb[1].([]any)[0].(map[string]any)
	if link["url"] != "https://t.me/c/1234567890/55" || link["text"] != "Mở topic" {
		t.Fatalf("url button=%v", link)
	}
	if _, has := link["callback_data"]; has {
		t.Fatalf("url button has callback_data (omitempty lost): %v", link)
	}
	raw, _ := json.Marshal(inlineButton{Text: "a", CallbackData: "b"})
	if strings.Contains(string(raw), "url") {
		t.Fatalf("inlineButton JSON=%s", raw)
	}
}

// TC-B1-51 (REQ-8, abuse): only https://t.me/ URL buttons are accepted; any
// other link, a mixed button or an oversize one is rejected before any request.
func TestPhase6bB1_URLButtonValidation(t *testing.T) {
	bad := map[string]Button{
		"http scheme":       {Text: "x", URL: "http://t.me/c/1/2"},
		"other host":        {Text: "x", URL: "https://example.com/c/1/2"},
		"look-alike host":   {Text: "x", URL: "https://t.me.evil.example/c/1/2"},
		"embedded t.me":     {Text: "x", URL: "https://evil.example/?u=https://t.me/c/1/2"},
		"userinfo host":     {Text: "x", URL: "https://t.me@evil.example/c/1/2"},
		"javascript":        {Text: "x", URL: "javascript:alert(1)"},
		"tg scheme":         {Text: "x", URL: "tg://resolve?domain=x"},
		"upper-case scheme": {Text: "x", URL: "HTTPS://t.me/c/1/2"},
		"leading space":     {Text: "x", URL: " https://t.me/c/1/2"},
		"both data and url": {Text: "x", Data: "d", URL: "https://t.me/c/1/2"},
		"empty text":        {Text: "", URL: "https://t.me/c/1/2"},
		"text too long":     {Text: strings.Repeat("a", 65), URL: "https://t.me/c/1/2"},
		"url too long":      {Text: "x", URL: "https://t.me/" + strings.Repeat("a", 500)},
		"invalid utf8 url":  {Text: "x", URL: "https://t.me/\xff"},
		"empty button":      {Text: "x"},
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			api := &p6bAPI{}
			c := api.client(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{"message_id":7}}` })
			_, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "x", MessageOptions{Buttons: [][]Button{{b}}})
			if ErrorCode(err) != "invalid_button" || len(api.calls) != 0 {
				t.Fatalf("err=%v calls=%d", err, len(api.calls))
			}
		})
	}
	// A bad button in a later row still rejects the whole keyboard.
	api := &p6bAPI{}
	c := api.client(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{"message_id":7}}` })
	_, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "x", MessageOptions{Buttons: [][]Button{
		{{Text: "ok", URL: "https://t.me/c/1/2"}}, {{Text: "bad", URL: "https://example.com"}}}})
	if ErrorCode(err) != "invalid_button" || len(api.calls) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(api.calls))
	}
	// Boundary: a 64-unit text and a 512-byte URL are accepted.
	ok := Button{Text: strings.Repeat("a", 64), URL: "https://t.me/" + strings.Repeat("a", 512-len("https://t.me/"))}
	if _, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "x", MessageOptions{Buttons: [][]Button{{ok}}}); err != nil {
		t.Fatalf("boundary button rejected: %v", err)
	}
}

// TC-B1-52 (REQ-8): the Messenger passes the URL through, and a callback
// button with a long payload is still rejected as before.
func TestPhase6bB1_MessengerButtonConversion(t *testing.T) {
	got := buttons([][]behavior.Button{{{Text: "a", Data: "d"}, {Text: "b", URL: "https://t.me/c/1/2"}}})
	if len(got) != 1 || got[0][0] != (Button{Text: "a", Data: "d"}) || got[0][1] != (Button{Text: "b", URL: "https://t.me/c/1/2"}) {
		t.Fatalf("buttons=%+v", got)
	}
	api := &p6bAPI{}
	c := api.client(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{"message_id":7}}` })
	_, err := c.SendMessage(context.Background(), "ws", "ref", "-100", "x", MessageOptions{Buttons: [][]Button{{{Text: "a", Data: strings.Repeat("d", MaxCallbackData+1)}}}})
	if ErrorCode(err) != "invalid_button" {
		t.Fatalf("long callback data: %v", err)
	}
}
