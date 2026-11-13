package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/reporting"
)

const templateReplyPrefix = "report-template:"

// ReportSettings is the mutable owner preference/template surface used by
// Telegram. The concrete implementation is reporting.PostgresStore.
type ReportSettings interface {
	OwnerUserID(context.Context, string) (string, error)
	Prefs(context.Context, string, string, string) (reporting.Prefs, error)
	SetPrefs(context.Context, string, string, string, reporting.Prefs) (reporting.Prefs, error)
	Template(context.Context, string, string, string, reporting.Kind) (string, bool, error)
	SetTemplate(context.Context, string, string, string, reporting.Kind, string) error
	DeleteTemplate(context.Context, string, string, string, reporting.Kind) (bool, error)
	RememberTemplatePrompt(context.Context, string, string, string, reporting.Kind, int64, time.Time) error
	ConsumeTemplateReply(context.Context, string, string) (reporting.TemplateReply, error)
}

func prefsText(p reporting.Prefs) string {
	language := p.Language
	if language == "" {
		language = "mặc định"
	}
	return fmt.Sprintf("Tùy chọn báo cáo:\nstyle: %s\nshow_reaction: %t\nshow_ai_summary: %t\nshow_links: %t\nshow_pr_titles: %t\nshow_trace_hint: %t\nlanguage: %s",
		p.Style, p.ShowReaction, p.ShowAISummary, p.ShowLinks, p.ShowPRTitles, p.ShowTraceHint, language)
}

func mark(enabled bool) string {
	if enabled {
		return "✓"
	}
	return "○"
}

func prefsButtons(p reporting.Prefs) [][]Button {
	return [][]Button{
		{{Text: "Style: " + string(p.Style), Data: "rp:style"}},
		{{Text: mark(p.ShowReaction) + " Reaction", Data: "rp:show_reaction"}},
		{{Text: mark(p.ShowAISummary) + " AI summary", Data: "rp:show_ai_summary"}},
		{{Text: mark(p.ShowLinks) + " Links", Data: "rp:show_links"}},
		{{Text: mark(p.ShowPRTitles) + " PR titles", Data: "rp:show_pr_titles"}},
		{{Text: mark(p.ShowTraceHint) + " Trace hint", Data: "rp:show_trace_hint"}},
	}
}

func (w *InboxWorker) reportOwner(ctx context.Context, workspaceID string) (string, error) {
	if w.d.Reports == nil {
		return "", ErrNotAvailable
	}
	return w.d.Reports.OwnerUserID(ctx, workspaceID)
}

func (w *InboxWorker) reportCommand(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	owner, err := w.reportOwner(ctx, u.WorkspaceID)
	if err != nil {
		text, code := handlerText("", err)
		return w.reply(ctx, to, text, code)
	}
	switch u.Command {
	case "/prefs":
		return w.prefsCommand(ctx, u, to, owner)
	case "/template":
		return w.templateCommand(ctx, u, to, owner)
	default:
		return w.reply(ctx, to, msgUnknown, "unknown_command")
	}
}

func (w *InboxWorker) prefsCommand(ctx context.Context, u InboundUpdate, to ChatRef, owner string) outcome {
	p, err := w.d.Reports.Prefs(ctx, owner, u.WorkspaceID, "")
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	arg := strings.TrimSpace(u.Args)
	if arg != "" {
		style := reporting.Style(arg)
		if style != reporting.StyleShort && style != reporting.StyleNormal && style != reporting.StyleDetailed {
			return w.reply(ctx, to, "Cú pháp: /prefs [short|normal|detailed]", "usage")
		}
		p.Style = style
		p, err = w.d.Reports.SetPrefs(ctx, owner, u.WorkspaceID, "", p)
		if err != nil {
			return w.reply(ctx, to, msgFailed, "handler_failed")
		}
	}
	return w.sendReportMessage(ctx, to, OutMessage{Text: prefsText(p), Buttons: prefsButtons(p)}, "")
}

func (w *InboxWorker) templateCommand(ctx context.Context, u InboundUpdate, to ChatRef, owner string) outcome {
	parts := strings.Fields(u.Args)
	if len(parts) < 1 || len(parts) > 2 {
		return w.reply(ctx, to, "Cú pháp: /template <kind> [reset]", "usage")
	}
	kind, err := reporting.ParseKind(parts[0])
	if err != nil {
		return w.reply(ctx, to, err.Error(), "usage")
	}
	if len(parts) == 2 {
		if parts[1] != "reset" {
			return w.reply(ctx, to, "Cú pháp: /template <kind> [reset]", "usage")
		}
		if _, err := w.d.Reports.DeleteTemplate(ctx, owner, u.WorkspaceID, "", kind); err != nil {
			return w.reply(ctx, to, msgFailed, "handler_failed")
		}
		return w.reply(ctx, to, "Đã dùng lại mẫu mặc định.", "")
	}
	body, ok, err := w.d.Reports.Template(ctx, owner, u.WorkspaceID, "", kind)
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	preview, err := reporting.Preview(kind)
	if ok {
		preview, err = reporting.Preview(kind, body)
	}
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	shown := "mặc định"
	if ok {
		shown = body
	}
	messageID, out := w.sendReportMessageID(ctx, to, OutMessage{Text: "Mẫu hiện tại:\n" + shown + "\n\nXem trước:\n" + preview + "\n\nTrả lời tin nhắn này để lưu mẫu mới.", ForceReply: true}, "")
	if out.code != "" {
		return out
	}
	if err := w.d.Reports.RememberTemplatePrompt(ctx, owner, u.WorkspaceID, "", kind, messageID, w.now().Add(24*time.Hour)); err != nil {
		return outcome{code: "storage"}
	}
	return outcome{}
}

