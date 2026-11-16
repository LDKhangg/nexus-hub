package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

const workflowTargetLifetime = 10 * time.Minute

type workflowTargetPayload struct {
	ChatID         int64   `json:"chat_id"`
	Kind           string  `json:"kind"`
	MemoryKey      string  `json:"memory_key,omitempty"`
	UpdateID       int64   `json:"update_id"`
	MessageID      int64   `json:"message_id,omitempty"`
	ConversationID int64   `json:"conversation_id"`
	Candidates     []int64 `json:"candidates"`
}

func (w *InboxWorker) workflowReader() workflow.Reader {
	if w.d.Workflows != nil {
		return w.d.Workflows
	}
	if w.d.Assistant != nil {
		return w.d.Assistant.Workflows
	}
	return nil
}

func (w *InboxWorker) workflowEngine() workflow.Engine {
	if w.d.WorkflowCommands != nil {
		return w.d.WorkflowCommands
	}
	if w.d.Assistant != nil {
		return w.d.Assistant.WorkflowCommands
	}
	return nil
}

// workflowOrAssistant takes staged owner text exactly once, handles explicit
// workflow instructions before any model call, and otherwise queues the
// unchanged text for the assistant.
func (w *InboxWorker) workflowOrAssistant(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	a := w.d.Assistant
	if a == nil || a.Inbox == nil {
		return w.reply(ctx, to, msgHint, "")
	}
	key := strings.TrimPrefix(strings.TrimSpace(u.Args), AssistantTextPrefix)
	text, ok := a.Inbox.take(u.WorkspaceID, key, w.now())
	if !ok {
		return w.reply(ctx, to, msgAssistantGone, "assistant_text_gone")
	}
	trimmed := strings.TrimSpace(text)
	// A message that is only a ClickUp task link, written in General, gets
	// the "Mở topic" button of that task's topic.
	if u.ThreadID == 0 && w.d.Links != nil {
		if ref, ok := taskURLTaskRef(trimmed); ok && w.jumpAvailable(u) {
			return w.topicJumpRef(ctx, u, to, ref)
		}
	}
	// Target order: exactly one explicit WF-n anywhere in the text, then the
	// reply target, then a clarification. Several references never store.
	refs := workflow.FindRefs(trimmed)
	if trimmed != "" && !strings.HasPrefix(trimmed, "/") && isWorkflowQuestion(trimmed) {
		// A free-text question is never stored as an instruction, even when it
		// starts with WF-n; /note forces an instruction. Context is the single
		// explicit reference, else the reply target, else none.
		var id int64
		var resolved bool
		switch len(refs) {
		case 0:
			id, resolved = w.resolveWorkflowTarget(ctx, u, 0)
		case 1:
			id, resolved = w.resolveWorkflowTarget(ctx, u, refs[0])
		}
		if resolved {
			return w.queueAssistantTextForWorkflow(ctx, u, to, actor, corr, text, id)
		}
		if len(refs) == 0 {
			// Inside a topic, the subject's single active execution is the
			// context; otherwise the subject itself is.
			if t := w.topicTarget(ctx, u); t.has() {
				if id, ok := t.single(); ok {
					return w.queueAssistantTextForWorkflow(ctx, u, to, actor, corr, text, id)
				}
				return w.queueAssistantTopic(ctx, u, to, actor, corr, text, t)
			}
		}
		return w.queueAssistantText(ctx, u, to, actor, corr, text)
	}
	if id, instruction, explicit := leadingWorkflowInstruction(trimmed); explicit {
		if len(refs) != 1 {
			return w.clarifyWorkflowTarget(ctx, u, to, "note", instruction)
		}
		return w.handleWorkflowRequest(ctx, u, to, actor, corr, "note", instruction, id)
	}
	if u.ReplyToMessageID > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "/") {
		switch len(refs) {
		case 0:
			if id, resolved := w.resolveWorkflowTarget(ctx, u, 0); resolved {
				if w.workflowTerminal(ctx, u.WorkspaceID, id) {
					// A plain reply to a finished workflow's message (for example a
					// final report) is a conversation, not an instruction.
					return w.queueAssistantTextForWorkflow(ctx, u, to, actor, corr, text, id)
				}
				return w.executeWorkflowRequest(ctx, u, to, actor, corr, "note", trimmed, id)
			}
		case 1:
			return w.handleWorkflowRequest(ctx, u, to, actor, corr, "note", trimmed, refs[0])
		default:
			return w.clarifyWorkflowTarget(ctx, u, to, "note", trimmed)
		}
	}
	// Inside a topic, text without a WF reference and without a bound reply
	// target goes to the topic subject: one active execution gets the
	// instruction, several ask, none ask the assistant with the subject as
	// context. A fixed topic, General and unknown threads keep Phase 6.
	if len(refs) == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "/") {
		if t := w.topicTarget(ctx, u); t.has() {
			if id, ok := t.single(); ok {
				return w.executeWorkflowRequest(ctx, u, to, actor, corr, "note", trimmed, id)
			}
			if len(t.active) > 0 {
				return w.clarifyWorkflowTarget(ctx, u, to, "note", trimmed)
			}
			return w.queueAssistantTopic(ctx, u, to, actor, corr, text, t)
		}
	}
	return w.queueAssistantText(ctx, u, to, actor, corr, text)
}

