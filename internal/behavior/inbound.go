package behavior

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// This file is the contract between the Telegram inbound worker and the
// parts of the engine that act on what the owner sends: commands that start
// behaviors, /trace, /undo, button presses and free-text answers. The worker
// knows no behavior; it only routes.

// Errors an inbound handler may return. The worker turns each into a short
// Vietnamese reply.
var (
	// ErrNotAvailable: the feature is not wired yet.
	ErrNotAvailable = errors.New("behavior: not available")
	// ErrUnknownCommand: no built-in command or enabled behavior takes it.
	ErrUnknownCommand = errors.New("behavior: unknown command")
)

// UsageError says what to send instead. Usage is a short Vietnamese line
// such as "Cú pháp: /trace <run id hoặc task> [item]".
type UsageError struct{ Usage string }

func (e *UsageError) Error() string { return "behavior: bad arguments" }

// CommandRequest is a command from the owner that is not built in.
type CommandRequest struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	UpdateID      int64  // the Telegram update_id; each update is one run revision
	Command       string // "/name", lower case
	Args          string // trimmed, at most 512 characters
	Actor         string // "telegram:<user id>"
	CorrelationID string
}

// CommandTriggers starts the enabled behaviors whose telegram.command
// trigger matches. It returns a short reply for the owner, or
// ErrUnknownCommand, ErrNotAvailable or a *UsageError.
type CommandTriggers interface {
	TriggerCommand(ctx context.Context, r CommandRequest) (reply string, err error)
}

// TraceRequest asks for a run's trace: Ref is a run ID or a subject (for
// example a ClickUp task); Item is an optional item key.
type TraceRequest struct {
	WorkspaceID   string
	Ref           string
	Item          string
	CorrelationID string
}

// Tracer renders a short trace for Telegram (no secrets, no context text).
type Tracer interface {
	Trace(ctx context.Context, r TraceRequest) (reply string, err error)
}

// UndoRequest asks to undo one run's writes.
type UndoRequest struct {
	WorkspaceID   string
	RunID         int64
	Actor         string
	CorrelationID string
}

// Undoer undoes a run when nothing changed since.
type Undoer interface {
	Undo(ctx context.Context, r UndoRequest) (reply string, err error)
}

// CallbackKind is the action an inline button encodes. The set is closed.
type CallbackKind string

const (
	CallbackApprove  CallbackKind = "a" // approve / post a draft / confirm
	CallbackReject   CallbackKind = "r" // reject / skip ("Bỏ qua")
	CallbackOption   CallbackKind = "o" // choose suggested option N
	CallbackFreeText CallbackKind = "t" // ask for a free-text answer ("Trả lời khác")
	CallbackRateUp   CallbackKind = "u" // feedback 👍
	CallbackRateDown CallbackKind = "d" // feedback 👎 (+ reason as free text)
)

// MaxCallbackData is Telegram's limit for callback data, in bytes.
const MaxCallbackData = 64

// MaxCallbackOption is the highest option index a button can carry.
const MaxCallbackOption = 99

// Decides reports whether the kind takes the approval's decision. Only
// those need the approval to be open and unexpired; feedback works on a
// decided approval too.
func (k CallbackKind) Decides() bool {
	switch k {
	case CallbackApprove, CallbackReject, CallbackOption, CallbackFreeText:
		return true
	}
	return false
}

func (k CallbackKind) valid() bool {
	return k.Decides() || k == CallbackRateUp || k == CallbackRateDown
}

// CallbackData is decoded button data: "<kind>:<nonce>[:<option>]".
type CallbackData struct {
	Kind   CallbackKind
	Nonce  string
	Option int // -1 when none; set (0..MaxCallbackOption) only for CallbackOption
}

var callbackNonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,58}$`)

// Encode returns the button data, or an error when it would not fit the
// Telegram limit or is malformed.
func (d CallbackData) Encode() (string, error) {
	if !d.Kind.valid() || !callbackNonceRe.MatchString(d.Nonce) {
		return "", ErrInvalid
	}
	s := string(d.Kind) + ":" + d.Nonce
	switch {
	case d.Kind == CallbackOption:
		if d.Option < 0 || d.Option > MaxCallbackOption {
			return "", ErrInvalid
		}
		s += ":" + strconv.Itoa(d.Option)
	case d.Option >= 0:
		return "", ErrInvalid
	}
	if len(s) > MaxCallbackData {
		return "", ErrInvalid
	}
	return s, nil
}

// ParseCallbackData decodes button data strictly; anything else is
// ErrInvalid.
func ParseCallbackData(s string) (CallbackData, error) {
	if len(s) > MaxCallbackData {
		return CallbackData{}, ErrInvalid
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return CallbackData{}, ErrInvalid
	}
	d := CallbackData{Kind: CallbackKind(parts[0]), Nonce: parts[1], Option: -1}
	if !d.Kind.valid() || !callbackNonceRe.MatchString(d.Nonce) {
		return CallbackData{}, ErrInvalid
	}
	if (d.Kind == CallbackOption) != (len(parts) == 3) {
		return CallbackData{}, ErrInvalid
	}
	if len(parts) == 3 {
		p := parts[2]
		if p == "" || len(p) > 2 || (len(p) > 1 && p[0] == '0') {
			return CallbackData{}, ErrInvalid
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > MaxCallbackOption {
			return CallbackData{}, ErrInvalid
		}
		d.Option = n
	}
	return d, nil
}

// NewNonce returns a random approval nonce of 32 URL-safe characters, short
// enough for callback data with an option index.
func NewNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CallbackAction is a button press on an approval's message.
type CallbackAction struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	MessageID     int64 // the message that carries the buttons
	Data          CallbackData
	// Approval is the approval the nonce names, as read before the call.
	// For a deciding kind it was open and unexpired then; the handler must
	// still decide through ApprovalStore.Decide, which is atomic.
	Approval      Approval
	Actor         string // "telegram:<user id>"
	CorrelationID string
}

// CallbackResult is what the owner sees after a press.
type CallbackResult struct {
	Toast         string // short text on the button answer
	RemoveButtons bool   // remove the inline keyboard of the pressed message
	Reply         string // optional message to send to the chat
}

// FreeTextAnswer is the owner's Telegram reply to a message whose approval
// is in free-text mode.
type FreeTextAnswer struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	Approval      Approval // open and unexpired when read
	Answer        string   // at most 4000 characters
	Actor         string
	CorrelationID string
}

// CallbackHandler acts on button presses and free-text answers. Errors:
// ErrDecided or ErrExpired (already handled), ErrNotAvailable, or any other
// error (reported as a failure).
type CallbackHandler interface {
	HandleCallback(ctx context.Context, a CallbackAction) (CallbackResult, error)
	HandleFreeText(ctx context.Context, a FreeTextAnswer) (reply string, err error)
}

// ChatRef addresses a Telegram chat through one integration (bot).
type ChatRef struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
}

// Button is one inline button; Data comes from CallbackData.Encode.
type Button struct {
	Text string
	Data string
}

// OutMessage is a plain-text message with optional inline buttons.
type OutMessage struct {
	Text       string
	Buttons    [][]Button // one slice per row
	ReplyTo    int64      // 0: not a reply
	ForceReply bool       // open a reply box (for free-text mode)
}

// Messenger sends to Telegram. Errors carry safe codes only.
type Messenger interface {
	SendMessage(ctx context.Context, to ChatRef, m OutMessage) (messageID int64, err error)
	RemoveButtons(ctx context.Context, to ChatRef, messageID int64) error
	AnswerCallback(ctx context.Context, to ChatRef, callbackID, text string) error
}

// Unavailable implements CommandTriggers, Tracer, Undoer and
// CallbackHandler with ErrNotAvailable, so the binary works before the
// engine parts are wired.
type Unavailable struct{}

func (Unavailable) TriggerCommand(context.Context, CommandRequest) (string, error) {
	return "", ErrNotAvailable
}
func (Unavailable) Trace(context.Context, TraceRequest) (string, error) { return "", ErrNotAvailable }
func (Unavailable) Undo(context.Context, UndoRequest) (string, error)   { return "", ErrNotAvailable }
func (Unavailable) HandleCallback(context.Context, CallbackAction) (CallbackResult, error) {
	return CallbackResult{}, ErrNotAvailable
}
func (Unavailable) HandleFreeText(context.Context, FreeTextAnswer) (string, error) {
	return "", ErrNotAvailable
}
