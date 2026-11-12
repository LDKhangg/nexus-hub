package behavior

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// texts holds the short messages the action library sends to the owner.
// Vietnamese is the default; a profile or behavior language starting with
// "en" switches to English. No text carries a secret or a provider error:
// failures are named by engine codes only.
type texts struct{ en bool }

func textsFor(language string) texts { return texts{en: strings.HasPrefix(language, "en")} }

func (t texts) pick(vi, en string) string {
	if t.en {
		return en
	}
	return vi
}

// Buttons.
func (t texts) btnPost() string    { return t.pick("Đăng", "Post") }
func (t texts) btnSkip() string    { return t.pick("Bỏ qua", "Skip") }
func (t texts) btnAgree() string   { return t.pick("Đồng ý với gợi ý", "Accept suggestion") }
func (t texts) btnOther() string   { return t.pick("Trả lời khác", "Other answer") }
func (t texts) btnConfirm() string { return t.pick("Xác nhận", "Confirm") }
func (t texts) btnCancel() string  { return t.pick("Huỷ", "Cancel") }
func (t texts) btnUp() string      { return "👍" }
func (t texts) btnDown() string    { return t.pick("👎 + lý do", "👎 + reason") }
func (t texts) runRef(name string, runID int64) string {
	return fmt.Sprintf("%s · run %d", name, runID)
}

// Headings and labels.
func (t texts) draftHead() string { return t.pick("📝 Nháp", "📝 Draft") }
func (t texts) askHead() string {
	return t.pick("❓ Cần bạn trả lời", "❓ Needs your answer")
}
func (t texts) digestHead() string { return t.pick("ℹ️ Thông tin", "ℹ️ For your information") }
func (t texts) fyiHead() string    { return t.pick("✅ Đã tự xử lý", "✅ Done on its own") }
func (t texts) confirmHead() string {
	return t.pick("⚠️ Cần xác nhận thay đổi", "⚠️ Confirm this change")
}
func (t texts) proposalLabel() string { return t.pick("Đề xuất", "Proposal") }
func (t texts) suggestLabel() string  { return t.pick("Gợi ý", "Suggestion") }
func (t texts) sourcesLabel() string  { return t.pick("Nguồn", "Sources") }

func (t texts) statusWord(s ItemStatus) string {
	switch s {
	case ItemDone:
		return t.pick("xong", "done")
	case ItemConflict:
		return t.pick("xung đột, không ghi", "conflict, not written")
	case ItemFailed:
		return t.pick("lỗi", "failed")
	case ItemSkipped:
		return t.pick("bỏ qua", "skipped")
	}
	return string(s)
}

func (t texts) changeStatus(task, from, to string) string {
	return t.pick(fmt.Sprintf("Đổi status task %s: %s → %s?", task, from, to), fmt.Sprintf("Change task %s status: %s → %s?", task, from, to))
}
func (t texts) changeUnassign(task, user string) string {
	return t.pick(fmt.Sprintf("Bỏ giao user %s khỏi task %s?", user, task), fmt.Sprintf("Unassign user %s from task %s?", user, task))
}

// Replies and toasts.
func (t texts) posted() string        { return t.pick("Đã đăng.", "Posted.") }
func (t texts) answered() string      { return t.pick("Đã ghi câu trả lời.", "Answer recorded.") }
func (t texts) skipped() string       { return t.pick("Đã bỏ qua.", "Skipped.") }
func (t texts) canceled() string      { return t.pick("Đã huỷ.", "Canceled.") }
func (t texts) thanks() string        { return t.pick("Đã ghi nhận.", "Noted.") }
func (t texts) notApplicable() string { return t.pick("Không áp dụng.", "Not applicable.") }
func (t texts) replyPrompt() string {
	return t.pick("Hãy trả lời tin nhắn này bằng câu trả lời của bạn.", "Reply to this message with your answer.")
}
func (t texts) replyPromptToast() string {
	return t.pick("Hãy trả lời tin nhắn vừa gửi.", "Reply to the message just sent.")
}
func (t texts) reasonPrompt() string {
	return t.pick("Lý do 👎? Trả lời tin nhắn này (không bắt buộc).", "Why 👎? Reply to this message (optional).")
}
func (t texts) reasonSaved() string { return t.pick("Đã ghi lý do.", "Reason recorded.") }
func (t texts) conflict(runID int64) string {
	return t.pick(fmt.Sprintf("Task đã thay đổi từ lúc chạy, không ghi. Xem /trace %d", runID),
		fmt.Sprintf("The task changed since the run read it; nothing written. See /trace %d", runID))
}
func (t texts) writeFailed(code string, runID int64) string {
	return t.pick(fmt.Sprintf("Không ghi được (%s). Xem /trace %d", code, runID), fmt.Sprintf("Write failed (%s). See /trace %d", code, runID))
}
func (t texts) statusDone(from, to string) string {
	return t.pick(fmt.Sprintf("Đã đổi status: %s → %s.", from, to), fmt.Sprintf("Status changed: %s → %s.", from, to))
}
func (t texts) statusAlready(to string) string {
	return t.pick(fmt.Sprintf("Status đã là %s, không làm gì.", to), fmt.Sprintf("Status is already %s; nothing done.", to))
}
func (t texts) statusMoved(cur string) string {
	return t.pick(fmt.Sprintf("Status đã đổi thành %s, không làm gì.", cur), fmt.Sprintf("Status is now %s; nothing done.", cur))
}
func (t texts) unassignDone(user string) string {
	return t.pick(fmt.Sprintf("Đã bỏ giao user %s.", user), fmt.Sprintf("User %s unassigned.", user))
}
func (t texts) unassignAlready(user string) string {
	return t.pick(fmt.Sprintf("User %s đã không còn được giao, không làm gì.", user), fmt.Sprintf("User %s is not assigned; nothing done.", user))
}
func (t texts) runFailed(name string, runID int64, code string) string {
	return t.pick(fmt.Sprintf("⚠️ Run %d (%s) lỗi: %s. Xem /trace %d", runID, name, code, runID),
		fmt.Sprintf("⚠️ Run %d (%s) failed: %s. See /trace %d", runID, name, code, runID))
}
func (t texts) undone(runID int64) string {
	return t.pick(fmt.Sprintf("Đã hoàn tác run %d.", runID), fmt.Sprintf("Run %d undone.", runID))
}
func (t texts) undoAlready() string {
	return t.pick("Run này đã được hoàn tác.", "This run was already undone.")
}
func (t texts) undoNothing() string {
	return t.pick("Run này chưa ghi mô tả nào để hoàn tác.", "This run wrote no description to undo.")
}
func (t texts) undoChanged() string {
	return t.pick("Mô tả đã thay đổi sau lần ghi của Nexus, không hoàn tác.", "The description changed after Nexus wrote it; not undone.")
}
func (t texts) undoFailed(code string) string {
	return t.pick(fmt.Sprintf("Không hoàn tác được (%s).", code), fmt.Sprintf("Undo failed (%s).", code))
}

// capRunes clips s to n runes with an ellipsis.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// maxTelegramRunes keeps a message under Telegram's 4096-character limit.
const maxTelegramRunes = 3800