// workflowTerminal reports whether the execution is known to be terminal.
func (w *InboxWorker) workflowTerminal(ctx context.Context, ws string, id int64) bool {
	r := w.workflowReader()
	if r == nil {
		return false
	}
	d, err := r.GetExecutionDetail(ctx, ws, id)
	return err == nil && d.State.IsTerminal()
}

func isWorkflowQuestion(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasSuffix(text, "?") || strings.HasSuffix(text, "？")
}

func leadingWorkflowInstruction(text string) (int64, string, bool) {
	first, rest, found := strings.Cut(text, " ")
	if !found {
		return 0, "", false
	}
	id, ok := workflow.ParseRef(first)
	if !ok {
		return 0, "", false
	}
	return id, strings.TrimSpace(rest), true
}

func (w *InboxWorker) workflowControlCommand(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr string) outcome {
	args := strings.TrimSpace(u.Args)
	switch u.Command {
	case "/list":
		if args != "" {
			return w.reply(ctx, to, "Cú pháp: /list", "usage")
		}
		if out, ok := w.topicWorkflowList(ctx, u, to); ok {
			return out
		}
		return w.workflowList(ctx, u, to)
	case "/note":
		if args == "" {
			return w.reply(ctx, to, "Cú pháp: /note [WF-id] <nội dung>", "usage")
		}
		id, text := int64(0), args
		refs := workflow.FindRefs(args)
		first, rest, found := strings.Cut(args, " ")
		if parsed, ok := workflow.ParseRef(first); ok {
			if !found || strings.TrimSpace(rest) == "" {
				return w.reply(ctx, to, "Cú pháp: /note [WF-id] <nội dung>", "usage")
			}
			id, text = parsed, strings.TrimSpace(rest)
		} else if len(refs) == 1 {
			id = refs[0]
		}
		if len(refs) > 1 {
			return w.clarifyWorkflowTarget(ctx, u, to, "note", text)
		}
		return w.handleWorkflowRequest(ctx, u, to, actor, corr, "note", text, id)
	case "/status", "/cancel", "/resume":
		id := int64(0)
		if args != "" {
			var ok bool
			id, ok = workflow.ParseRef(args)
			if !ok {
				return w.reply(ctx, to, "Cú pháp: "+u.Command+" [WF-id]", "usage")
			}
		}
		return w.handleWorkflowRequest(ctx, u, to, actor, corr, strings.TrimPrefix(u.Command, "/"), "", id)
	}
	return w.reply(ctx, to, msgUnknown, "unknown_command")
}

