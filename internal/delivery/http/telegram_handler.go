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
	ownerChatID  int64
	ownerUserID  int64
	now          func() time.Time
	ignored      atomic.Int64
}

func NewTelegramHandler(integrations WebhookIntegrationLookup, resolver SecretResolver, inbox behavior.InboxStore, answers AnswerStager, ownerChatID int64) *TelegramHandler {
	return &TelegramHandler{integrations: integrations, secrets: resolver, inbox: inbox, answers: answers, ownerChatID: ownerChatID, now: time.Now}
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
	MessageID int64   `json:"message_id"`
	Chat      tgChat  `json:"chat"`
	From      *tgUser `json:"from"`
	Text      *string `json:"text"`
	ReplyTo   *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_to_message"`
}

type tgCallback struct {
	ID      string `json:"id"`
	From    tgUser `json:"from"`
	Message *struct {
		MessageID int64  `json:"message_id"`
		Chat      tgChat `json:"chat"`
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
	var upd tgUpdate
	if json.Unmarshal(raw, &upd) != nil || upd.UpdateID <= 0 {
		return respond(http.StatusBadRequest, "invalid update")
	}
	u, owner, err := h.normalize(ctx, integration.WorkspaceID, upd)
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	if !owner {
		n := h.ignored.Add(1)
		log.Printf("telegram webhook: non-owner update ignored integration=%s chat=%d count=%d", id, u.ChatID, n)
		return ok("ignored")
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
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	if !created {
		return ok("duplicate")
	}
	return ok("queued")
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

// isOwner applies the owner rule (see TelegramHandler).
func (h *TelegramHandler) isOwner(chatID, fromID int64) bool {
	owner := h.ownerChatID
	if h.ownerUserID != 0 {
		owner = h.ownerUserID
	}
	return h.ownerChatID != 0 && chatID == h.ownerChatID && fromID == owner
}

// normalize keeps only the fields the worker needs. It returns false for an
// update that must have no effect (not the owner's). An update of another
// type carries no owner to check; it is kept as UpdateOther, which the
// worker finishes without any effect, so its update_id is deduplicated.
func (h *TelegramHandler) normalize(ctx context.Context, workspaceID string, upd tgUpdate) (behavior.InboundUpdate, bool, error) {
	switch {
	case upd.CallbackQuery != nil:
		q := upd.CallbackQuery
		if q.Message == nil {
			return behavior.InboundUpdate{}, false, nil // inline-mode presses are not ours
		}
		u := behavior.InboundUpdate{ChatID: q.Message.Chat.ID, Kind: behavior.UpdateCallback}
		if !h.isOwner(q.Message.Chat.ID, q.From.ID) {
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
		u := behavior.InboundUpdate{ChatID: m.Chat.ID, Kind: behavior.UpdateOther}
		if m.From == nil || !h.isOwner(m.Chat.ID, m.From.ID) {
			return u, false, nil
		}
		if m.Text == nil {
			return u, true, nil
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
			if utf8.RuneCountInString(args) > 512 {
				u.Note = behavior.NoteArgsTooLong
				return u, true, nil
			}
			u.Args = args
			return u, true, nil
		}
		u.Kind = behavior.UpdateText
		if m.ReplyTo == nil || m.ReplyTo.MessageID <= 0 || text == "" {
			return u, true, nil
		}
		if utf8.RuneCountInString(text) > 4000 {
			u.Note = behavior.NoteTextTooLong
			return u, true, nil
		}
		// The answer is staged on the approval, never in the update row.
		nonce, err := h.answers.StageAnswer(ctx, workspaceID, m.ReplyTo.MessageID, text, h.now())
		if errors.Is(err, behavior.ErrNotFound) {
			u.Note = behavior.NoteNoOpenReply
			return u, true, nil
		}
		if err != nil {
			return u, true, err
		}
		u.Args = nonce
		return u, true, nil
	}
	return behavior.InboundUpdate{Kind: behavior.UpdateOther}, true, nil
}
