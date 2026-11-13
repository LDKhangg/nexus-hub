package behavior

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/google/uuid"
)

// Short Vietnamese texts the inbound worker sends. They never carry a
// secret or a provider error.
const (
	msgStale        = "Đã xử lý hoặc đã hết hạn."
	msgNotAvailable = "Lệnh này chưa khả dụng."
	msgUnknown      = "Lệnh không rõ. Gửi /help để xem các lệnh."
	msgFailed       = "Không xử lý được, thử lại sau."
	msgNotFound     = "Không tìm thấy."
	msgReceived     = "Đã nhận."
	msgBadButton    = "Nút không hợp lệ."
	msgHint         = "Gửi /help để xem các lệnh."
	msgArgsTooLong  = "Tham số quá dài (tối đa 512 ký tự)."
	msgTextTooLong  = "Câu trả lời quá dài (tối đa 4000 ký tự). Hãy gửi ngắn hơn."
	msgNoOpenReply  = "Tin nhắn đó không chờ câu trả lời, hoặc đã hết hạn."
	msgNoAnswer     = "Không thấy câu trả lời, hãy gửi lại."
	usageTrace      = "Cú pháp: /trace <run id hoặc task> [item]"
	usageUndo       = "Cú pháp: /undo <run id>"
	usageAlias      = "Cú pháp: /alias <platform:id> <tên>"
)

// InboxSettings tunes the inbound worker.
type InboxSettings struct {
	PollInterval   time.Duration // idle wait between claims
	Lease          time.Duration // claim lease; longer than one update's work
	MaxAttempts    int           // claims before an update is given up
	RequestTimeout time.Duration // one Telegram call
	HandlerTimeout time.Duration // one engine handler call
}

// InboxDeps are the worker's collaborators. Nil handlers default to
// Unavailable.
type InboxDeps struct {
	Inbox     InboxStore
	Approvals ApprovalStore
	Defs      DefinitionStore
	Messenger Messenger
	Commands  CommandTriggers
	Tracer    Tracer
	Undoer    Undoer
	Callbacks CallbackHandler
	People    people.Directory
	Reports   ReportSettings
	// Assistant, when set, answers the owner's plain free text.
	Assistant *Assistant
}

// InboxWorker processes stored Telegram updates: built-in commands,
// behavior commands, button presses and free-text answers. Every update is
// finished once; an update is claimed again only when it failed before any
// handler ran, and at most MaxAttempts times.
type InboxWorker struct {
	d        InboxDeps
	settings InboxSettings
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
}

func NewInboxWorker(d InboxDeps, s InboxSettings) *InboxWorker {
	if s.PollInterval <= 0 {
		s.PollInterval = time.Second
	}
	if s.Lease <= 0 {
		s.Lease = 2 * time.Minute
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = 3
	}
	if s.RequestTimeout <= 0 {
		s.RequestTimeout = 10 * time.Second
	}
	if s.HandlerTimeout <= 0 {
		s.HandlerTimeout = 30 * time.Second
	}
	if d.Commands == nil {
		d.Commands = Unavailable{}
	}
	if d.Tracer == nil {
		d.Tracer = Unavailable{}
	}
	if d.Undoer == nil {
		d.Undoer = Unavailable{}
	}
	if d.Callbacks == nil {
		d.Callbacks = Unavailable{}
	}
	return &InboxWorker{d: d, settings: s, now: time.Now, wait: waitFor}
}

func waitFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run processes updates until ctx ends.
func (w *InboxWorker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		worked, err := w.processNext(ctx)
		if err != nil && ctx.Err() == nil {
			log.Print("telegram inbound: storage operation failed; pending work retained")
		}
		if worked && err == nil {
			continue
		}
		if err := w.wait(ctx, w.settings.PollInterval); err != nil {
			return nil
		}
	}
	return nil
}

// outcome is the result of one update: an error code ("" when fine) and
// whether it may be claimed again (only before any handler ran).
type outcome struct {
	code  string
	retry bool
}