func (w *InboxWorker) resolveWorkflowTarget(ctx context.Context, u InboundUpdate, explicitID int64) (int64, bool) {
	r := w.workflowReader()
	if r == nil {
		return 0, false
	}
	if explicitID > 0 {
		if _, err := r.GetExecutionDetail(ctx, u.WorkspaceID, explicitID); err != nil {
			return 0, false
		}
		if state := conversationFromContext(ctx); w.d.Conversations != nil && state.id > 0 {
			if err := w.d.Conversations.BindWorkflow(ctx, u.WorkspaceID, state.id, explicitID, "explicit_ref"); err != nil {
				return 0, false
			}
		}
		return explicitID, true
	}
	if u.ReplyToMessageID <= 0 || w.d.Conversations == nil {
		return 0, false
	}
	ids, err := w.d.Conversations.ExecutionsByResource(ctx, u.WorkspaceID, "telegram.message",
		workflow.TelegramMessageRef(u.IntegrationID, u.ChatID, u.ReplyToMessageID))
	if err != nil || len(ids) != 1 {
		return 0, false
	}
	if _, err := r.GetExecutionDetail(ctx, u.WorkspaceID, ids[0]); err != nil {
		return 0, false
	}
	return ids[0], true
}

func (w *InboxWorker) handleWorkflowRequest(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr, kind, text string, explicitID int64) outcome {
	if id, ok := w.resolveWorkflowTarget(ctx, u, explicitID); ok {
		return w.executeWorkflowRequest(ctx, u, to, actor, corr, kind, text, id)
	}
	if explicitID <= 0 {
		// No explicit id and no bound reply target: inside a topic, the
		// subject's single active execution is the target.
		if id, ok := w.topicTarget(ctx, u).single(); ok {
			return w.executeWorkflowRequest(ctx, u, to, actor, corr, kind, text, id)
		}
	}
	return w.clarifyWorkflowTarget(ctx, u, to, kind, text)
}

