package behavior

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file makes the blocking approvals of a durable behavior step
// durable waits of its execution.
//
// A step with approval waits enabled creates every blocking approval
// (item_answer, post_draft, confirm_action) through the wait store, which
// binds it to the step in the same transaction. Alias, preferences,
// feedback and assistant approvals are never bound and never block or wake
// an execution.
//
// When the step's own work ended and some bound approvals are still open,
// the step ends its attempt in a wait (WAITING_FOR_APPROVAL, or
// WAITING_FOR_USER when only answers are awaited) and holds no job lease.
// Deciding or expiring a bound approval records a signal in the same
// transaction and queues the step again. The woken step, under its job
// fence, applies each recorded decision exactly as the legacy callback
// would (post the draft, run the ask actions, apply the confirmed change,
// or record the skipped, canceled or expired outcome of that item only),
// marks the signal consumed, and waits again until every bound approval is
// decided. Existing action receipts keep a re-applied decision from
// repeating a write; a rejected or expired item's action never runs.

// CheckpointApprovalWait is the checkpoint reference of an open bound
// approval; ReferenceID is the approval ID.
const CheckpointApprovalWait = "approval_wait"

// StepWait is one approval bound to an execution step.
type StepWait struct {
	WaitID   int64
	Approval Approval // its decision and answer, as stored
	Input    bool     // a user-input wait (an ask's answer)
	SignalID int64    // the decision's signal; 0 while none is recorded
	Consumed bool     // the signal was applied
}

// ApprovalWaitStore persists the approval waits of durable steps
// (storage.BehaviorStore). Writes run under the step's job fence; a refused
// fence is ErrWorkflowFence.
type ApprovalWaitStore interface {
	// CreateWaitApproval creates a blocking approval of the fenced step's
	// run and binds it to the step in one transaction. ErrConflict as
	// CreateApproval.
	CreateWaitApproval(ctx context.Context, f RunFence, a NewApproval) (Approval, error)
	ListStepWaits(ctx context.Context, workspaceID string, executionID int64, stepKey string) ([]StepWait, error)
	// ConsumeWaitSignal marks an applied signal; a repeat is a no-op.
	ConsumeWaitSignal(ctx context.Context, f RunFence, signalID int64) error
}

// DecisionApplier applies a recorded decision of a bound approval
// (*Engine). It is idempotent: an item no longer awaiting the decision is
// left alone.
type DecisionApplier interface {
	ApplyDecision(ctx context.Context, ap Approval) error
}

var _ DecisionApplier = (*Engine)(nil)

// blockingApproval reports whether an approval of kind blocks its step.
func blockingApproval(kind ApprovalKind) bool {
	switch kind {
	case ApprovalItemAnswer, ApprovalPostDraft, ApprovalConfirmAction:
		return true
	}
	return false
}

type approvalBinderKey struct{}

// approvalBinder creates a blocking approval bound to the current step.
type approvalBinder func(ctx context.Context, na NewApproval) (Approval, error)

func withApprovalBinder(ctx context.Context, b approvalBinder) context.Context {
	return context.WithValue(ctx, approvalBinderKey{}, b)
}

func approvalBinderFrom(ctx context.Context) approvalBinder {
	b, _ := ctx.Value(approvalBinderKey{}).(approvalBinder)
	return b
}

// EnableApprovalWaits makes the steps' blocking approvals durable waits.
// Without it steps create unbound approvals decided on the legacy route.
func (s *WorkflowSteps) EnableApprovalWaits(waits ApprovalWaitStore, applier DecisionApplier) error {
	if s == nil || waits == nil || applier == nil {
		return errors.New("behavior workflow steps: missing approval wait dependency")
	}
	s.waits, s.applier = waits, applier
	return nil
}

// binder is the step's approval binder: creation under the job fence.
func (s *WorkflowSteps) binder(fence RunFence) approvalBinder {
	return func(ctx context.Context, na NewApproval) (Approval, error) {
		f := fence
		f.At = s.now()
		return s.waits.CreateWaitApproval(ctx, f, na)
	}
}

