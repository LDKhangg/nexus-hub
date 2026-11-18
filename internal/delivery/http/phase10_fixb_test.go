package http

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 10 Fix B regressions (review B R-B-1 and R-B-3, commits b317aa1 and
// 7c49cfc): the Telegram handler's routing of replies to a message that shows
// a ClickUp comment, and of "/comment <text>". Fakes only.

// fixbLinks is a comment link reader: one linked message, or an error.
type fixbLinks struct {
	mu     sync.Mutex
	linked map[int64]behavior.CommentLink
	err    error
}

func (l *fixbLinks) LookupCommentLink(_ context.Context, ws, integration string, chat, msg int64) (behavior.CommentLink, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return behavior.CommentLink{}, l.err
	}
	c, ok := l.linked[msg]
	if !ok {
		return behavior.CommentLink{}, behavior.ErrNotFound
	}
	c.WorkspaceID, c.IntegrationID, c.ChatID = ws, integration, chat
	return c, nil
}

func fixbHandler(links *fixbLinks) (*TelegramHandler, *telegramInboxFake, *assistantStagerFake) {
	h, inbox, _ := p6bHandler(map[string]any{"owner_chat_id": float64(-100777)}, 77, 42)
	stager := &assistantStagerFake{}
	h.WithAssistant(stager)
	h.WithReplyTasks(behavior.ReplyTasks{Links: links})
	return h, inbox, stager
}

func fixbSay(id, chat, from, thread, replyTo int64, text string) string {
	th, rp := "", ""
	if thread != 0 {
		th = `,"message_thread_id":` + itoa(thread) + `,"is_topic_message":true`
	}
	if replyTo != 0 {
		rp = `,"reply_to_message":{"message_id":` + itoa(replyTo) + `}`
	}
	return `{"update_id":` + itoa(id) + `,"message":{"message_id":31` + th + rp + `,"chat":{"id":` + itoa(chat) +
		`},"from":{"id":` + itoa(from) + `},"text":"` + text + `"}}`
}

const fixbGroup, fixbOwner, fixbThread = int64(-100777), int64(42), int64(55)

// R-B-1: a plain reply to a linked comment message is staged for the assistant, never a note.
func TestP10FixB_Handler_ReplyToLinkedCommentGoesToAssistant(t *testing.T) {
	links := &fixbLinks{linked: map[int64]behavior.CommentLink{9: {MessageID: 9, TaskID: "86abc", CommentID: "1234"}}}
	h, inbox, stager := fixbHandler(links)
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 9, "cảm ơn, mình sẽ xem"))
	if len(inbox.updates) != 1 {
		t.Fatalf("updates %+v", inbox.updates)
	}
	u := inbox.updates[0]
	if u.Kind != behavior.UpdateText || u.Note != "" || !strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) || u.ReplyToMessageID != 9 {
		t.Fatalf("stored %+v", u)
	}
	if strings.Contains(u.Args, "cảm ơn") || len(stager.texts) != 1 || stager.texts[0] != "cảm ơn, mình sẽ xem" {
		t.Fatalf("text must be staged in memory only: args %q staged %q", u.Args, stager.texts)
	}
}

// R-B-1: a reply to an unlinked message keeps today's behaviour (the "no open reply" note).
func TestP10FixB_Handler_ReplyToUnlinkedKeepsNote(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{linked: map[int64]behavior.CommentLink{9: {MessageID: 9, TaskID: "86abc", CommentID: "1234"}}})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 10, "trả lời một tin khác"))
	if len(inbox.updates) != 1 || inbox.updates[0].Note != behavior.NoteNoOpenReply || len(stager.texts) != 0 || inbox.updates[0].Args != "" {
		t.Fatalf("updates %+v staged %q", inbox.updates, stager.texts)
	}
}

// R-B-1 (abuse): a link row with an unusable comment id never routes the reply to the assistant.
func TestP10FixB_Handler_UnusableLinkKeepsNote(t *testing.T) {
	for name, c := range map[string]behavior.CommentLink{
		"non numeric comment id": {MessageID: 9, TaskID: "86abc", CommentID: "abc"},
		"empty comment id":       {MessageID: 9, TaskID: "86abc"},
		"bad task id":            {MessageID: 9, TaskID: "86 abc", CommentID: "12"},
	} {
		t.Run(name, func(t *testing.T) {
			h, inbox, stager := fixbHandler(&fixbLinks{linked: map[int64]behavior.CommentLink{9: c}})
			p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 9, "ok"))
			if len(inbox.updates) != 1 || inbox.updates[0].Note != behavior.NoteNoOpenReply || len(stager.texts) != 0 {
				t.Fatalf("updates %+v staged %q", inbox.updates, stager.texts)
			}
		})
	}
}