func (w *InboxWorker) sendReportMessage(ctx context.Context, to ChatRef, message OutMessage, code string) outcome {
	_, out := w.sendReportMessageID(ctx, to, message, code)
	return out
}

func (w *InboxWorker) sendReportMessageID(ctx context.Context, to ChatRef, message OutMessage, code string) (int64, outcome) {
	if w.d.Messenger == nil {
		return 0, outcome{code: firstCode(code, "reply_failed")}
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	defer cancel()
	id, err := w.d.Messenger.SendMessage(sendCtx, to, message)
	if err != nil {
		return 0, outcome{code: firstCode(code, "reply_failed")}
	}
	return id, outcome{code: code}
}

func (w *InboxWorker) reportCallback(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	qid, messageID := callbackArgs(u.Args)
	if qid == "" {
		return outcome{code: "bad_callback"}
	}
	owner, err := w.reportOwner(ctx, u.WorkspaceID)
	if err != nil {
		return outcome{code: "handler_failed"}
	}
	p, err := w.d.Reports.Prefs(ctx, owner, u.WorkspaceID, "")
	if err != nil {
		return outcome{code: "storage", retry: true}
	}
	action := strings.TrimPrefix(u.Command, "rp:")
	switch action {
	case "style":
		switch p.Style {
		case reporting.StyleShort:
			p.Style = reporting.StyleNormal
		case reporting.StyleNormal:
			p.Style = reporting.StyleDetailed
		default:
			p.Style = reporting.StyleShort
		}
	case "show_reaction":
		p.ShowReaction = !p.ShowReaction
	case "show_ai_summary":
		p.ShowAISummary = !p.ShowAISummary
	case "show_links":
		p.ShowLinks = !p.ShowLinks
	case "show_pr_titles":
		p.ShowPRTitles = !p.ShowPRTitles
	case "show_trace_hint":
		p.ShowTraceHint = !p.ShowTraceHint
	default:
		return w.answerReportCallback(ctx, to, u, msgBadButton, "bad_callback")
	}
	p, err = w.d.Reports.SetPrefs(ctx, owner, u.WorkspaceID, "", p)
	if err != nil {
		// The write may have committed before its error became visible. Do not
		// retry a toggle and risk applying it twice.
		return outcome{code: "storage"}
	}
	if w.d.Messenger != nil && messageID > 0 {
		rctx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		_ = w.d.Messenger.RemoveButtons(rctx, to, messageID)
		cancel()
	}
	out := w.answerReportCallback(ctx, to, u, "Đã cập nhật.", "")
	if out.code != "" {
		return out
	}
	return w.sendReportMessage(ctx, to, OutMessage{Text: prefsText(p), Buttons: prefsButtons(p)}, "")
}

func (w *InboxWorker) answerReportCallback(ctx context.Context, to ChatRef, u InboundUpdate, text, code string) outcome {
	qid, _ := callbackArgs(u.Args)
	if qid == "" || w.d.Messenger == nil {
		return outcome{code: firstCode(code, "answer_failed")}
	}
	actx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	defer cancel()
	if err := w.d.Messenger.AnswerCallback(actx, to, qid, text); err != nil {
		code = firstCode(code, "answer_failed")
	}
	return outcome{code: code}
}

func (w *InboxWorker) reportTemplateReply(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	if w.d.Reports == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	id := strings.TrimPrefix(u.Args, templateReplyPrefix)
	reply, err := w.d.Reports.ConsumeTemplateReply(ctx, u.WorkspaceID, id)
	if err != nil {
		return w.reply(ctx, to, msgNoOpenReply, NoteNoOpenReply)
	}
	if err := reporting.Validate(reply.Body); err != nil {
		return w.reply(ctx, to, "Mẫu không hợp lệ: "+err.Error()+".", "invalid_template")
	}
	if err := w.d.Reports.SetTemplate(ctx, reply.UserID, u.WorkspaceID, reply.Behavior, reply.Kind, reply.Body); err != nil {
		if errors.Is(err, reporting.ErrTemplatePromptNotFound) {
			return w.reply(ctx, to, msgNoOpenReply, NoteNoOpenReply)
		}
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	return w.reply(ctx, to, "Đã lưu mẫu báo cáo.", "")
}