func (w *InboxWorker) activeBoundWorkflows(ctx context.Context, ws string, conversationID int64) []workflow.ExecutionDetail {
	if w.d.Conversations == nil || conversationID <= 0 || w.workflowReader() == nil {
		return nil
	}
	ids, err := w.d.Conversations.ListBoundWorkflows(ctx, ws, conversationID)
	if err != nil {
		return nil
	}
	items := make([]workflow.ExecutionDetail, 0, len(ids))
	for _, id := range ids {
		d, err := w.workflowReader().GetExecutionDetail(ctx, ws, id)
		if err == nil && !d.State.IsTerminal() {
			items = append(items, d)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	if len(items) > 5 {
		items = items[:5]
	}
	return items
}

func (w *InboxWorker) clarifyWorkflowTarget(ctx context.Context, u InboundUpdate, to ChatRef, kind, text string) outcome {
	state := conversationFromContext(ctx)
	var items []workflow.ExecutionDetail
	if t := w.topicTarget(ctx, u); t.has() {
		// Inside a topic, the buttons list only the subject's executions.
		items = t.active
		if len(items) > 5 {
			items = items[:5]
		}
	} else {
		items = w.activeBoundWorkflows(ctx, u.WorkspaceID, state.id)
	}
	if len(items) == 0 || w.d.Approvals == nil {
		return w.reply(ctx, to, "Không tìm thấy workflow đang hoạt động phù hợp. Hãy ghi rõ mã WF-id, ví dụ: WF-123 <nội dung>.", "workflow_target_not_found")
	}
	expires := w.now().Add(workflowTargetLifetime)
	memoryKey := ""
	if text != "" {
		var err error
		memoryKey, err = w.workflowTargetInbox.stageUntil(u.WorkspaceID, text, w.now(), expires)
		if err != nil {
			return w.reply(ctx, to, msgFailed, "handler_failed")
		}
	}
	p := workflowTargetPayload{ChatID: u.ChatID, Kind: kind, MemoryKey: memoryKey, UpdateID: u.UpdateID,
		MessageID: u.MessageID, ConversationID: state.id, Candidates: make([]int64, len(items))}
	labels := make([]string, len(items))
	for i, item := range items {
		p.Candidates[i] = item.ID
		labels[i] = workflow.FormatRef(item.ID)
	}
	payload, _ := json.Marshal(p)
	options, _ := json.Marshal(labels)
	nonce, err := NewNonce()
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	ap, err := w.d.Approvals.CreateApproval(ctx, u.WorkspaceID, NewApproval{Kind: ApprovalWorkflowTarget,
		Action: kind, Resource: "conversation:" + strconv.FormatInt(state.id, 10), Payload: payload, Options: options,
		Nonce: nonce, ExpiresAt: expires})
	if err != nil {
		w.workflowTargetInbox.remove(memoryKey)
		return w.reply(ctx, to, msgFailed, "storage")
	}
	buttons := make([][]Button, 0, len(items))
	for i, item := range items {
		data, err := (CallbackData{Kind: CallbackOption, Nonce: ap.Nonce, Option: i}).Encode()
		if err != nil {
			return w.reply(ctx, to, msgFailed, "handler_failed")
		}
		label := workflow.FormatRef(item.ID) + " · " + oneLine(item.BehaviorName, 40) + " · " + string(item.State)
		buttons = append(buttons, []Button{{Text: label, Data: data}})
	}
	id, out := w.sendReportMessageID(ctx, to, OutMessage{Text: "Chọn workflow cần xử lý:", Buttons: buttons}, "workflow_target")
	if out.code == "workflow_target" && id > 0 {
		if err := w.d.Approvals.SetApprovalMessage(ctx, u.WorkspaceID, ap.ID, id); err != nil {
			out.code = "workflow_target_message_not_saved"
		}
	}
	return out
}

func (w *InboxWorker) workflowTargetCallback(ctx context.Context, u InboundUpdate, to ChatRef, data CallbackData, a Approval, actor, corr string) (string, string, bool) {
	if data.Kind != CallbackOption {
		return msgBadButton, NoteBadCallback, false
	}
	var p workflowTargetPayload
	if json.Unmarshal(a.Payload, &p) != nil || a.WorkspaceID != u.WorkspaceID || p.ChatID != u.ChatID || p.ConversationID <= 0 ||
		data.Option < 0 || data.Option >= len(p.Candidates) {
		return msgStale, "stale", true
	}
	id := p.Candidates[data.Option]
	r := w.workflowReader()
	if r == nil {
		return "Không xử lý được, hãy bấm lại.", "workflow_target_retry", false
	}
	if _, err := r.GetExecutionDetail(ctx, u.WorkspaceID, id); err != nil {
		if !errors.Is(err, workflow.ErrNotFound) {
			return "Không xử lý được, hãy bấm lại.", "workflow_target_retry", false
		}
		return w.finishWorkflowTarget(ctx, u, a, actor, p.MemoryKey, "Không tìm thấy workflow.", "not_found")
	}
	text := ""
	if p.Kind == "note" {
		var ok bool
		text, ok = w.workflowTargetInbox.peek(u.WorkspaceID, p.MemoryKey, w.now())
		if !ok {
			return w.finishWorkflowTarget(ctx, u, a, actor, p.MemoryKey, "Yêu cầu đã hết hạn, hãy gửi lại.", "workflow_target_expired")
		}
	}
	original := InboundUpdate{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, UpdateID: p.UpdateID,
		ChatID: u.ChatID, MessageID: p.MessageID, Kind: UpdateText}
	ctx = withConversation(ctx, p.ConversationID, UpdateText)
	out := w.executeWorkflowRequest(ctx, original, to, actor, corr, p.Kind, text, id)
	if workflowTargetSucceeded(p.Kind, out.code) {
		return w.finishWorkflowTarget(ctx, u, a, actor, p.MemoryKey, "Đã chọn "+workflow.FormatRef(id), out.code)
	}
	if toast, terminal := workflowTargetTerminal(out.code); terminal {
		return w.finishWorkflowTarget(ctx, u, a, actor, p.MemoryKey, toast, out.code)
	}
	return "Không xử lý được, hãy bấm lại.", out.code, false
}

func (w *InboxWorker) finishWorkflowTarget(ctx context.Context, u InboundUpdate, a Approval, actor, memoryKey, toast, code string) (string, string, bool) {
	if _, err := w.d.Approvals.Decide(ctx, u.WorkspaceID, a.Nonce, DecisionApproved, "", actor, w.now()); err != nil {
		text, decideCode := handlerText("", err)
		if decideCode == "stale" {
			w.workflowTargetInbox.remove(memoryKey)
			return text, decideCode, true
		}
		return "Không xử lý được, hãy bấm lại.", "workflow_target_retry", false
	}
	w.workflowTargetInbox.remove(memoryKey)
	return toast, code, true
}

func workflowTargetSucceeded(kind, code string) bool {
	if code == "" || code == "reply_failed" {
		return true
	}
	return kind == "cancel" && code == "assistant_"+toolWorkflowCancel ||
		kind == "resume" && code == "assistant_"+toolWorkflowResume
}

func workflowTargetTerminal(code string) (string, bool) {
	switch code {
	case "not_found":
		return "Không tìm thấy workflow.", true
	case "workflow_terminal", "not_cancellable":
		return "Workflow đã kết thúc.", true
	case "not_resumable":
		return "Workflow không thể tiếp tục.", true
	case "usage", "unknown_command":
		return "Yêu cầu không hợp lệ.", true
	default:
		return "", false
	}
}

func (w *InboxWorker) executeWorkflowRequest(ctx context.Context, u InboundUpdate, to ChatRef, actor, corr, kind, text string, id int64) outcome {
	switch kind {
	case "note":
		return w.appendWorkflowInstruction(ctx, u, to, id, text)
	case "status":
		return w.workflowStatus(ctx, u, to, id)
	case "cancel", "resume":
		return w.proposeWorkflowControl(ctx, u, to, kind, id)
	}
	return w.reply(ctx, to, msgUnknown, "unknown_command")
}

func (w *InboxWorker) appendWorkflowInstruction(ctx context.Context, u InboundUpdate, to ChatRef, id int64, text string) outcome {
	text = sanitizeWorkflowInstruction(text)
	state := conversationFromContext(ctx)
	if w.d.Conversations == nil || state.id <= 0 || text == "" || utf8.RuneCountInString(text) > 2000 {
		return w.reply(ctx, to, "Yêu cầu bổ sung phải có từ 1 đến 2.000 ký tự.", "usage")
	}
	ins, err := w.d.Conversations.AppendInstruction(ctx, workflow.InstructionRequest{WorkspaceID: u.WorkspaceID,
		ExecutionID: id, Text: text, ConversationID: state.id, TelegramUpdateID: u.UpdateID})
	if errors.Is(err, workflow.ErrNotEligible) {
		return w.reply(ctx, to, workflow.FormatRef(id)+": workflow đã kết thúc, không thể nhận yêu cầu bổ sung.", "workflow_terminal")
	}
	if errors.Is(err, workflow.ErrNotFound) {
		return w.reply(ctx, to, msgNotFound, "not_found")
	}
	if err != nil {
		return w.reply(ctx, to, msgFailed, "storage")
	}
	_ = w.d.Conversations.BindWorkflow(ctx, u.WorkspaceID, state.id, id, "instruction")
	if u.MessageID > 0 {
		_ = w.d.Conversations.BindResource(ctx, workflow.ResourceBinding{WorkspaceID: u.WorkspaceID, ExecutionID: id,
			Kind: "telegram.message", ExternalID: workflow.TelegramMessageRef(u.IntegrationID, u.ChatID, u.MessageID)})
	}
	return w.reply(ctx, to, fmt.Sprintf("%s: đã nhận yêu cầu bổ sung (v%d)", workflow.FormatRef(id), ins.Version), "")
}

func (w *InboxWorker) workflowList(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	r := w.workflowReader()
	if r == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	const (
		listLimit = 20
		maxPages  = 10
	)
	lines := make([]string, 0, listLimit)
	var listed []int64
	var before int64
	for page := 0; page < maxPages && len(lines) < listLimit; page++ {
		items, next, err := r.ListExecutions(ctx, workflow.ExecutionQuery{WorkspaceID: u.WorkspaceID, BeforeID: before, Limit: listLimit})
		if err != nil {
			return w.reply(ctx, to, msgFailed, "handler_failed")
		}
		for _, item := range items {
			if !item.State.IsTerminal() {
				lines = append(lines, fmt.Sprintf("%s · %s · %s", workflow.FormatRef(item.ID), oneLine(item.BehaviorName, 60), item.State))
				listed = append(listed, item.ID)
				if len(lines) == listLimit {
					break
				}
			}
		}
		if next <= 0 || next == before {
			break
		}
		before = next
	}
	if len(lines) == 0 {
		return w.reply(ctx, to, "Không có workflow đang hoạt động.", "")
	}
	if rows := w.listJumpRows(ctx, u, listed); len(rows) > 0 {
		return w.replyMessage(ctx, to, OutMessage{Text: strings.Join(lines, "\n"), Buttons: rows}, "")
	}
	return w.reply(ctx, to, strings.Join(lines, "\n"), "")
}

func (w *InboxWorker) workflowStatus(ctx context.Context, u InboundUpdate, to ChatRef, id int64) outcome {
	r := w.workflowReader()
	if r == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	d, err := r.GetExecutionDetail(ctx, u.WorkspaceID, id)
	if err != nil {
		return w.reply(ctx, to, msgNotFound, "not_found")
	}
	lines := []string{fmt.Sprintf("%s · %s · %s", workflow.FormatRef(id), oneLine(d.BehaviorName, 60), d.State)}
	if d.LiveJob != nil {
		lines = append(lines, "Bước hiện tại: "+d.LiveJob.StepKey)
	}
	if len(d.Waits) > 0 {
		waits := make([]string, 0, len(d.Waits))
		for _, wait := range d.Waits {
			if wait.ClosedAt == nil {
				waits = append(waits, fmt.Sprintf("%s (%s)", wait.StepKey, wait.Kind))
			}
		}
		lines = append(lines, fmt.Sprintf("Đang chờ: %d", len(waits)))
		if len(waits) > 0 {
			lines = append(lines, strings.Join(waits, ", "))
		}
	}
	if w.d.Conversations != nil {
		if instructions, err := w.d.Conversations.ListInstructions(ctx, u.WorkspaceID, id); err == nil && len(instructions) > 0 {
			latest := instructions[0]
			for _, instruction := range instructions[1:] {
				if instruction.Version > latest.Version {
					latest = instruction
				}
			}
			steps, err := r.ListSteps(ctx, u.WorkspaceID, id)
			if err != nil {
				return w.reply(ctx, to, msgFailed, "handler_failed")
			}
			lines = append(lines, fmt.Sprintf("Yêu cầu: v%d (%s) · %d bản · used v%d", latest.Version,
				latest.CreatedAt.UTC().Format(time.RFC3339), len(instructions), workflow.OwnerInstructionsUsedVersion(steps)))
		}
		if lister, ok := w.d.Conversations.(workflow.ResourceLister); ok {
			if resources, err := lister.ListResources(ctx, u.WorkspaceID, id); err == nil {
				counts := map[string]int{}
				for _, resource := range resources {
					counts[resource.Kind]++
				}
				kinds := make([]string, 0, len(counts))
				for kind, count := range counts {
					kinds = append(kinds, fmt.Sprintf("%s:%d", kind, count))
				}
				sort.Strings(kinds)
				lines = append(lines, fmt.Sprintf("Tài nguyên: %d", len(resources))+func() string {
					if len(kinds) == 0 {
						return ""
					}
					return " (" + strings.Join(kinds, ", ") + ")"
				}())
			}
		}
	}
	return w.reply(ctx, to, strings.Join(lines, "\n"), "")
}

func (w *InboxWorker) proposeWorkflowControl(ctx context.Context, u InboundUpdate, to ChatRef, kind string, id int64) outcome {
	r := w.workflowReader()
	if r == nil || w.workflowEngine() == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	d, err := r.GetExecutionDetail(ctx, u.WorkspaceID, id)
	if err != nil {
		return w.reply(ctx, to, msgNotFound, "not_found")
	}
	p := assistantPayload{ExecutionID: id}
	action, text := toolWorkflowCancel, ""
	if kind == "cancel" {
		if d.State.IsTerminal() {
			return w.reply(ctx, to, fmt.Sprintf("%s đã kết thúc (%s), không thể huỷ.", workflow.FormatRef(id), d.State), "not_cancellable")
		}
		p.IdempotencyKey = fmt.Sprintf("telegram:cancel:%s:%d", u.WorkspaceID, id)
		text = fmt.Sprintf("Huỷ %s (%s · %s)? Yêu cầu bên ngoài đang chạy dở sẽ không được hoàn tác.", workflow.FormatRef(id), oneLine(d.BehaviorName, 60), d.State)
	} else {
		action = toolWorkflowResume
		if !d.State.IsWaiting() {
			return w.reply(ctx, to, fmt.Sprintf("%s không ở trạng thái chờ (%s).", workflow.FormatRef(id), d.State), "not_resumable")
		}
		for _, wait := range d.Waits {
			p.WaitID = max(p.WaitID, wait.ID)
		}
		if p.WaitID <= 0 {
			return w.reply(ctx, to, workflow.FormatRef(id)+" không có lượt chờ để tiếp tục.", "not_resumable")
		}
		p.IdempotencyKey = fmt.Sprintf("telegram:resume:%s:%d:wait:%d", u.WorkspaceID, id, p.WaitID)
		text = fmt.Sprintf("Tiếp tục %s (%s · %s)?", workflow.FormatRef(id), oneLine(d.BehaviorName, 60), d.State)
	}
	return w.proposeAssistantAction(ctx, u, to, action, p, text)
}

func (w *InboxWorker) toolWorkflowNote(ctx context.Context, tt *assistantToolTurn, args toolArgs) (string, bool) {
	if tt.proposed == nil {
		tt.proposed = make(map[string]bool)
	}
	if tt.proposed[toolWorkflowNote] {
		return "A workflow instruction was already recorded in this answer.", true
	}
	id, err := args.num("id", 0, 1, int64(^uint64(0)>>1))
	if err != nil || id == 0 {
		return toolErr(errToolArgs)
	}
	text, err := args.str("text")
	text = sanitizeWorkflowInstruction(text)
	if err != nil || text == "" || utf8.RuneCountInString(text) > 2000 {
		return "Instruction text must contain 1 to 2,000 characters.", true
	}
	r := w.workflowReader()
	if r == nil || w.d.Conversations == nil {
		return toolMsgNotAvailable, true
	}
	conversation, err := w.ensureConversation(ctx, tt.to)
	if err != nil || conversation.ID <= 0 {
		return toolMsgFailed, true
	}
	refs := workflow.FindRefs(tt.ownerText)
	var targetID int64
	switch {
	case len(refs) == 1:
		targetID = refs[0]
	case len(refs) > 1:
		// Multiple owner references are ambiguous even when the model chooses one.
	case tt.workflowID > 0:
		targetID = tt.workflowID
	default:
		if t := w.topicTarget(ctx, tt.u); t.has() {
			// Inside a topic only the subject's single active execution is a
			// default target, never another task's workflow bound here.
			targetID, _ = t.single()
			break
		}
		bound := w.activeBoundWorkflows(ctx, tt.u.WorkspaceID, conversation.ID)
		if len(bound) == 1 {
			targetID = bound[0].ID
		}
	}
	if targetID != id {
		return "The workflow target is ambiguous. Ask the owner which WF-id to use; no instruction was recorded.", true
	}
	if _, err := r.GetExecutionDetail(ctx, tt.u.WorkspaceID, id); err != nil {
		return toolErr(err)
	}
	ins, err := w.d.Conversations.AppendInstruction(ctx, workflow.InstructionRequest{WorkspaceID: tt.u.WorkspaceID,
		ExecutionID: id, Text: text, ConversationID: conversation.ID, TelegramUpdateID: tt.u.UpdateID})
	if errors.Is(err, workflow.ErrNotEligible) {
		return workflow.FormatRef(id) + " is terminal and cannot accept an instruction.", true
	}
	if err != nil {
		return toolErr(err)
	}
	_ = w.d.Conversations.BindWorkflow(ctx, tt.u.WorkspaceID, conversation.ID, id, "assistant_instruction")
	tt.proposed[toolWorkflowNote] = true
	return fmt.Sprintf("%s: instruction accepted (v%d).", workflow.FormatRef(id), ins.Version), false
}

func sanitizeWorkflowInstruction(text string) string {
	text = strings.ToValidUTF8(text, "")
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	return strings.TrimSpace(clean)
}
