package http

import (
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// TD-TT-7: in a forum topic Telegram sets reply_to_message to the topic's
// creation message for every message that is not an explicit reply. Plain
// text must route as plain text; real replies stay replies.
func TestTD_TT7_TopicRootIsNotAReply(t *testing.T) {
	const group, owner, topic = int64(-100777), int64(42), int64(55)
	cfg := map[string]any{"owner_chat_id": float64(group)}
	msg := func(id int64, flags string, replyTo int64, text string) string {
		reply := ""
		if replyTo != 0 {
			reply = `,"reply_to_message":{"message_id":` + itoa(replyTo) + `}`
		}
		return `{"update_id":` + itoa(id) + `,"message":{"message_id":9` + flags + `,"chat":{"id":-100777},"from":{"id":42},"text":"` + text + `"` + reply + `}}`
	}
	inTopic := `,"message_thread_id":55,"is_topic_message":true`
	setup := func(withAssistant bool) (*telegramInboxFake, *assistantStagerFake, func(string)) {
		h, inbox, in := p6bHandler(cfg, 77, owner)
		stager := &assistantStagerFake{}
		if withAssistant {
			h.WithAssistant(stager)
		}
		return inbox, stager, func(body string) { p6bPost(t, h, in, body) }
	}

	t.Run("plain text carrying the topic root reply goes to the assistant", func(t *testing.T) {
		inbox, stager, post := setup(true)
		post(msg(1, inTopic, topic, "đổi tên task giúp tôi"))
		if len(inbox.updates) != 1 {
			t.Fatalf("updates=%+v", inbox.updates)
		}
		u := inbox.updates[0]
		if u.Kind != behavior.UpdateText || !strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) || u.Note != "" || u.ReplyToMessageID != 0 || u.ThreadID != topic {
			t.Fatalf("update %+v", u)
		}
		if len(stager.texts) != 1 || stager.texts[0] != "đổi tên task giúp tôi" {
			t.Fatalf("staged %q", stager.texts)
		}
		if strings.Contains(u.Args+u.Note, "đổi tên") {
			t.Fatalf("text stored in the update: %+v", u)
		}
	})

	t.Run("a real reply inside the topic stays a reply", func(t *testing.T) {
		inbox, stager, post := setup(true)
		post(msg(2, inTopic, 77, "trả lời tin 77"))
		u := inbox.updates[0]
		if u.ReplyToMessageID != 77 || u.Note != behavior.NoteNoOpenReply || strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) || len(stager.texts) != 0 {
			t.Fatalf("update %+v staged=%q", u, stager.texts)
		}
	})

	t.Run("the topic number of another topic is a real reply", func(t *testing.T) {
		inbox, stager, post := setup(true)
		post(msg(3, inTopic, 56, "x"))
		u := inbox.updates[0]
		if u.ReplyToMessageID != 56 || len(stager.texts) != 0 {
			t.Fatalf("update %+v", u)
		}
	})

	t.Run("without is_topic_message the same id is a real reply", func(t *testing.T) {
		inbox, stager, post := setup(true)
		post(msg(4, `,"message_thread_id":55`, 55, "trả lời tin 55"))
		u := inbox.updates[0]
		if u.ReplyToMessageID != 55 || u.Note != behavior.NoteNoOpenReply || len(stager.texts) != 0 {
			t.Fatalf("update %+v", u)
		}
		inbox2, stager2, post2 := setup(true)
		post2(msg(5, ``, 55, "trả lời trong group thường"))
		if u := inbox2.updates[0]; u.ReplyToMessageID != 55 || len(stager2.texts) != 0 {
			t.Fatalf("non-topic update %+v", u)
		}
	})

	t.Run("a command in a topic with the root reply is a command without a reply id", func(t *testing.T) {
		inbox, _, post := setup(true)
		post(msg(6, inTopic, topic, "/status"))
		u := inbox.updates[0]
		if u.Kind != behavior.UpdateCommand || u.Command != "/status" || u.ReplyToMessageID != 0 || u.ThreadID != topic {
			t.Fatalf("update %+v", u)
		}
	})

	t.Run("reply ids of zero are plain text", func(t *testing.T) {
		inbox, stager, post := setup(true)
		post(`{"update_id":7,"message":{"message_id":9,"message_thread_id":55,"is_topic_message":true,"chat":{"id":-100777},"from":{"id":42},"text":"hi","reply_to_message":{"message_id":0}}}`)
		post(`{"update_id":8,"message":{"message_id":9,"message_thread_id":55,"is_topic_message":true,"chat":{"id":-100777},"from":{"id":42},"text":"hi again","reply_to_message":{"message_id":-3}}}`)
		if len(stager.texts) != 2 || inbox.updates[0].ReplyToMessageID != 0 || inbox.updates[1].ReplyToMessageID != 0 {
			t.Fatalf("staged=%q updates=%+v", stager.texts, inbox.updates)
		}
	})

	t.Run("without the assistant a root reply is plain text, not an unanswerable reply", func(t *testing.T) {
		inbox, _, post := setup(false)
		post(msg(9, inTopic, topic, "plain"))
		u := inbox.updates[0]
		if u.Note != "" || u.ReplyToMessageID != 0 {
			t.Fatalf("update %+v", u)
		}
		inbox2, _, post2 := setup(false)
		post2(msg(10, inTopic, 77, "real reply"))
		if u := inbox2.updates[0]; u.Note != behavior.NoteNoOpenReply || u.ReplyToMessageID != 77 {
			t.Fatalf("real reply without assistant: %+v", u)
		}
	})

	t.Run("a stranger's root reply stores no text", func(t *testing.T) {
		h, inbox, in := p6bHandler(cfg, 77, owner)
		h.WithAssistant(&assistantStagerFake{})
		p6bPost(t, h, in, `{"update_id":11,"message":{"message_id":9,"message_thread_id":55,"is_topic_message":true,"chat":{"id":-100777},"from":{"id":4242},"text":"SECRET-STRANGER","reply_to_message":{"message_id":55}}}`)
		for _, u := range inbox.updates {
			if strings.Contains(u.Args+u.Note+u.Command, "SECRET-STRANGER") || strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) {
				t.Fatalf("stranger text accepted: %+v", u)
			}
		}
	})
}
