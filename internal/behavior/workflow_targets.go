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
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

const workflowTargetLifetime = 10 * time.Minute

type workflowTargetPayload struct {
	ChatID         int64   `json:"chat_id"`
	Kind           string  `json:"kind"`
	Text           string  `json:"text,omitempty"`
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
	if id, instruction, explicit := leadingWorkflowInstruction(trimmed); explicit {
		if len(workflow.FindRefs(trimmed)) != 1 {
			id = 0
		}
		return w.handleWorkflowRequest(ctx, u, to, actor, corr, "note", instruction, id)
	}
	if u.ReplyToMessageID > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "/") {
		if id, resolved := w.resolveWorkflowTarget(ctx, u, 0); resolved {
			if isWorkflowQuestion(trimmed) {
				return w.queueAssistantTextForWorkflow(ctx, u, to, actor, corr, text, id)
			}
			return w.executeWorkflowRequest(ctx, u, to, actor, corr, "note", trimmed, id)
		}
	}
	return w.queueAssistantText(ctx, u, to, actor, corr, text)
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
		return w.workflowList(ctx, u, to)
	case "/note":
		if args == "" {
			return w.reply(ctx, to, "Cú pháp: /note [WF-id] <nội dung>", "usage")
		}
		id, text := int64(0), args
		first, rest, found := strings.Cut(args, " ")
		if parsed, ok := workflow.ParseRef(first); ok {
			if !found || strings.TrimSpace(rest) == "" {
				return w.reply(ctx, to, "Cú pháp: /note [WF-id] <nội dung>", "usage")
			}
			id, text = parsed, strings.TrimSpace(rest)
			if len(workflow.FindRefs(args)) != 1 {
				id = 0
			}
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
	ids, err := w.d.Conversations.ExecutionsByResource(ctx, u.WorkspaceID, "telegram.message", fmt.Sprintf("%d:%d", u.ChatID, u.ReplyToMessageID))
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
	items := w.activeBoundWorkflows(ctx, u.WorkspaceID, state.id)
	if len(items) == 0 || w.d.Approvals == nil {
		return w.reply(ctx, to, "Không tìm thấy workflow đang hoạt động phù hợp trong cuộc trò chuyện này.", "workflow_target_not_found")
	}
	p := workflowTargetPayload{ChatID: u.ChatID, Kind: kind, Text: text, UpdateID: u.UpdateID,
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
		Nonce: nonce, ExpiresAt: w.now().Add(workflowTargetLifetime)})
	if err != nil {
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
	if json.Unmarshal(a.Payload, &p) != nil || p.ChatID != u.ChatID || p.ConversationID <= 0 ||
		data.Option < 0 || data.Option >= len(p.Candidates) {
		return msgStale, "stale", true
	}
	id := p.Candidates[data.Option]
	r := w.workflowReader()
	if r == nil {
		return msgStale, "stale", true
	}
	if _, err := r.GetExecutionDetail(ctx, u.WorkspaceID, id); err != nil {
		return msgStale, "stale", true
	}
	if _, err := w.d.Approvals.Decide(ctx, u.WorkspaceID, a.Nonce, DecisionApproved, workflow.FormatRef(id), actor, w.now()); err != nil {
		text, code := handlerText("", err)
		return text, code, code == "stale"
	}
	original := InboundUpdate{WorkspaceID: u.WorkspaceID, IntegrationID: u.IntegrationID, UpdateID: p.UpdateID,
		ChatID: u.ChatID, MessageID: p.MessageID, Kind: UpdateText}
	ctx = withConversation(ctx, p.ConversationID, UpdateText)
	out := w.executeWorkflowRequest(ctx, original, to, actor, corr, p.Kind, p.Text, id)
	if out.code == "" || out.code == "reply_failed" {
		return "Đã chọn " + workflow.FormatRef(id), out.code, true
	}
	return "Không xử lý được.", out.code, true
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
	text = strings.TrimSpace(text)
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
			Kind: "telegram.message", ExternalID: fmt.Sprintf("%d:%d", u.ChatID, u.MessageID)})
	}
	return w.reply(ctx, to, fmt.Sprintf("%s: đã nhận yêu cầu bổ sung (v%d)", workflow.FormatRef(id), ins.Version), "")
}

func (w *InboxWorker) workflowList(ctx context.Context, u InboundUpdate, to ChatRef) outcome {
	r := w.workflowReader()
	if r == nil {
		return w.reply(ctx, to, msgNotAvailable, "not_available")
	}
	items, _, err := r.ListExecutions(ctx, workflow.ExecutionQuery{WorkspaceID: u.WorkspaceID, Limit: 20})
	if err != nil {
		return w.reply(ctx, to, msgFailed, "handler_failed")
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if !item.State.IsTerminal() {
			lines = append(lines, fmt.Sprintf("%s · %s · %s", workflow.FormatRef(item.ID), oneLine(item.BehaviorName, 60), item.State))
		}
	}
	if len(lines) == 0 {
		return w.reply(ctx, to, "Không có workflow đang hoạt động.", "")
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
			latest := instructions[len(instructions)-1]
			lines = append(lines, fmt.Sprintf("Yêu cầu mới nhất: v%d · %s · %s", latest.Version,
				latest.CreatedAt.UTC().Format(time.RFC3339), oneLine(latest.Text, 240)))
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
	if err != nil || text == "" || utf8.RuneCountInString(text) > 2000 {
		return "Instruction text must contain 1 to 2,000 characters.", true
	}
	r := w.workflowReader()
	if r == nil || w.d.Conversations == nil {
		return toolMsgNotAvailable, true
	}
	if _, err := r.GetExecutionDetail(ctx, tt.u.WorkspaceID, id); err != nil {
		return toolErr(err)
	}
	conversation, err := w.ensureConversation(ctx, tt.to)
	if err != nil || conversation.ID <= 0 {
		return toolMsgFailed, true
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
