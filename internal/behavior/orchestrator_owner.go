package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file connects the orchestrated task steps (internal/orchestrator)
// to the hub's existing owner paths: messages go to the task topic, owner
// decisions are approvals of kind orchestrator_gate bound to the waiting
// step (decided by the buttons, expired by the expiry sweep; both record
// the signal that wakes the step), and worker jobs go through the 7a job
// store. Messages carry ids, fixed codes, titles and links only; none is
// logged.

// InstructionWaker is told after an owner instruction was added to an
// execution (orchestrator.InstructionWaker).
type InstructionWaker interface {
	InstructionAdded(ctx context.Context, workspaceID string, executionID int64) error
}

// OrchestratorGateStore persists the gates of orchestrated task steps
// (storage.BehaviorStore). Writes run under the step's job fence.
type OrchestratorGateStore interface {
	// CreateOrchestratorWaitApproval creates an orchestrator_gate approval
	// and binds it to the fenced step in one transaction.
	CreateOrchestratorWaitApproval(ctx context.Context, f RunFence, a NewApproval) (Approval, error)
	ListStepWaits(ctx context.Context, workspaceID string, executionID int64, stepKey string) ([]StepWait, error)
	ConsumeWaitSignal(ctx context.Context, f RunFence, signalID int64) error
	SetApprovalMessage(ctx context.Context, workspaceID string, id, messageID int64) error
}

// orchestratorTopics is the part of the topic router the owner adapter uses.
type orchestratorTopics interface {
	Destination(ctx context.Context, base ChatRef, in TopicIntent) ChatRef
	Recover(ctx context.Context, chat ChatRef, err error) (ChatRef, bool)
	NoteSent(c ChatRef)
}

// OrchestratorOwner implements orchestrator.Owner.
type OrchestratorOwner struct {
	Messenger Messenger
	Topics    orchestratorTopics // nil: messages go to the route chat
	Gates     OrchestratorGateStore
	Now       func() time.Time
	// ReportButton adds "📤 Đăng lên ClickUp" to the final report of an
	// execution on a ClickUp task (Phase 10 REQ-9).
	ReportButton bool
}

var _ orchestrator.Owner = (*OrchestratorOwner)(nil)

// NewOrchestratorOwner builds the adapter; topics may be nil.
func NewOrchestratorOwner(m Messenger, topics orchestratorTopics, gates OrchestratorGateStore) *OrchestratorOwner {
	return &OrchestratorOwner{Messenger: m, Topics: topics, Gates: gates}
}

func (o *OrchestratorOwner) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// orchestratorGatePayload is the payload of an orchestrator_gate approval.
type orchestratorGatePayload struct {
	ChatID      int64  `json:"chat_id"`
	ExecutionID int64  `json:"execution_id"`
	Purpose     string `json:"purpose"`
}

// send delivers to the task topic (else the workflow topic); a rejected
// topic send is retried once where the router says.
func (o *OrchestratorOwner) send(ctx context.Context, route orchestrator.Route, m OutMessage) (int64, error) {
	if o.Messenger == nil {
		return 0, ErrNotAvailable
	}
	chat := o.TopicChat(ctx, route)
	id, err := o.Messenger.SendMessage(ctx, chat, m)
	if err == nil {
		if o.Topics != nil {
			o.Topics.NoteSent(chat)
		}
		return id, nil
	}
	if chat.ThreadID != 0 && o.Topics != nil {
		if again, ok := o.Topics.Recover(context.WithoutCancel(ctx), chat, err); ok {
			if id, err = o.Messenger.SendMessage(ctx, again, m); err == nil {
				o.Topics.NoteSent(again)
				return id, nil
			}
		}
	}
	return 0, err
}

// TopicChat is where the owner messages of route go: the task topic, else
// the workflow topic, else the route chat. It sends nothing (a draft
// preview of the behavior-actions executor uses it, Phase 10 REQ-10).
func (o *OrchestratorOwner) TopicChat(ctx context.Context, route orchestrator.Route) ChatRef {
	chat := ChatRef{WorkspaceID: route.WorkspaceID, IntegrationID: route.IntegrationID, ChatID: route.ChatID, ThreadID: route.ThreadID}
	if o.Topics != nil {
		intent := TopicIntent{Mode: ReportTopicWorkflow, ExecutionID: route.ExecutionID}
		if route.ClickUpTaskID != "" {
			intent = TopicIntent{Mode: ReportTopicTask, ExecutionID: route.ExecutionID, TaskID: route.ClickUpTaskID}
		}
		chat = o.Topics.Destination(ctx, ChatRef{WorkspaceID: route.WorkspaceID, IntegrationID: route.IntegrationID, ChatID: route.ChatID}, intent)
	}
	return chat
}