// waitContinuation reports whether the step continues an earlier wait: it
// has bound approvals and either ended an attempt in a wait or already
// recorded the run's end.
func (s *WorkflowSteps) waitContinuation(ctx context.Context, sc workflow.StepContext, run Run, st *durableState) (bool, error) {
	if s.waits == nil {
		return false, nil
	}
	waits, err := s.waits.ListStepWaits(ctx, sc.Execution.WorkspaceID, sc.Execution.ID, sc.Step.Key)
	if err != nil || len(waits) == 0 {
		return false, err
	}
	return st.priorHas(CheckpointApprovalWait) || run.Status == RunDone || run.Status == RunSkipped, nil
}

// continueWaits applies the recorded decisions of a woken step without
// starting another run attempt, then completes the step or waits again.
func (s *WorkflowSteps) continueWaits(ctx context.Context, sc workflow.StepContext, fence RunFence, st *durableState) (workflow.StepResult, error) {
	if st.prior != nil {
		for _, r := range st.prior.References {
			if r.Kind != CheckpointApprovalWait {
				st.add(r.Kind, r.ReferenceID, r.OperationKey)
			}
		}
	}
	actx, cancel := s.continuationContext(ctx, sc, fence)
	defer cancel()
	return s.settleWaits(ctx, actx, fence, st)
}

func (s *WorkflowSteps) continuationContext(ctx context.Context, sc workflow.StepContext, fence RunFence) (context.Context, context.CancelFunc) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if t := sc.Definition.StepTimeout; t > 0 {
		actx, cancel = context.WithTimeout(ctx, t-stepMargin(t))
	}
	return withApprovalBinder(withActionGate(actx, s.gate(fence)), s.binder(fence)), cancel
}

// settleWaits applies every recorded, unapplied decision of the step's
// bound approvals (each under the action gate), then returns success when
// none is open, or a wait on the open ones.
func (s *WorkflowSteps) settleWaits(ctx, actx context.Context, fence RunFence, st *durableState) (workflow.StepResult, error) {
	var open []StepWait
	inputOnly := true
	// Applying a decision may bind a new approval (an answer that asks for
	// a confirmation), so the waits are listed again until a pass applies
	// nothing.
	for applied := true; applied; {
		applied = false
		waits, err := s.waits.ListStepWaits(ctx, fence.WorkspaceID, fence.ExecutionID, fence.StepKey)
		if err != nil {
			return workflow.StepResult{}, storageErr(ctx, err)
		}
		open, inputOnly = nil, true
		for _, w := range waits {
			if w.SignalID == 0 {
				open = append(open, w)
				inputOnly = inputOnly && w.Input
				continue
			}
			if w.Consumed {
				continue
			}
			if err := s.applier.ApplyDecision(actx, w.Approval); err != nil {
				if ctx.Err() == nil && actx.Err() != nil {
					return workflow.StepResult{}, &workflow.StepError{Code: CodeRunTimeout, Retryable: true}
				}
				return workflow.StepResult{}, storageErr(ctx, err)
			}
			f := fence
			f.At = s.now()
			if err := s.waits.ConsumeWaitSignal(ctx, f, w.SignalID); err != nil {
				return workflow.StepResult{}, storageErr(ctx, err)
			}
			applied = true
			log.Printf("behavior run %d execution %d: approval %d decision applied", st.runID, fence.ExecutionID, w.Approval.ID)
		}
	}
	if len(open) == 0 {
		return workflow.StepResult{Checkpoint: st.checkpoint(true)}, nil
	}
	refs := append([]workflow.CheckpointReference(nil), st.refs...)
	for _, w := range open {
		refs = append(refs, workflow.CheckpointReference{Kind: CheckpointApprovalWait, ReferenceID: strconv.FormatInt(w.Approval.ID, 10)})
	}
	state := workflow.StateWaitingForApproval
	if inputOnly {
		state = workflow.StateWaitingForUser
	}
	log.Printf("behavior run %d execution %d: waiting on %d approvals", st.runID, fence.ExecutionID, len(open))
	return workflow.StepResult{
		Checkpoint:     &workflow.Checkpoint{References: refs},
		WaitState:      state,
		WaitReferences: refs,
	}, nil
}

