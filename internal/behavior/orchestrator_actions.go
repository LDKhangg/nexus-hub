package behavior

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file is the orchestrator's behavior-actions executor (Phase 10
// REQ-10) and the topic route of orchestrated worker jobs (REQ-12).
//
// The executor never writes to ClickUp itself. A clickup.write task becomes
// a ClickUp draft (REQ-7) on the execution's own ClickUp task, shown in the
// execution's topic with "Đăng" / "Sửa" / "Huỷ"; the plan task waits until
// the draft ends: posted is success (the comment id is the evidence),
// cancelled, expired or failed with no retry blocks the task and Evaluate
// decides. Once a draft's post outcome is unknown (it may be on ClickUp),
// every later clickup.write task of the execution is blocked without a
// draft, so a replan never leads to a second comment. A
// telegram.notify task sends one message to the execution's topic. The text
// is the plan task's title and the bounded results of its dependencies,
// which the model wrote; it is only data, and the owner sees the exact text
// before anything reaches ClickUp.

// Reason codes of behavior-actions tasks.
const (
	ReasonActionNoClickUpTask  = "no_clickup_task"
	ReasonActionUnsupported    = "unsupported_capability"
	ReasonActionInvalidText    = "invalid_input"
	ReasonActionPreviewNotSent = "preview_not_sent"
	ReasonActionDraftMissing   = "draft_missing"
	ReasonActionDraftCancelled = "draft_cancelled"
	ReasonActionDraftExpired   = "draft_expired"
	ReasonActionDraftFailed    = "draft_failed"
	ReasonActionNotifyFailed   = "notify_failed"
	ReasonActionInterrupted    = orchestrator.ReasonExecutorInterrupted

	// ReasonActionDraftUnknown: the draft's post may or may not have
	// reached ClickUp.
	ReasonActionDraftUnknown = "draft_outcome_unknown"
	// ReasonActionDraftUnknownEarlier: an earlier draft of the execution
	// has an unknown outcome, so no new draft is made.
	ReasonActionDraftUnknownEarlier = "draft_outcome_unknown_earlier"
)

// orchestratorActionMaxRunes bounds the text of one action (a ClickUp
// draft allows 8,000 runes with the bot marker).
const orchestratorActionMaxRunes = 4000

// draftWaitPrefix is the executor's wait reference of a draft.
const draftWaitPrefix = "draft:"

// OrchestratorDraftPort is what the executor needs of the ClickUp draft
// flow (ClickUpDrafts).
type OrchestratorDraftPort interface {
	Create(ctx context.Context, r DraftRequest) (int64, error)
	Get(ctx context.Context, workspaceID string, id int64) (ClickUpDraft, error)
	Cancel(ctx context.Context, workspaceID string, id int64) error
	// ForPlanTask returns the newest orchestrator draft of an execution's
	// plan task key; ErrNotAvailable when the store cannot look it up.
	ForPlanTask(ctx context.Context, workspaceID string, executionID int64, planTaskKey string) (ClickUpDraft, bool, error)
}

// OrchestratorSayer sends one message to the execution's topic
// (orchestrator.Owner.Say).
type OrchestratorSayer interface {
	Say(ctx context.Context, route orchestrator.Route, text string) error
}

// OrchestratorActions implements the behavior-actions executor.
type OrchestratorActions struct {
	Drafts OrchestratorDraftPort // nil: clickup.write tasks fail
	Owner  OrchestratorSayer     // nil: telegram.notify tasks fail
	// Chat is where a draft preview goes: the execution's topic. nil means
	// the route's own chat and thread.
	Chat func(ctx context.Context, route orchestrator.Route) ChatRef
	// Merge runs github.merge tasks (Phase 9); nil: github.merge is not
	// offered to Plan and its tasks fail with executor_unavailable.
	Merge *OrchestratorMerge
}

var (
	_ orchestrator.TaskExecutor = (*OrchestratorActions)(nil)
	_ orchestrator.TaskResumer  = (*OrchestratorActions)(nil)
)