// Say sends one message.
func (o *OrchestratorOwner) Say(ctx context.Context, route orchestrator.Route, text string) error {
	_, err := o.send(ctx, route, OutMessage{Text: capRunes(text, maxTelegramRunes)})
	return err
}

// SayFinal sends the final report; with ReportButton on and a ClickUp task
// it carries "📤 Đăng lên ClickUp".
func (o *OrchestratorOwner) SayFinal(ctx context.Context, route orchestrator.Route, text string) error {
	m := OutMessage{Text: capRunes(text, maxTelegramRunes)}
	if o.ReportButton && route.ClickUpTaskID != "" {
		if b, ok := ExecutionReportButton(route.ExecutionID); ok {
			m.Buttons = [][]Button{{b}}
		}
	}
	_, err := o.send(ctx, route, m)
	return err
}

var _ orchestrator.FinalSayer = (*OrchestratorOwner)(nil)

func runFence(f orchestrator.Fence) RunFence {
	return RunFence{JobID: f.JobID, WorkspaceID: f.WorkspaceID, ExecutionID: f.ExecutionID, StepKey: f.StepKey,
		Attempt: f.Attempt, ClaimToken: f.ClaimToken, At: f.At}
}

// AskGate binds an orchestrator_gate approval to the step and sends it
// with its two buttons. A message that cannot be sent leaves the gate open;
// it still expires.
func (o *OrchestratorOwner) AskGate(ctx context.Context, f orchestrator.Fence, route orchestrator.Route, g orchestrator.Gate) error {
	if o.Gates == nil {
		return ErrNotAvailable
	}
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(orchestratorGatePayload{ChatID: route.ChatID, ExecutionID: f.ExecutionID, Purpose: g.Purpose})
	ap, err := o.Gates.CreateOrchestratorWaitApproval(ctx, runFence(f), NewApproval{
		Kind: ApprovalOrchestratorGate, Action: g.Purpose, Resource: fmt.Sprintf("workflow:%d", f.ExecutionID),
		Payload: payload, Nonce: nonce, ExpiresAt: o.now().Add(g.TTL),
	})
	if err != nil {
		return err
	}
	approve, err1 := (CallbackData{Kind: CallbackApprove, Nonce: ap.Nonce, Option: -1}).Encode()
	reject, err2 := (CallbackData{Kind: CallbackReject, Nonce: ap.Nonce, Option: -1}).Encode()
	if err1 != nil || err2 != nil {
		log.Print("orchestrator gate: buttons not encoded")
		return nil
	}
	id, err := o.send(ctx, route, OutMessage{Text: capRunes(g.Text, maxTelegramRunes), Buttons: [][]Button{{{Text: g.Approve, Data: approve}, {Text: g.Reject, Data: reject}}}})
	if err != nil {
		log.Print("orchestrator gate: message not sent; the gate stays open until it expires")
		return nil
	}
	if err := o.Gates.SetApprovalMessage(ctx, route.WorkspaceID, ap.ID, id); err != nil {
		log.Print("orchestrator gate: button message not saved")
	}
	return nil
}

// GateWaits lists the step's orchestrator gates.
func (o *OrchestratorOwner) GateWaits(ctx context.Context, workspaceID string, executionID int64, stepKey string) ([]orchestrator.GateWait, error) {
	if o.Gates == nil {
		return nil, ErrNotAvailable
	}
	waits, err := o.Gates.ListStepWaits(ctx, workspaceID, executionID, stepKey)
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.GateWait, 0, len(waits))
	for _, w := range waits {
		if w.Approval.Kind != ApprovalOrchestratorGate {
			continue
		}
		out = append(out, orchestrator.GateWait{
			ApprovalID: w.Approval.ID, Purpose: w.Approval.Action, Decision: string(w.Approval.Decision),
			SignalID: w.SignalID, Consumed: w.Consumed,
		})
	}
	return out, nil
}

// ConsumeGate marks a gate's signal applied.
func (o *OrchestratorOwner) ConsumeGate(ctx context.Context, f orchestrator.Fence, signalID int64) error {
	if o.Gates == nil {
		return ErrNotAvailable
	}
	return o.Gates.ConsumeWaitSignal(ctx, runFence(f), signalID)
}

