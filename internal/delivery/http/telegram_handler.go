package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// TelegramSecretHeader carries the webhook secret token set with setWebhook.
const TelegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

// AnswerStager stages a free-text reply on the approval waiting for it.
type AnswerStager interface {
	StageAnswer(ctx context.Context, workspaceID string, messageID int64, answer string, now time.Time) (string, error)
}

// TemplateReplyStager keeps an owner template reply out of the generic
// Telegram update row and returns a short opaque reference for the worker.
type TemplateReplyStager interface {
	StageTemplateReply(ctx context.Context, workspaceID string, messageID int64, body string, now time.Time) (string, error)
}

// AssistantStager keeps the owner's plain free text in memory for the
// Telegram assistant and returns a short opaque key for the worker.
type AssistantStager interface {
	StageAssistantText(workspaceID, text string, now time.Time) (string, error)
}

// ReplyTaskLookup reports whether a replied-to message is a hub message
// about a ClickUp task (a notice). Such a reply, with no open approval or
// template prompt, goes to the assistant instead of "no open reply".
type ReplyTaskLookup interface {
	ReplyHasTask(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) bool
}

// ReplyCommentLookup is the optional part of a ReplyTaskLookup that knows
// the Telegram messages showing a ClickUp comment. An error makes Telegram
// deliver the update again, so a reply never becomes a note on a guess.
type ReplyCommentLookup interface {
	ReplyHasComment(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (bool, error)
}

// TelegramHandler receives Telegram updates for the owner. It verifies the
// secret token, keeps only the owner's updates, stores them once per
// update_id and answers at once; all work runs later from storage.
//
// Owner rule: an update has an effect only when its chat ID equals the
// owner_telegram_chat_id setting and its sender's user ID is the owner's.
// By default the owner's user ID is the chat ID, so only the owner's
// private chat with the bot is accepted. When owner_telegram_user_id is set
// (WithOwnerUser), the owner chat may be a group: updates in that chat are
// accepted only when sent by that user. Any other chat or sender is ignored
// (200, no reply, nothing stored). For a button press the chat is the chat
// of the message that carries the button.
type TelegramHandler struct {
	integrations WebhookIntegrationLookup
	secrets      SecretResolver
	inbox        behavior.InboxStore
	answers      AnswerStager
	templates    TemplateReplyStager
	assistant    AssistantStager
	replyTasks   ReplyTaskLookup
	ownerChatID  int64
	ownerUserID  int64
	now          func() time.Time
	ignored      atomic.Int64
}

func NewTelegramHandler(integrations WebhookIntegrationLookup, resolver SecretResolver, inbox behavior.InboxStore, answers AnswerStager, ownerChatID int64, templates ...TemplateReplyStager) *TelegramHandler {
	h := &TelegramHandler{integrations: integrations, secrets: resolver, inbox: inbox, answers: answers, ownerChatID: ownerChatID, now: time.Now}
	if len(templates) > 0 {
		h.templates = templates[0]
	}
	return h
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgMessage struct {
	MessageID       int64   `json:"message_id"`
	MessageThreadID int64   `json:"message_thread_id"`
	IsTopicMessage  bool    `json:"is_topic_message"`
	Chat            tgChat  `json:"chat"`
	From            *tgUser `json:"from"`
	Text            *string `json:"text"`
	ReplyTo         *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_to_message"`
}

type tgCallback struct {
	ID      string `json:"id"`
	From    tgUser `json:"from"`
	Message *struct {
		MessageID       int64  `json:"message_id"`
		MessageThreadID int64  `json:"message_thread_id"`
		IsTopicMessage  bool   `json:"is_topic_message"`
		Chat            tgChat `json:"chat"`
	} `json:"message"`
	Data *string `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

var (
	commandNameRe = regexp.MustCompile(`^/[a-z][a-z0-9_]{0,31}$`)
	callbackIDRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func (h *TelegramHandler) reject(c echo.Context, integrationID, reason string) error {
	log.Printf("telegram webhook: rejected integration=%s reason=%s", integrationID, reason)
	return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func (h *TelegramHandler) Handle(c echo.Context) error {
	respond := func(code int, message string) error { return c.JSON(code, map[string]string{"error": message}) }
	ok := func(status string) error { return c.JSON(http.StatusOK, map[string]string{"status": status}) }
	id := c.Param("integrationID")
	if !tenant.ValidUUID(id) {
		return respond(http.StatusBadRequest, "invalid integration")
	}
	ctx := c.Request().Context()
	integration, err := h.integrations.LookupWebhookIntegration(ctx, id)
	if errors.Is(err, tenant.ErrIntegrationNotFound) {
		return respond(http.StatusNotFound, "unknown integration")
	}
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	if tenant.ValidateTelegramControl(integration) != nil {
		return respond(http.StatusNotFound, "unknown integration")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodyBytes+1))
	if err != nil {
		if errors.Is(err, echo.ErrStatusRequestEntityTooLarge) {
			return respond(http.StatusRequestEntityTooLarge, "body too large")
		}
		return respond(http.StatusBadRequest, "invalid body")
	}
	if len(raw) > maxBodyBytes {
		return respond(http.StatusRequestEntityTooLarge, "body too large")
	}
	given := c.Request().Header.Get(TelegramSecretHeader)
	if given == "" {
		return h.reject(c, id, "missing_secret")
	}
	want, err := h.secrets.Resolve(ctx, integration.WorkspaceID, behavior.CredentialTelegramWebhookSecret)
	var se *secrets.Error
	switch {
	case err == nil && want != "":
	case err == nil || (errors.As(err, &se) && se.Code == secrets.CodeMissingCredentials):
		return h.reject(c, id, "secret_not_configured")
	default:
		reason := "resolve_failed"
		if se != nil {
			reason = se.Code
		}
		log.Printf("telegram webhook: secret unavailable integration=%s reason=%s", id, reason)
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(want)) != 1 {
		return h.reject(c, id, "wrong_secret")
	}
	switch status, err := h.process(ctx, integration, raw, "telegram webhook"); {
	case err != nil:
		return respond(http.StatusServiceUnavailable, "service unavailable")
	case status == UpdateInvalid:
		return respond(http.StatusBadRequest, "invalid update")
	default:
		return ok(status)
	}
}

// Results of HandleUpdate (and the webhook's 200 status values).
const (
	UpdateQueued    = "queued"    // stored for the inbox worker
	UpdateDuplicate = "duplicate" // this update_id was stored before
	UpdateIgnored   = "ignored"   // not the owner's: no effect, nothing stored
	UpdateInvalid   = "invalid"   // malformed: can never be stored
)

// ErrUpdateUnavailable means the update could not be handled now (storage,
// lookup or staging failed); the caller keeps it and tries again.
var ErrUpdateUnavailable = errors.New("telegram update: service unavailable")

// HandleUpdate handles one raw Telegram update received by long polling
// (getUpdates) for the Telegram control integration integrationID. After
// the integration lookup it runs exactly the webhook's path (owner rule,
// normalize, staging, people sighting, inbox insert deduplicated by
// update_id). It returns one of the Update* results, or an error wrapping
// ErrUpdateUnavailable when the update must be offered again later.
func (h *TelegramHandler) HandleUpdate(ctx context.Context, integrationID string, raw []byte) (string, error) {
	if !tenant.ValidUUID(integrationID) {
		return "", ErrUpdateUnavailable
	}
	integration, err := h.integrations.LookupWebhookIntegration(ctx, integrationID)
	if err != nil || tenant.ValidateTelegramControl(integration) != nil {
		return "", ErrUpdateUnavailable
	}
	if len(raw) > maxBodyBytes {
		return UpdateInvalid, nil
	}
	return h.process(ctx, integration, raw, "telegram poll")
}

// process is the shared part of the webhook and polling paths; label only
// prefixes its log lines.
func (h *TelegramHandler) process(ctx context.Context, integration tenant.WebhookIntegration, raw []byte, label string) (string, error) {
	var upd tgUpdate
	if json.Unmarshal(raw, &upd) != nil || upd.UpdateID <= 0 {
		return UpdateInvalid, nil
	}
	u, owner, err := h.normalize(ctx, integration, upd)
	if err != nil {
		return "", ErrUpdateUnavailable
	}
	if !owner {
		n := h.ignored.Add(1)
		log.Printf("%s: non-owner update ignored integration=%s chat=%d count=%d", label, integration.ID, u.ChatID, n)
		return UpdateIgnored, nil
	}
	if directory, ok := h.inbox.(people.Resolver); ok {
		if from := telegramSender(upd); from != nil {
			_ = directory.Sighting(ctx, people.Sighting{
				WorkspaceID:  integration.WorkspaceID,
				Platform:     "telegram",
				ExternalID:   strconv.FormatInt(from.ID, 10),
				PlatformName: strings.TrimSpace(from.FirstName + " " + from.LastName),
				Username:     from.Username,
				Source:       "auto",
				IsSelf:       true,
				SeenAt:       h.now(),
			})
		}
	}
	u.WorkspaceID, u.IntegrationID, u.UpdateID = integration.WorkspaceID, integration.ID, upd.UpdateID
	created, err := h.inbox.RecordUpdate(ctx, integration.WorkspaceID, u)
	if err != nil {
		return "", ErrUpdateUnavailable
	}
	if !created {
		return UpdateDuplicate, nil
	}
	return UpdateQueued, nil
}

func telegramSender(upd tgUpdate) *tgUser {
	if upd.CallbackQuery != nil {
		return &upd.CallbackQuery.From
	}
	if upd.Message != nil {
		return upd.Message.From
	}
	return nil
}

// WithOwnerUser sets the owner's Telegram user ID for an owner chat that is
// a group (owner_telegram_user_id). Zero or a negative ID keeps the default:
// the owner's user ID is the chat ID (a private chat).
func (h *TelegramHandler) WithOwnerUser(userID int64) *TelegramHandler {
	if userID > 0 {
		h.ownerUserID = userID
	}
	return h
}

// WithAssistant hands the owner's plain free text (not a reply) to the
// Telegram assistant through stager. Nil keeps plain text without content.
func (h *TelegramHandler) WithAssistant(stager AssistantStager) *TelegramHandler {
	h.assistant = stager
	return h
}

// WithReplyTasks lets a reply to a hub message about a ClickUp task reach
// the assistant (see ReplyTaskLookup). Nil keeps NoteNoOpenReply.
func (h *TelegramHandler) WithReplyTasks(lookup ReplyTaskLookup) *TelegramHandler {
	h.replyTasks = lookup
	return h
}

// stageDeep keeps the text of "/deep <text>" out of the update row: like
// plain text, it is staged in memory for the assistant and Args holds only
// its key. Without an assistant (or when staging fails) Args stays empty.
func (h *TelegramHandler) stageDeep(workspaceID string, u behavior.InboundUpdate, args string) behavior.InboundUpdate {
	if args == "" || h.assistant == nil {
		return u
	}
	if utf8.RuneCountInString(args) > 4000 {
		u.Note = behavior.NoteTextTooLong
		return u
	}
	key, err := h.assistant.StageAssistantText(workspaceID, args, h.now())
	if err != nil {
		return u
	}
	u.Args = behavior.AssistantTextPrefix + key
	return u
}

// isOwner applies the owner rule (see TelegramHandler).
//
// The owner chat is the integration's own group (owner_chat_id or chat_id in
// its config) when it has one, otherwise the global ownerChatID. The sender
// must be the owner user ID, or the chat ID itself (a private chat) when no
// owner user is set; a group chat ID never equals a sender, so a group needs
// owner_telegram_user_id.
func (h *TelegramHandler) isOwner(integration tenant.WebhookIntegration, chatID, fromID int64) bool {
	chat := tenant.TelegramOwnerChatID(integration.Config)
	if chat == 0 {
		chat = h.ownerChatID
	}
	owner := chat
	if h.ownerUserID != 0 {
		owner = h.ownerUserID
	}
	return chat != 0 && chatID == chat && fromID == owner
}

// normalize keeps only the fields the worker needs. It returns false for an
// update that must have no effect (not the owner's). An update of another
// type carries no owner to check; it is kept as UpdateOther, which the
// worker finishes without any effect, so its update_id is deduplicated.
func (h *TelegramHandler) normalize(ctx context.Context, integration tenant.WebhookIntegration, upd tgUpdate) (behavior.InboundUpdate, bool, error) {
	workspaceID := integration.WorkspaceID
	switch {
	case upd.CallbackQuery != nil:
		q := upd.CallbackQuery
		if q.Message == nil {
			return behavior.InboundUpdate{}, false, nil // inline-mode presses are not ours
		}
		u := behavior.InboundUpdate{ChatID: q.Message.Chat.ID, ThreadID: topicThread(q.Message.IsTopicMessage, q.Message.MessageThreadID), MessageID: q.Message.MessageID, Kind: behavior.UpdateCallback}
		if !h.isOwner(integration, q.Message.Chat.ID, q.From.ID) {
			return u, false, nil
		}
		if !callbackIDRe.MatchString(q.ID) {
			u.Kind, u.Note = behavior.UpdateOther, behavior.NoteBadCallback
			return u, true, nil
		}
		u.Args = q.ID + " " + strconv.FormatInt(q.Message.MessageID, 10)
		if q.Data == nil || len(*q.Data) > behavior.MaxCallbackData || !utf8.ValidString(*q.Data) {
			u.Note = behavior.NoteBadCallback
			return u, true, nil
		}
		u.Command = *q.Data
		return u, true, nil
	case upd.Message != nil:
		m := upd.Message
		u := behavior.InboundUpdate{ChatID: m.Chat.ID, ThreadID: topicThread(m.IsTopicMessage, m.MessageThreadID), MessageID: m.MessageID, Kind: behavior.UpdateOther}
		if m.From == nil || !h.isOwner(integration, m.Chat.ID, m.From.ID) {
			return u, false, nil
		}
		if m.Text == nil {
			return u, true, nil
		}
		// In a forum topic Telegram sets reply_to_message to the topic's
		// creation message for every message that is not an explicit reply.
		replyTo := int64(0)
		if m.ReplyTo != nil && m.ReplyTo.MessageID > 0 &&
			!(m.IsTopicMessage && m.ReplyTo.MessageID == m.MessageThreadID) {
			replyTo = m.ReplyTo.MessageID
		}
		if replyTo > 0 {
			u.ReplyToMessageID = replyTo
		}
		text := strings.TrimSpace(*m.Text)
		if strings.HasPrefix(text, "/") {
			u.Kind = behavior.UpdateCommand
			name, args := text, ""
			if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
				name, args = text[:i], text[i:]
			}
			name, _, _ = strings.Cut(strings.ToLower(name), "@")
			if !commandNameRe.MatchString(name) {
				u.Note = behavior.NoteBadCommand
				return u, true, nil
			}
			u.Command = name
			args = strings.TrimSpace(args)
			if name == "/deep" {
				return h.stageDeep(workspaceID, u, args), true, nil
			}
			if utf8.RuneCountInString(args) > 512 {
				u.Note = behavior.NoteArgsTooLong
				return u, true, nil
			}
			u.Args = args
			return u, true, nil
		}
		u.Kind = behavior.UpdateText
		if text == "" {
			return u, true, nil
		}
		// stageAssistant hands plain text to the assistant, staged in memory.
		stageAssistant := func() (behavior.InboundUpdate, bool, error) {
			if h.assistant == nil {
				return u, true, nil
			}
			if utf8.RuneCountInString(text) > 4000 {
				u.Note = behavior.NoteTextTooLong
				return u, true, nil
			}
			// The text stays in memory for the worker, never in the update row.
			key, err := h.assistant.StageAssistantText(workspaceID, text, h.now())
			if err != nil {
				return u, true, nil
			}
			u.Args = behavior.AssistantTextPrefix + key
			return u, true, nil
		}
		if replyTo == 0 {
			return stageAssistant()
		}
		// noOpenReply: a reply with no open approval or template prompt goes
		// to the assistant only when it replies to a hub message about a
		// ClickUp task or to a message showing a ClickUp comment; otherwise
		// it keeps the note.
		noOpenReply := func() (behavior.InboundUpdate, bool, error) {
			if h.assistant != nil && h.replyTasks != nil {
				if h.replyTasks.ReplyHasTask(ctx, workspaceID, integration.ID, m.Chat.ID, replyTo) {
					return stageAssistant()
				}
				if comments, ok := h.replyTasks.(ReplyCommentLookup); ok {
					linked, err := comments.ReplyHasComment(ctx, workspaceID, integration.ID, m.Chat.ID, replyTo)
					if err != nil {
						return u, true, err
					}
					if linked {
						return stageAssistant()
					}
				}
			}
			u.Note = behavior.NoteNoOpenReply
			return u, true, nil
		}
		if utf8.RuneCountInString(text) > 4000 {
			u.Note = behavior.NoteTextTooLong
			return u, true, nil
		}
		// The answer is staged on the approval, never in the update row.
		nonce, err := h.answers.StageAnswer(ctx, workspaceID, replyTo, text, h.now())
		if errors.Is(err, behavior.ErrNotFound) {
			if h.templates == nil {
				return noOpenReply()
			}
			nonce, err = h.templates.StageTemplateReply(ctx, workspaceID, replyTo, text, h.now())
			if errors.Is(err, reporting.ErrTemplatePromptNotFound) {
				return noOpenReply()
			}
			if err == nil {
				u.Args = "report-template:" + nonce
				return u, true, nil
			}
		}
		if err != nil {
			return u, true, err
		}
		u.Args = nonce
		return u, true, nil
	}
	return behavior.InboundUpdate{Kind: behavior.UpdateOther}, true, nil
}

// topicThread returns the forum topic of a message. In the Bot API,
// message_thread_id names a topic only when is_topic_message is true; a reply
// in a non-forum supergroup also carries a thread id (the reply-chain root),
// which must not open a conversation.
func topicThread(isTopic bool, threadID int64) int64 {
	if !isTopic || threadID <= 0 {
		return 0
	}
	return threadID
}