// orchestratorDraftKey binds a draft to its run slot and plan task: one run
// slot runs one task, so the key never repeats in an execution.
func orchestratorDraftKey(run orchestrator.TaskRun) string {
	return run.StepKey + ":" + run.Task.Key
}

// orchestratorActionText is the action's text: the plan task's title and
// the results of its dependencies, bounded.
func orchestratorActionText(run orchestrator.TaskRun) string {
	text := strings.TrimSpace(run.Task.Title)
	if len(run.Inputs) > 0 {
		text += "\n\n" + strings.Join(run.Inputs, "\n")
	}
	return capRunes(strings.TrimSpace(text), orchestratorActionMaxRunes)
}

func actionFailed(reason string) orchestrator.TaskOutcome {
	return orchestrator.TaskOutcome{ReasonCode: reason}
}

// RunTask starts one behavior-actions task. A ClickUp write returns a wait
// on its draft; a notify finishes at once; a merge runs under the policy
// engine and may wait on the owner's approval (orchestrator_merge.go).
func (a *OrchestratorActions) RunTask(ctx context.Context, run orchestrator.TaskRun) (orchestrator.TaskOutcome, error) {
	switch {
	case slices.Contains(run.Task.Capabilities, orchestrator.CapGitHubMerge):
		return a.merge(ctx, run)
	case slices.Contains(run.Task.Capabilities, orchestrator.CapClickUpWrite):
		return a.draft(ctx, run)
	case slices.Contains(run.Task.Capabilities, orchestrator.CapTelegramNotify):
		return a.notify(ctx, run)
	}
	return actionFailed(ReasonActionUnsupported), nil
}

// ResumeTask follows a task that waits on its draft. A notify that was
// interrupted cannot tell whether its message went out: it is reported as
// interrupted, never sent twice.
func (a *OrchestratorActions) ResumeTask(ctx context.Context, run orchestrator.TaskRun, waitRef string) (orchestrator.TaskOutcome, error) {
	if slices.Contains(run.Task.Capabilities, orchestrator.CapGitHubMerge) {
		return a.resumeMerge(ctx, run, waitRef)
	}
	if !slices.Contains(run.Task.Capabilities, orchestrator.CapClickUpWrite) {
		return actionFailed(ReasonActionInterrupted), nil
	}
	if a.Drafts == nil {
		return actionFailed(ReasonActionInterrupted), nil
	}
	var (
		d     ClickUpDraft
		found bool
	)
	if waitRef != "" {
		id, err := strconv.ParseInt(strings.TrimPrefix(waitRef, draftWaitPrefix), 10, 64)
		if !strings.HasPrefix(waitRef, draftWaitPrefix) || err != nil || id <= 0 {
			return actionFailed(ReasonActionDraftMissing), nil
		}
		d, err = a.Drafts.Get(ctx, run.WorkspaceID, id)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return orchestrator.TaskOutcome{}, err
		default:
			found = true
		}
	} else {
		var err error
		d, found, err = a.Drafts.ForPlanTask(ctx, run.WorkspaceID, run.ExecutionID, orchestratorDraftKey(run))
		if errors.Is(err, ErrNotAvailable) {
			return actionFailed(ReasonActionInterrupted), nil
		}
		if err != nil {
			return orchestrator.TaskOutcome{}, err
		}
		if !found {
			// The interrupted attempt made no draft: make it now.
			return a.draft(ctx, run)
		}
	}
	if !found || d.Source != DraftSourceOrchestrator || d.ExecutionID != run.ExecutionID ||
		d.PlanTaskKey != orchestratorDraftKey(run) || d.TaskID != run.Route.ClickUpTaskID {
		return actionFailed(ReasonActionDraftMissing), nil
	}
	return draftOutcome(d), nil
}