func (w *InboxWorker) processNext(ctx context.Context) (bool, error) {
	u, err := w.d.Inbox.ClaimUpdate(ctx, w.now(), w.settings.Lease)
	if err != nil || u == nil {
		return false, err
	}
	corr := correlationID()
	var out outcome
	if u.Attempts > w.settings.MaxAttempts {
		out = outcome{code: "attempts_exhausted"}
	} else {
		workCtx, cancel := context.WithTimeout(ctx, w.settings.Lease-time.Second)
		out = w.dispatch(workCtx, *u, corr)
		cancel()
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if out.retry && u.Attempts < w.settings.MaxAttempts {
		log.Printf("telegram inbound: update=%d kind=%s code=%s attempt=%d corr=%s retry", u.ID, u.Kind, out.code, u.Attempts, corr)
		return true, nil // the lease ends and the update is claimed again
	}
	if err := w.d.Inbox.FinishUpdate(ctx, u.ID, u.ClaimToken, out.code); err != nil {
		return true, err
	}
	log.Printf("telegram inbound: update=%d kind=%s code=%s corr=%s", u.ID, u.Kind, out.code, corr)
	return true, nil
}

func correlationID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func (w *InboxWorker) dispatch(ctx context.Context, u InboundUpdate, corr string) outcome {
	if u.Kind == UpdateOther && u.Note == "" {
		return outcome{}
	}
	if u.IntegrationID == "" || u.ChatID == 0 {
		return outcome{code: "no_integration"}
	}
	to := ChatRef{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID}
	actor := "telegram:" + strconv.FormatInt(u.ChatID, 10)
	switch u.Kind {
	case UpdateCallback:
		if strings.HasPrefix(u.Command, "rp:") {
			return w.reportCallback(ctx, u, to)
		}
		return w.callback(ctx, u, to, actor, corr)
	case UpdateCommand, UpdateText, UpdateOther:
		if text, code := noteText(u.Note); text != "" {
			return w.reply(ctx, to, text, code)
		}
		if u.Kind == UpdateCommand {
			return w.command(ctx, u, to, actor, corr)
		}
		if u.Kind == UpdateText {
			return w.freeText(ctx, u, to, actor, corr)
		}
	}
	return outcome{}
}

func noteText(note string) (string, string) {
	switch note {
	case "":
		return "", ""
	case NoteArgsTooLong:
		return msgArgsTooLong, note
	case NoteTextTooLong:
		return msgTextTooLong, note
	case NoteBadCommand:
		return msgUnknown, note
	case NoteNoOpenReply:
		return msgNoOpenReply, note
	}
	return msgHint, note
}

// reply sends text; a failed send keeps the earlier code or reports
// reply_failed.
func (w *InboxWorker) reply(ctx context.Context, to ChatRef, text, code string) outcome {
	if w.d.Messenger == nil {
		return outcome{code: firstCode(code, "reply_failed")}
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	defer cancel()
	if _, err := w.d.Messenger.SendMessage(sendCtx, to, OutMessage{Text: text}); err != nil {
		return outcome{code: firstCode(code, "reply_failed")}
	}
	return outcome{code: code}
}

func firstCode(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// handlerText maps a handler result to the reply and an update code.
func handlerText(reply string, err error) (string, string) {
	var ue *UsageError
	switch {
	case err == nil:
		if strings.TrimSpace(reply) == "" {
			return msgReceived, ""
		}
		return reply, ""
	case errors.As(err, &ue) && strings.TrimSpace(ue.Usage) != "":
		return ue.Usage, "usage"
	case errors.Is(err, ErrNotAvailable):
		return msgNotAvailable, "not_available"
	case errors.Is(err, ErrUnknownCommand):
		return msgUnknown, "unknown_command"
	case errors.Is(err, ErrNotFound):
		return msgNotFound, "not_found"
	case errors.Is(err, ErrDecided), errors.Is(err, ErrExpired):
		return msgStale, "stale"
	}
	return msgFailed, "handler_failed"
}

func (w *InboxWorker) command(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	args := strings.TrimSpace(u.Args)
	var (
		reply string
		err   error
	)
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	defer cancel()
	switch u.Command {
	case "/help", "/start":
		return w.reply(ctx, to, w.helpText(hctx, u.WorkspaceID), "")
	case "/trace":
		// The first word names the run; the rest is an item key, which may
		// contain spaces.
		f := strings.Fields(args)
		if len(f) < 1 {
			return w.reply(ctx, to, usageTrace, "usage")
		}
		r := TraceRequest{WorkspaceID: u.WorkspaceID, Ref: f[0], CorrelationID: corr}
		if len(f) > 1 {
			r.Item = strings.Join(f[1:], " ")
		}
		reply, err = w.d.Tracer.Trace(hctx, r)
	case "/undo":
		f := strings.Fields(args)
		if len(f) != 1 {
			return w.reply(ctx, to, usageUndo, "usage")
		}
		id, perr := strconv.ParseInt(f[0], 10, 64)
		if perr != nil || id <= 0 {
			return w.reply(ctx, to, usageUndo, "usage")
		}
		reply, err = w.d.Undoer.Undo(hctx, UndoRequest{WorkspaceID: u.WorkspaceID, RunID: id, Actor: actor, CorrelationID: corr})
	case "/contacts":
		if w.d.People == nil {
			err = ErrNotAvailable
			break
		}
		contacts, listErr := w.d.People.List(hctx, u.WorkspaceID, "", args, 10)
		if listErr != nil {
			err = listErr
			break
		}
		if len(contacts) == 0 {
			reply = msgNotFound
			break
		}
		var lines []string
		for _, contact := range contacts {
			lines = append(lines, contact.Name+" · "+contact.ID)
		}
		reply = strings.Join(lines, "\n")
	case "/alias":
		if w.d.People == nil {
			err = ErrNotAvailable
			break
		}
		ref, name, ok := strings.Cut(args, " ")
		if !ok || strings.TrimSpace(ref) == "" || strings.TrimSpace(name) == "" {
			return w.reply(ctx, to, usageAlias, "usage")
		}
		contact, aliasErr := w.d.People.SetAlias(hctx, u.WorkspaceID, ref, name)
		if aliasErr != nil {
			err = aliasErr
		} else {
			reply = "Đã đặt tên: " + contact.Name
		}
	case "/prefs", "/template":
		return w.reportCommand(hctx, u, to)
	default:
		reply, err = w.d.Commands.TriggerCommand(hctx, CommandRequest{
			WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID, UpdateID: u.UpdateID,
			Command: u.Command, Args: args, Actor: actor, CorrelationID: corr,
		})
	}
	text, code := handlerText(reply, err)
	return w.reply(ctx, to, text, code)
}

// helpText lists the built-in commands and the enabled behaviors' commands.
func (w *InboxWorker) helpText(ctx context.Context, workspaceID string) string {
	var b strings.Builder
	b.WriteString("Lệnh có sẵn:\n")
	b.WriteString("/help: danh sách lệnh\n")
	b.WriteString("/trace <run id hoặc task> [item]: xem một lần chạy\n")
	b.WriteString("/prefs [short|normal|detailed]: tùy chọn báo cáo\n")
	b.WriteString("/template <kind> [reset]: mẫu báo cáo\n")
	b.WriteString("/undo <run id>: hoàn tác một lần chạy")
	b.WriteString("\n/contacts [tìm kiếm]: danh bạ\n/alias <platform:id> <tên>: đặt tên")
	if w.d.Defs == nil {
		return b.String()
	}
	versions, err := w.d.Defs.EnabledVersions(ctx, workspaceID)
	if err != nil {
		return b.String()
	}
	var lines []string
	for _, v := range versions {
		doc, err := ResolveVersion(v)
		if err != nil {
			continue
		}
		for _, t := range doc.Triggers {
			if t.Type != TriggerTelegramCommand || t.Command == "" {
				continue
			}
			line := t.Command
			for _, a := range t.Args {
				line += " <" + a + ">"
			}
			lines = append(lines, fmt.Sprintf("%s: %s", line, doc.Name))
		}
	}
	if len(lines) > 0 {
		sort.Strings(lines)
		b.WriteString("\n\nLệnh của behavior:\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	return b.String()
}

// callbackArgs splits "<query id> <message id>".
func callbackArgs(args string) (string, int64) {
	qid, msg, _ := strings.Cut(args, " ")
	id, _ := strconv.ParseInt(msg, 10, 64)
	return qid, id
}

func (w *InboxWorker) callback(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	qid, msgID := callbackArgs(u.Args)
	if qid == "" {
		return outcome{code: "bad_callback"}
	}
	toast, code, remove := w.decideCallback(ctx, u, to, msgID, actor, corr)
	if code == "retry" {
		return outcome{code: "storage", retry: true}
	}
	if remove && msgID > 0 && w.d.Messenger != nil {
		rctx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		if err := w.d.Messenger.RemoveButtons(rctx, to, msgID); err != nil {
			code = firstCode(code, "remove_buttons_failed")
		}
		cancel()
	}
	if w.d.Messenger == nil {
		return outcome{code: firstCode(code, "answer_failed")}
	}
	actx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	defer cancel()
	if err := w.d.Messenger.AnswerCallback(actx, to, qid, toast); err != nil {
		code = firstCode(code, "answer_failed")
	}
	return outcome{code: code}
}

// decideCallback returns the toast, the update code ("retry" before any
// handler ran on a storage failure) and whether to remove the buttons.
func (w *InboxWorker) decideCallback(ctx context.Context, u InboundUpdate, to ChatRef, msgID int64, actor, corr string) (string, string, bool) {
	if u.Note != "" {
		return msgBadButton, u.Note, false
	}
	data, err := ParseCallbackData(u.Command)
	if err != nil {
		return msgBadButton, NoteBadCallback, false
	}
	a, err := w.d.Approvals.GetApprovalByNonce(ctx, u.WorkspaceID, data.Nonce)
	if errors.Is(err, ErrNotFound) {
		return msgStale, "stale", true
	}
	if err != nil {
		return "", "retry", false
	}
	if data.Kind.Decides() && (a.Decision != "" || !w.now().Before(a.ExpiresAt)) {
		return msgStale, "stale", true
	}
	if a.Kind == ApprovalAssistantAction {
		return w.assistantCallback(ctx, u, to, data, a, actor, corr)
	}
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	defer cancel()
	res, err := w.d.Callbacks.HandleCallback(hctx, CallbackAction{
		WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID, MessageID: msgID,
		Data: data, Approval: a, Actor: actor, CorrelationID: corr,
	})
	if err != nil {
		text, code := handlerText("", err)
		if code == "not_available" {
			text = "Chưa khả dụng."
		}
		return text, code, code == "stale"
	}
	if res.Reply != "" {
		_ = w.reply(ctx, to, res.Reply, "")
	}
	return firstCode(res.Toast, msgReceived), "", res.RemoveButtons
}

func (w *InboxWorker) freeText(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	nonce := strings.TrimSpace(u.Args)
	if strings.HasPrefix(nonce, templateReplyPrefix) {
		return w.reportTemplateReply(ctx, u, to)
	}
	if strings.HasPrefix(nonce, AssistantTextPrefix) {
		return w.assistant(ctx, u, to, actor, corr)
	}
	if nonce == "" {
		return w.reply(ctx, to, msgHint, "")
	}
	a, err := w.d.Approvals.GetApprovalByNonce(ctx, u.WorkspaceID, nonce)
	if errors.Is(err, ErrNotFound) {
		return w.reply(ctx, to, msgNoOpenReply, NoteNoOpenReply)
	}
	if err != nil {
		return outcome{code: "storage", retry: true}
	}
	if a.Decision != "" || !w.now().Before(a.ExpiresAt) {
		return w.reply(ctx, to, msgStale, "stale")
	}
	if strings.TrimSpace(a.PendingAnswer) == "" {
		return w.reply(ctx, to, msgNoAnswer, "no_pending_answer")
	}
	hctx, cancel := context.WithTimeout(ctx, w.settings.HandlerTimeout)
	reply, err := w.d.Callbacks.HandleFreeText(hctx, FreeTextAnswer{
		WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, ChatID: u.ChatID,
		Approval: a, Answer: a.PendingAnswer, Actor: actor, CorrelationID: corr,
	})
	cancel()
	text, code := handlerText(reply, err)
	// Handed on or refused: the staged text is never kept for a retry.
	if cerr := w.d.Approvals.ClearPendingAnswer(ctx, u.WorkspaceID, a.ID); cerr != nil {
		code = firstCode(code, "clear_failed")
	}
	return w.reply(ctx, to, text, code)
}