// orchestratorGateCallback decides an orchestrator gate. Only a press in
// the chat the gate was sent to counts; the decision records the signal
// that wakes the step, which applies it under its job fence.
func (w *InboxWorker) orchestratorGateCallback(ctx context.Context, u InboundUpdate, data CallbackData, a Approval, actor string) (string, string, bool) {
	var p orchestratorGatePayload
	if json.Unmarshal(a.Payload, &p) != nil || a.WorkspaceID != u.WorkspaceID || p.ChatID != u.ChatID || p.ExecutionID <= 0 {
		return msgStale, "stale", true
	}
	if data.Kind != CallbackApprove && data.Kind != CallbackReject {
		return msgNotAvailable, "not_available", false
	}
	decision, toast := DecisionApproved, "Đã ghi nhận, đang chạy tiếp."
	if strings.HasPrefix(p.Purpose, "budget_") && data.Kind == CallbackApprove {
		toast = "Đã thêm 1 vòng."
	}
	if data.Kind == CallbackReject {
		decision, toast = DecisionRejected, "Đã huỷ."
	}
	if _, err := w.d.Approvals.Decide(ctx, u.WorkspaceID, a.Nonce, decision, "", actor, w.now()); err != nil {
		text, code := handlerText("", err)
		return text, code, code == "stale"
	}
	log.Printf("orchestrator gate %d: decision recorded for %s", a.ID, workflow.FormatRef(p.ExecutionID))
	return toast, "", true
}

// noteInstructionAdded wakes a step waiting for an answer; failures only
// delay the answer until the next wake or /resume.
func (w *InboxWorker) noteInstructionAdded(ctx context.Context, workspaceID string, executionID int64) {
	if w.d.InstructionWaker == nil {
		return
	}
	if err := w.d.InstructionWaker.InstructionAdded(ctx, workspaceID, executionID); err != nil {
		log.Print("workflow instruction: waiting step not woken")
	}
}

// --- worker jobs and repos for orchestrated tasks ---

// OrchestratorJobs adapts the 7a job store to orchestrator.WorkerJobs. With
// Routes and Queued set, an orchestrated job gets the topic messages of a
// 7a /task job (Phase 10 REQ-12): its route is stored before the job is
// created, and the "waiting for a machine" notice follows the creation.
type OrchestratorJobs struct {
	Jobs   LocalTaskJobs
	Routes OrchestratorWorkerRoutes // nil: no topic messages for the job
	Queued WorkerJobQueued          // nil: no waiting notice
}

var _ orchestrator.WorkerJobs = OrchestratorJobs{}

func orchestratorJob(j WorkerJob) orchestrator.WorkerJob {
	out := orchestrator.WorkerJob{ID: j.ID, State: j.State}
	if r := j.Result; r != nil {
		out.ReasonCode, out.Branch, out.PRURL, out.TestOutcome, out.Summary = r.ReasonCode, r.Branch, r.PRURL, r.TestOutcome, r.Summary
	}
	return out
}

func (a OrchestratorJobs) JobForStep(ctx context.Context, workspaceID string, executionID int64, stepKey string) (orchestrator.WorkerJob, bool, error) {
	j, ok, err := a.Jobs.JobForStep(ctx, workspaceID, executionID, stepKey)
	if err != nil || !ok {
		return orchestrator.WorkerJob{}, ok, err
	}
	return orchestratorJob(j), true, nil
}

func (a OrchestratorJobs) CreateJob(ctx context.Context, workspaceID string, executionID int64, stepKey string, caps []string, payload json.RawMessage) (orchestrator.WorkerJob, error) {
	j, err := a.Jobs.CreateJob(ctx, workspaceID, executionID, stepKey, caps, payload)
	if err != nil {
		return orchestrator.WorkerJob{}, err
	}
	if a.Queued != nil {
		a.Queued.WorkerJobQueued(ctx, j)
	}
	return orchestratorJob(j), nil
}

// OrchestratorRepos adapts the local-task starter to orchestrator.RepoSource.
type OrchestratorRepos struct {
	Starter LocalTaskStarter
	Memory  LocalTaskRepoMemory // nil: no remembered repo
}

var _ orchestrator.RepoSource = OrchestratorRepos{}

func (a OrchestratorRepos) RegisteredRepos(ctx context.Context, workspaceID string) ([]string, error) {
	if a.Starter == nil {
		return nil, errors.New("orchestrator repos: no starter")
	}
	return a.Starter.RegisteredRepos(ctx, workspaceID)
}

func (a OrchestratorRepos) LastTaskRepo(ctx context.Context, workspaceID, clickUpTaskID string) (string, error) {
	if a.Memory == nil {
		return "", nil
	}
	return a.Memory.LastTaskRepo(ctx, workspaceID, clickUpTaskID)
}