// R-B-1: a failed link lookup is a retryable error: nothing is stored or staged, so Telegram delivers it again.
func TestP10FixB_Handler_LinkLookupErrorStoresNothing(t *testing.T) {
	links := &fixbLinks{err: errors.New("db down")}
	h, inbox, stager := fixbHandler(links)
	body := []byte(fixbSay(1, fixbGroup, fixbOwner, fixbThread, 9, "trả lời"))
	if _, err := h.HandleUpdate(context.Background(), testIntegrationID, body); !errors.Is(err, ErrUpdateUnavailable) {
		t.Fatalf("err %v", err)
	}
	if len(inbox.updates) != 0 || len(stager.texts) != 0 {
		t.Fatalf("stored %+v staged %q", inbox.updates, stager.texts)
	}
	// The retry after the store is back works and routes to the assistant.
	links.mu.Lock()
	links.err, links.linked = nil, map[int64]behavior.CommentLink{9: {MessageID: 9, TaskID: "86abc", CommentID: "77"}}
	links.mu.Unlock()
	if got, err := h.HandleUpdate(context.Background(), testIntegrationID, body); err != nil || got == "" {
		t.Fatalf("retry: %q %v", got, err)
	}
	if len(inbox.updates) != 1 || !strings.HasPrefix(inbox.updates[0].Args, behavior.AssistantTextPrefix) {
		t.Fatalf("retry stored %+v", inbox.updates)
	}
}

// R-B-1: a reply over 4000 runes to a linked comment is refused with the too-long note, nothing staged.
func TestP10FixB_Handler_LinkedReplyTooLong(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{linked: map[int64]behavior.CommentLink{9: {MessageID: 9, TaskID: "86abc", CommentID: "12"}}})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 9, strings.Repeat("é", 4001)))
	if len(inbox.updates) != 1 || inbox.updates[0].Note != behavior.NoteTextTooLong || len(stager.texts) != 0 {
		t.Fatalf("updates %+v staged %d", inbox.updates, len(stager.texts))
	}
}

// R-B-3: /comment <text> is staged in memory; the stored row carries only the key.
func TestP10FixB_Handler_CommentCommandStagesText(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 0, "/comment Đã sửa xong, nhờ anh Nam xem"))
	if len(inbox.updates) != 1 {
		t.Fatalf("updates %+v", inbox.updates)
	}
	u := inbox.updates[0]
	if u.Kind != behavior.UpdateCommand || u.Command != "/comment" || u.Note != "" || !strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) || u.ThreadID != fixbThread {
		t.Fatalf("stored %+v", u)
	}
	if strings.Contains(u.Args, "Nam") || len(stager.texts) != 1 || stager.texts[0] != "Đã sửa xong, nhờ anh Nam xem" {
		t.Fatalf("args %q staged %q", u.Args, stager.texts)
	}
}

// R-B-3: the bot-suffixed spelling is the same command, and a reply keeps its reply id.
func TestP10FixB_Handler_CommentCommandVariants(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 9, "/Comment@nexus_bot nội dung"))
	if len(inbox.updates) != 1 || inbox.updates[0].Command != "/comment" || inbox.updates[0].ReplyToMessageID != 9 ||
		len(stager.texts) != 1 || stager.texts[0] != "nội dung" {
		t.Fatalf("updates %+v staged %q", inbox.updates, stager.texts)
	}
}

// R-B-3: /comment without text stages nothing (the worker answers the usage line).
func TestP10FixB_Handler_CommentCommandWithoutText(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 0, "/comment"))
	p6bPost(t, h, p7Integration(), fixbSay(2, fixbGroup, fixbOwner, fixbThread, 0, "/comment    "))
	if len(inbox.updates) != 2 || len(stager.texts) != 0 {
		t.Fatalf("updates %+v staged %q", inbox.updates, stager.texts)
	}
	for _, u := range inbox.updates {
		if u.Command != "/comment" || u.Args != "" || u.Note != "" {
			t.Fatalf("stored %+v", u)
		}
	}
}

// R-B-3: /comment over 4000 runes gets the too-long note; exactly 4000 is staged.
func TestP10FixB_Handler_CommentCommandTooLong(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 0, "/comment "+strings.Repeat("é", 4001)))
	if len(inbox.updates) != 1 || inbox.updates[0].Note != behavior.NoteTextTooLong || len(stager.texts) != 0 || inbox.updates[0].Args != "" {
		t.Fatalf("updates %+v staged %d", inbox.updates, len(stager.texts))
	}
	p6bPost(t, h, p7Integration(), fixbSay(2, fixbGroup, fixbOwner, fixbThread, 0, "/comment "+strings.Repeat("é", 4000)))
	if len(inbox.updates) != 2 || inbox.updates[1].Note != "" || len(stager.texts) != 1 {
		t.Fatalf("at the limit: %+v staged %d", inbox.updates, len(stager.texts))
	}
}

// R-B-3 (abuse): without an assistant /comment text is not stored in the row either.
func TestP10FixB_Handler_CommentCommandWithoutAssistantKeepsTextOut(t *testing.T) {
	h, inbox, _ := p6bHandler(map[string]any{"owner_chat_id": float64(fixbGroup)}, 77, fixbOwner)
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, fixbOwner, fixbThread, 0, "/comment bí mật của tôi"))
	if len(inbox.updates) != 1 || inbox.updates[0].Args != "" {
		t.Fatalf("the text reached the update row: %+v", inbox.updates)
	}
}

// R-B-3: a stranger's /comment is dropped before anything is staged or stored.
func TestP10FixB_Handler_CommentCommandNonOwner(t *testing.T) {
	h, inbox, stager := fixbHandler(&fixbLinks{})
	p6bPost(t, h, p7Integration(), fixbSay(1, fixbGroup, 4242, fixbThread, 0, "/comment xin chào"))
	p6bPost(t, h, p7Integration(), fixbSay(2, 77, fixbOwner, fixbThread, 0, "/comment xin chào"))
	if len(inbox.updates) != 0 || len(stager.texts) != 0 {
		t.Fatalf("stored %+v staged %q", inbox.updates, stager.texts)
	}
}