// awaitDecisions turns a successful step result into a wait while bound
// approvals of the step are open (approval waits enabled only).
func (s *WorkflowSteps) awaitDecisions(ctx context.Context, sc workflow.StepContext, fence RunFence, st *durableState,
	res workflow.StepResult, err error) (workflow.StepResult, error) {
	if s.waits == nil || err != nil || res.WaitState != "" {
		return res, err
	}
	actx, cancel := s.continuationContext(ctx, sc, fence)
	defer cancel()
	return s.settleWaits(ctx, actx, fence, st)
}

// --- applying a decision ---

// ApplyDecision applies a recorded decision of a bound approval as the
// legacy callback would, once: an approved draft is posted, an answered ask
// runs its ask actions, a confirmed change is applied after a fresh read; a
// rejected or expired approval records that item's skipped, canceled or
// expired outcome and runs no action. Other items are untouched. A
// non-success reply is sent to the owner once per approval.
func (e *Engine) ApplyDecision(ctx context.Context, ap Approval) error {
	if ap.RunID == 0 || ap.ItemID == 0 || ap.Decision == "" || !blockingApproval(ap.Kind) {
		return nil
	}
	rc, err := e.loadRun(ctx, ap.WorkspaceID, ap.RunID)
	if err != nil {
		return err
	}
	it, err := e.d.Store.GetItem(ctx, ap.WorkspaceID, ap.ItemID)
	if err != nil {
		return err
	}
	if !awaitingDecision(ap, it) {
		return nil
	}
	switch ap.Decision {
	case DecisionRejected:
		_, err := e.applyReject(ctx, rc, ap)
		return err
	case DecisionExpired:
		return e.applyExpired(ctx, ap)
	}
	chat, err := e.d.Chats.OwnerChat(ctx, ap.WorkspaceID, rc.Trigger)
	if err != nil {
		return err
	}
	var reply string
	ok := true
	switch {
	case ap.Kind == ApprovalPostDraft && ap.Decision == DecisionApproved:
		reply, ok, err = e.applyPost(ctx, rc, ap, chat)
	case ap.Kind == ApprovalItemAnswer && ap.Decision == DecisionAnswered:
		reply, ok, err = e.applyAnswer(ctx, rc, ap, chat, ap.Answer)
	case ap.Kind == ApprovalConfirmAction && ap.Decision == DecisionApproved:
		reply, err = e.applyConfirm(ctx, rc, ap)
		ok = false // the legacy callback always shows a confirmation's result
	default:
		return nil
	}
	if err != nil {
		return err
	}
	if ok || strings.TrimSpace(reply) == "" {
		return nil
	}
	key := fmt.Sprintf("approval_outcome:%d", ap.ID)
	if _, err := e.sendOnce(ctx, rc.Run, key, chat, OutMessage{Text: capRunes(reply, maxTelegramRunes)}); errors.Is(err, ErrStaleClaim) {
		return err
	}
	return nil
}

// awaitingDecision reports whether the item still awaits the approval's
// decision: a draft still drafted, an ask still asked, or a confirmation
// with a change still awaiting confirmation.
func awaitingDecision(ap Approval, it Item) bool {
	switch ap.Kind {
	case ApprovalPostDraft:
		return it.Status == ItemDrafted
	case ApprovalItemAnswer:
		return it.Status == ItemAsked || it.Status == ItemDrafted
	case ApprovalConfirmAction:
		for _, o := range decodeResult(it.ActionResult).Actions {
			if o.Status == OutcomeAwaitingConfirm {
				return true
			}
		}
	}
	return false
}
