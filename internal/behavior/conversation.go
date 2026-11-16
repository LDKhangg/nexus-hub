package behavior

import (
	"context"
	"log"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type conversationContextKey struct{}

type conversationContext struct {
	id   int64
	kind string
}

func messageKind(kind UpdateKind) string {
	switch kind {
	case UpdateCommand:
		return "command"
	case UpdateCallback:
		return "callback"
	default:
		return "text"
	}
}

func withConversation(ctx context.Context, id int64, kind UpdateKind) context.Context {
	return context.WithValue(ctx, conversationContextKey{}, conversationContext{id: id, kind: messageKind(kind)})
}

func conversationFromContext(ctx context.Context) conversationContext {
	state, _ := ctx.Value(conversationContextKey{}).(conversationContext)
	return state
}

func (w *InboxWorker) ensureConversation(ctx context.Context, to ChatRef) (workflow.Conversation, error) {
	if w.d.Conversations == nil {
		return workflow.Conversation{}, nil
	}
	return w.d.Conversations.EnsureConversation(ctx, workflow.Conversation{
		WorkspaceID: to.WorkspaceID, IntegrationID: to.IntegrationID, ChatID: to.ChatID, ThreadID: to.ThreadID,
	})
}

func (w *InboxWorker) recordInboundConversation(ctx context.Context, u InboundUpdate, to ChatRef) context.Context {
	if w.d.Conversations == nil {
		return withConversation(ctx, 0, u.Kind)
	}
	conversation, err := w.ensureConversation(ctx, to)
	if err != nil {
		log.Print("telegram conversation: ensure failed")
		return withConversation(ctx, 0, u.Kind)
	}
	ctx = withConversation(ctx, conversation.ID, u.Kind)
	if u.MessageID <= 0 || (u.Kind != UpdateText && u.Kind != UpdateCommand && u.Kind != UpdateCallback) {
		return ctx
	}
	_, err = w.d.Conversations.RecordMessage(ctx, workflow.Message{
		WorkspaceID: u.WorkspaceID, ConversationID: conversation.ID, Direction: "in", Kind: messageKind(u.Kind),
		TelegramMessageID: u.MessageID, ReplyToMessageID: u.ReplyToMessageID,
	})
	if err != nil {
		log.Print("telegram conversation: inbound metadata record failed")
	}
	return ctx
}

func (w *InboxWorker) recordOutboundConversation(ctx context.Context, to ChatRef, messageID, replyToMessageID int64) {
	if w.d.Conversations == nil || messageID <= 0 {
		return
	}
	state := conversationFromContext(ctx)
	if state.id == 0 {
		conversation, err := w.ensureConversation(ctx, to)
		if err != nil {
			log.Print("telegram conversation: outbound ensure failed")
			return
		}
		state.id = conversation.ID
	}
	if state.kind == "" {
		state.kind = "text"
	}
	_, err := w.d.Conversations.RecordMessage(ctx, workflow.Message{
		WorkspaceID: to.WorkspaceID, ConversationID: state.id, Direction: "out", Kind: state.kind,
		TelegramMessageID: messageID, ReplyToMessageID: replyToMessageID,
	})
	if err != nil {
		log.Print("telegram conversation: outbound metadata record failed")
	}
}