// draftOutcome maps a draft's state to the plan task's outcome.
func draftOutcome(d ClickUpDraft) orchestrator.TaskOutcome {
	switch d.State {
	case DraftPosted:
		ref := "clickup:draft:" + strconv.FormatInt(d.ID, 10)
		if d.PostedCommentID != "" {
			ref = "clickup:comment:" + d.PostedCommentID
		}
		return orchestrator.TaskOutcome{
			Succeeded: true, Summary: "Đã đăng comment lên ClickUp.",
			Evidence: []orchestrator.EvidenceRef{{Kind: orchestrator.EvidenceArtifact, ID: ref}},
		}
	case DraftCancelled:
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonActionDraftCancelled}
	case DraftExpired:
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonActionDraftExpired}
	case DraftFailedFinal:
		// The post failed with no retry: dry run or denied keep their own
		// reason code; an unknown outcome has its own.
		reason := d.ReasonCode
		switch {
		case reason == OutcomeUnknown:
			reason = ReasonActionDraftUnknown
		case reason == "":
			reason = ReasonActionDraftFailed
		}
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: reason}
	}
	// pending, posting, or failed with "Thử lại" / "Huỷ" shown: the owner
	// still decides, and the sweep ends it at expiry or when the execution
	// ends.
	return orchestrator.TaskOutcome{Wait: draftWaitPrefix + strconv.FormatInt(d.ID, 10)}
}

// orchestratorUnknownChecker is the optional part of an
// OrchestratorDraftPort that tells whether an execution already has a draft
// with an unknown post outcome (ClickUpDrafts).
type orchestratorUnknownChecker interface {
	UnknownOutcomeEarlier(ctx context.Context, workspaceID string, executionID int64) (bool, error)
}

// draft makes the task's ClickUp draft on the execution's own task. The
// model never chooses the task: only the route's ClickUp task is used.
func (a *OrchestratorActions) draft(ctx context.Context, run orchestrator.TaskRun) (orchestrator.TaskOutcome, error) {
	if a.Drafts == nil {
		return actionFailed(orchestrator.ReasonExecutorUnavailable), nil
	}
	taskID := run.Route.ClickUpTaskID
	if !taskIDRe.MatchString(taskID) {
		return actionFailed(ReasonActionNoClickUpTask), nil
	}
	text := orchestratorActionText(run)
	if text == "" {
		return actionFailed(ReasonActionInvalidText), nil
	}
	if u, ok := a.Drafts.(orchestratorUnknownChecker); ok {
		unknown, err := u.UnknownOutcomeEarlier(ctx, run.WorkspaceID, run.ExecutionID)
		switch {
		case errors.Is(err, ErrNotAvailable):
		case err != nil:
			return orchestrator.TaskOutcome{}, err
		case unknown:
			return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonActionDraftUnknownEarlier}, nil
		}
	}
	chat := ChatRef{WorkspaceID: run.Route.WorkspaceID, IntegrationID: run.Route.IntegrationID, ChatID: run.Route.ChatID, ThreadID: run.Route.ThreadID}
	if a.Chat != nil {
		chat = a.Chat(ctx, run.Route)
	}
	id, err := a.Drafts.Create(ctx, DraftRequest{
		Chat: chat, TaskID: taskID, Text: text, Source: DraftSourceOrchestrator,
		ExecutionID: run.ExecutionID, PlanTaskKey: orchestratorDraftKey(run),
	})
	switch {
	case errors.Is(err, ErrInvalid):
		return actionFailed(ReasonActionInvalidText), nil
	case err != nil && id > 0:
		// The draft exists but the owner never saw it: end it now instead
		// of waiting for its expiry.
		if cerr := a.Drafts.Cancel(ctx, run.WorkspaceID, id); cerr != nil {
			log.Printf("orchestrator actions: execution %d draft %d not cancelled", run.ExecutionID, id)
		}
		return actionFailed(ReasonActionPreviewNotSent), nil
	case err != nil:
		return orchestrator.TaskOutcome{}, err
	}
	return orchestrator.TaskOutcome{Wait: draftWaitPrefix + strconv.FormatInt(id, 10)}, nil
}

// notify sends the task's text to the execution's topic.
func (a *OrchestratorActions) notify(ctx context.Context, run orchestrator.TaskRun) (orchestrator.TaskOutcome, error) {
	if a.Owner == nil {
		return actionFailed(orchestrator.ReasonExecutorUnavailable), nil
	}
	text := orchestratorActionText(run)
	if text == "" {
		return actionFailed(ReasonActionInvalidText), nil
	}
	if err := a.Owner.Say(ctx, run.Route, workflow.FormatRef(run.ExecutionID)+": "+text); err != nil {
		return actionFailed(ReasonActionNotifyFailed), nil
	}
	return orchestrator.TaskOutcome{
		Succeeded: true, Summary: "Đã gửi thông báo vào topic.",
		Evidence: []orchestrator.EvidenceRef{{Kind: orchestrator.EvidenceStepOutput, ID: fmt.Sprintf("telegram:wf%d:%s", run.ExecutionID, run.StepKey)}},
	}, nil
}

// OrchestratorDraftWaker returns the ClickUp draft listener that wakes the
// run step waiting on an orchestrator draft once it ended (posted,
// cancelled, expired, or failed with no retry). A missed wake is repaired by the worker task
// sweeper.
func OrchestratorDraftWaker(w LocalTaskWaker) ClickUpDraftListener {
	return func(ctx context.Context, o ClickUpDraftOutcome) {
		if w == nil || o.Source != DraftSourceOrchestrator || o.ExecutionID <= 0 {
			return
		}
		step, _, ok := strings.Cut(o.PlanTaskKey, ":")
		if !ok || step == "" {
			return
		}
		if err := w.WakeWorkerStep(ctx, o.WorkspaceID, o.ExecutionID, step); err != nil && !errors.Is(err, workflow.ErrNotEligible) {
			log.Print("orchestrator actions: waiting step not woken; the sweeper retries")
		}
	}
}

// --- worker job routes (REQ-12) ---

// OrchestratorWorkerRoutes stores the local-task route of an orchestrated
// execution's worker job (storage.WorkerTaskStore).
type OrchestratorWorkerRoutes interface {
	EnsureOrchestratedRoute(ctx context.Context, route LocalTaskRoute) error
}

var _ orchestrator.WorkerJobRoutes = OrchestratorJobs{}

// PrepareJob stores the job's topic route before the job is created, so the
// worker notifier sends its progress and Retry/Cancel to the execution's
// topic exactly like a /task job. A repo the route cannot hold is skipped.
func (a OrchestratorJobs) PrepareJob(ctx context.Context, route orchestrator.Route, _ string, repo string, instructionVersion int64) error {
	if a.Routes == nil || len(repo) < 3 {
		return nil
	}
	return a.Routes.EnsureOrchestratedRoute(ctx, LocalTaskRoute{
		WorkspaceID: route.WorkspaceID, ExecutionID: route.ExecutionID, IntegrationID: route.IntegrationID,
		ChatID: route.ChatID, ThreadID: route.ThreadID, Repo: repo, TaskRef: route.TaskRef,
		ClickUpTaskID: route.ClickUpTaskID, InstructionVersion: max(instructionVersion, 0),
	})
}

// --- ClickUp drafts lookup ---

// clickUpDraftFinder is the optional store lookup of orchestrator drafts.
type clickUpDraftFinder interface {
	OrchestratorDraft(ctx context.Context, workspaceID string, executionID int64, planTaskKey string) (ClickUpDraft, bool, error)
}

// ForPlanTask returns the newest draft an orchestrated execution made for a
// plan task key; ErrNotAvailable when the store has no such lookup.
func (c *ClickUpDrafts) ForPlanTask(ctx context.Context, workspaceID string, executionID int64, planTaskKey string) (ClickUpDraft, bool, error) {
	f, ok := c.d.Store.(clickUpDraftFinder)
	if !ok {
		return ClickUpDraft{}, false, ErrNotAvailable
	}
	return f.OrchestratorDraft(ctx, workspaceID, executionID, planTaskKey)
}
