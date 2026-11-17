package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file is the hub side of "/task": the built-in workflow local_task
// (one worker.run step), the step executor that hands one job to the worker
// bridge and waits for it, and the source of follow-up instructions that
// the lease protocol delivers with a heartbeat (Phase 7a, W-4).
//
// A local task waits in WAITING_FOR_USER while its job is anywhere but
// succeeded or cancelled: a failed, interrupted or needs-input job keeps the
// execution open so the owner can press Retry or Cancel in the topic. The
// step finishes only when the job succeeds (execution COMPLETED) or is
// cancelled (execution FAILED with reason worker_cancelled).
//
// The instruction text never enters workflow JSON: it is a Phase 6
// instruction row of the execution, read when the job is created, and the
// job payload carries it to the laptop only.

const (
	// InternalLocalTaskKind is the internal definition kind of local tasks.
	InternalLocalTaskKind = "local_task"
	// WorkflowSourceLocalTask is the execution source kind of local tasks.
	WorkflowSourceLocalTask = "local_task"
	// StepKeyWorkerRun is the only step of the local-task definition.
	StepKeyWorkerRun = "run"
	// CheckpointWorkerJob references the worker job of a step; ReferenceID
	// is the job id.
	CheckpointWorkerJob = "worker_job"

	localTaskStartDelay      = 2 * time.Second
	maxLocalInstructionBytes = 8 << 10
	maxLocalInstructionRunes = 2000
)

// Step failure codes of local tasks.
const (
	CodeWorkerCancelled    = "worker_cancelled"
	CodeInstructionPending = "instruction_pending"
	CodeLocalTaskRoute     = "local_task_route_missing"
)

// KnownLocalTaskReasons returns every code a local task step exposes as a
// workflow retry or failure reason.
func KnownLocalTaskReasons() []workflow.ReasonCode {
	return []workflow.ReasonCode{
		CodeWorkerCancelled, CodeInstructionPending, CodeLocalTaskRoute,
		CodeWorkflowReference, CodeStorageUnavailable,
	}
}

// DefaultLocalTaskCapabilities are the capabilities a device needs to take a
// local task. The device profile is set at enrollment (--capabilities).
var DefaultLocalTaskCapabilities = []string{"repo.write"}

// LocalTaskDefinition is the immutable orchestration of the local-task
// definition. The step only creates the job and re-reads its state, so its
// timeout is short; the wait for the worker is a durable workflow wait.
func LocalTaskDefinition() workflow.Definition {
	return workflow.Definition{
		Steps:         []workflow.StepDefinition{{Key: StepKeyWorkerRun, Kind: workflow.StepKindWorkerRun}},
		Retry:         workflow.RetryPolicy{MaxAttempts: 3, Delays: []time.Duration{5 * time.Second, 30 * time.Second}},
		StepTimeout:   2 * time.Minute,
		LeaseDuration: 5 * time.Minute,
	}
}

// LocalTaskSnapshot is the document of the internal local-task version.
type LocalTaskSnapshot struct {
	Schema   int                 `json:"schema"`
	Kind     string              `json:"kind"`
	Workflow workflow.Definition `json:"workflow"`
}

// NewLocalTaskSnapshot snapshots the current definition.
func NewLocalTaskSnapshot() LocalTaskSnapshot {
	return LocalTaskSnapshot{Schema: 1, Kind: InternalLocalTaskKind, Workflow: LocalTaskDefinition()}
}

// LocalTaskRequest asks to start one local task. TaskRef is the id the owner
// wrote (for example FC-6583), ClickUpTaskID the canonical ClickUp id.
type LocalTaskRequest struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	UpdateID      int64
	Repo          string
	TaskRef       string
	ClickUpTaskID string
}

// TriggerKey is the idempotency key of the task: one execution per inbound
// update, so a redelivered command never starts a second job.
func (r LocalTaskRequest) TriggerKey() string {
	return fmt.Sprintf("local_task:%s:%d:%d", r.IntegrationID, r.ChatID, r.UpdateID)
}

// LocalTaskStartRequest builds the workflow start request. The first job
// becomes due shortly after the start so the owner's instruction row is
// stored before the step reads it.
func LocalTaskStartRequest(r LocalTaskRequest, versionID int64, correlationID string, now time.Time) workflow.StartRequest {
	return workflow.StartRequest{
		WorkspaceID: r.WorkspaceID, VersionID: versionID, TriggerKey: r.TriggerKey(), CorrelationID: correlationID,
		Definition: LocalTaskDefinition(),
		References: workflow.References{
			SourceKind: WorkflowSourceLocalTask, SourceID: fmt.Sprintf("%d:%d", r.ChatID, r.UpdateID),
			Subject: "clickup:task:" + r.ClickUpTaskID, OperationKey: r.TriggerKey(),
		},
		At: now.Add(localTaskStartDelay),
	}
}

// LocalTaskRoute is where the messages of one local task go and what it
// works on. It holds ids and counters only, never the instruction.
type LocalTaskRoute struct {
	WorkspaceID        string
	ExecutionID        int64
	IntegrationID      string
	ChatID             int64
	ThreadID           int64
	Repo               string
	TaskRef            string
	ClickUpTaskID      string
	ProgressMessageID  int64
	ProgressAttempt    int
	WaitingNotice      string
	InstructionVersion int64
}

// LocalTaskStarter starts local tasks and lists the repositories the hub
// knows (registered with nh github-repo). ErrNotAvailable when the
// workspace does not run on the durable workflow runtime.
type LocalTaskStarter interface {
	StartLocalTask(ctx context.Context, req LocalTaskRequest) (workflow.Execution, error)
	RegisteredRepos(ctx context.Context, workspaceID string) ([]string, error)
}

// LocalTaskRepoMemory is implemented by a LocalTaskStarter that remembers
// the repository of a ClickUp task's last local task; "" when none.
type LocalTaskRepoMemory interface {
	LastTaskRepo(ctx context.Context, workspaceID, clickUpTaskID string) (string, error)
}

// LocalTaskRoutes persists the routes. GetRoute is ErrNotFound when the
// execution is not a local task.
type LocalTaskRoutes interface {
	GetRoute(ctx context.Context, workspaceID string, executionID int64) (LocalTaskRoute, error)
	SetProgressMessage(ctx context.Context, workspaceID string, executionID, messageID int64, attempt int, threadID int64) error
	SetWaitingNotice(ctx context.Context, workspaceID string, executionID int64, notice string) error
	SetInstructionVersion(ctx context.Context, workspaceID string, executionID, version int64) error
}

// LocalTaskJobs is what the step executor needs of the job store.
type LocalTaskJobs interface {
	CreateJob(ctx context.Context, workspaceID string, executionID int64, stepID string, requiredCaps []string, payload json.RawMessage) (WorkerJob, error)
	// JobForStep returns the newest job of the step; ok=false when none.
	JobForStep(ctx context.Context, workspaceID string, executionID int64, stepID string) (job WorkerJob, ok bool, err error)
}

// WorkerJobControl is the owner's control of a job: Cancel and Retry buttons,
// /retry, /status.
type WorkerJobControl interface {
	CancelJob(ctx context.Context, workspaceID, jobID string) (WorkerJob, error)
	GetJob(ctx context.Context, workspaceID, jobID string) (WorkerJob, error)
	JobForStep(ctx context.Context, workspaceID string, executionID int64, stepID string) (job WorkerJob, ok bool, err error)
	// RetryJob queues a FAILED, NEEDS_INPUT or INTERRUPTED job again when it
	// is still on the given attempt and its workflow execution is still
	// active; otherwise ErrConflict (ErrWorkerExecutionEnded, which wraps
	// it, when only the ended execution stands in the way).
	RetryJob(ctx context.Context, workspaceID, jobID string, attempt int) (WorkerJob, error)
}

// ErrWorkerExecutionEnded: the job could be retried but its workflow
// execution already ended. It wraps ErrConflict.
var ErrWorkerExecutionEnded = fmt.Errorf("%w: workflow execution ended", ErrConflict)

// msgWorkerExecutionEnded is the owner-facing text for a Retry refused
// because the workflow already ended.
const msgWorkerExecutionEnded = "Quy trình của việc này đã kết thúc nên không thể thử lại. Hãy gửi /task mới."

// LocalTaskWaker queues a waiting worker.run step again after its job ended.
type LocalTaskWaker interface {
	WakeWorkerStep(ctx context.Context, workspaceID string, executionID int64, stepID string) error
}

// WorkerJobQueued is told when the step created its job, so the topic can say
// that the task waits for a machine.
type WorkerJobQueued interface {
	WorkerJobQueued(ctx context.Context, job WorkerJob)
}

// WorkerRunStep executes the worker.run step.
type WorkerRunStep struct {
	Jobs         LocalTaskJobs
	Routes       LocalTaskRoutes
	Instructions workflow.InstructionReader
	Queued       WorkerJobQueued
	Capabilities []string
}

// Register adds the step kind to the registry.
func (s *WorkerRunStep) Register(reg *workflow.Registry) error {
	if s == nil || s.Jobs == nil || s.Routes == nil || s.Instructions == nil {
		return workflow.ErrInvalidInput
	}
	return reg.Register(workflow.StepKindWorkerRun, s)
}

// localJobPayload is the payload the laptop worker parses (internal/worker
// JobPayload). InstructionVersion is the highest instruction version it
// contains; the worker ignores the key.
type localJobPayload struct {
	Repo               string `json:"repo"`
	TaskID             string `json:"task_id"`
	ClickUpTaskID      string `json:"clickup_task_id"`
	Title              string `json:"title"`
	Instruction        string `json:"instruction"`
	InstructionVersion int64  `json:"instruction_version"`
}

func localStepFail(code string, retryable bool) error {
	return &workflow.StepError{Code: workflow.ReasonCode(code), Retryable: retryable}
}

// Execute creates the step's job once and then follows it: a succeeded job
// completes the step, a cancelled one fails it, anything else keeps waiting.
func (s *WorkerRunStep) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	ws, id, key := sc.Execution.WorkspaceID, sc.Execution.ID, sc.Step.Key
	job, ok, err := s.Jobs.JobForStep(ctx, ws, id, key)
	if err != nil {
		return workflow.StepResult{}, localStepFail(CodeStorageUnavailable, true)
	}
	if !ok {
		job, err = s.createJob(ctx, ws, id, key)
		if err != nil {
			return workflow.StepResult{}, err
		}
		if s.Queued != nil {
			s.Queued.WorkerJobQueued(ctx, job)
		}
	}
	ref := workflow.CheckpointReference{Kind: CheckpointWorkerJob, ReferenceID: job.ID}
	switch job.State {
	case WorkerJobSucceeded:
		return workflow.StepResult{Checkpoint: &workflow.Checkpoint{References: []workflow.CheckpointReference{ref}}}, nil
	case WorkerJobCancelled:
		return workflow.StepResult{}, localStepFail(CodeWorkerCancelled, false)
	}
	return workflow.StepResult{WaitState: workflow.StateWaitingForUser, WaitReferences: []workflow.CheckpointReference{ref}}, nil
}

func (s *WorkerRunStep) createJob(ctx context.Context, ws string, id int64, key string) (WorkerJob, error) {
	route, err := s.Routes.GetRoute(ctx, ws, id)
	if errors.Is(err, ErrNotFound) {
		return WorkerJob{}, localStepFail(CodeLocalTaskRoute, false)
	}
	if err != nil {
		return WorkerJob{}, localStepFail(CodeStorageUnavailable, true)
	}
	list, err := s.Instructions.ListInstructions(ctx, ws, id)
	if err != nil {
		return WorkerJob{}, localStepFail(CodeStorageUnavailable, true)
	}
	if len(list) == 0 {
		// The owner's text is stored right after the start; try again.
		return WorkerJob{}, localStepFail(CodeInstructionPending, true)
	}
	text, version := joinInstructions(list)
	display := route.TaskRef
	if display == "" {
		display = route.ClickUpTaskID
	}
	payload, err := json.Marshal(localJobPayload{
		Repo: route.Repo, TaskID: display, ClickUpTaskID: route.ClickUpTaskID, Instruction: text, InstructionVersion: version,
	})
	if err != nil {
		return WorkerJob{}, localStepFail(CodeWorkflowReference, false)
	}
	caps := s.Capabilities
	if caps == nil {
		caps = DefaultLocalTaskCapabilities
	}
	job, err := s.Jobs.CreateJob(ctx, ws, id, key, caps, payload)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return WorkerJob{}, localStepFail(CodeWorkflowReference, false)
		}
		return WorkerJob{}, localStepFail(CodeStorageUnavailable, true)
	}
	if err := s.Routes.SetInstructionVersion(ctx, ws, id, version); err != nil {
		// The job exists; a later heartbeat may repeat an instruction at worst.
		return job, nil
	}
	return job, nil
}

// joinInstructions joins the instruction texts in version order, newest
// versions last, within the payload bound. It returns the highest version
// included.
func joinInstructions(list []workflow.Instruction) (string, int64) {
	ordered := append([]workflow.Instruction(nil), list...)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].Version < ordered[j-1].Version; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	parts := make([]string, 0, len(ordered))
	var version int64
	for _, in := range ordered {
		t := strings.TrimSpace(in.Text)
		if t == "" {
			continue
		}
		parts = append(parts, t)
		version = max(version, in.Version)
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > maxLocalInstructionBytes {
		text = strings.ToValidUTF8(text[:maxLocalInstructionBytes], "")
	}
	return text, version
}

// LocalTaskInstructions delivers the owner's follow-up text of a running
// local task to the worker with its heartbeat (WorkerInstructionSource). An
// instruction is delivered once: the route keeps the highest version given.
type LocalTaskInstructions struct {
	Routes LocalTaskRoutes
	Reader workflow.InstructionReader
}

var _ WorkerInstructionSource = (*LocalTaskInstructions)(nil)

// PendingWorkerInstructions returns the instructions newer than the last
// acknowledged version, oldest first. It does not mark them delivered: the
// hub resends them with every heartbeat until the worker acknowledges them
// (AckWorkerInstructions); the worker drops repeats by id.
func (s *LocalTaskInstructions) PendingWorkerInstructions(ctx context.Context, job WorkerJob) ([]WorkerInstruction, error) {
	if s == nil || job.ExecutionID <= 0 {
		return nil, nil
	}
	route, err := s.Routes.GetRoute(ctx, job.WorkspaceID, job.ExecutionID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := s.Reader.ListInstructions(ctx, job.WorkspaceID, job.ExecutionID)
	if err != nil {
		return nil, err
	}
	var out []WorkerInstruction
	for _, in := range list {
		text := strings.TrimSpace(in.Text)
		if in.Version <= route.InstructionVersion || text == "" {
			continue
		}
		out = append(out, WorkerInstruction{ID: "v" + strconv.FormatInt(in.Version, 10), Text: text})
	}
	if len(out) == 0 {
		return nil, nil
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && instructionNumber(out[j].ID) < instructionNumber(out[j-1].ID); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// AckWorkerInstructions moves the delivered version of the job's route to
// the highest acknowledged id that names an existing instruction (ids of
// the form "v<version>"; others are ignored). The version only moves
// forward.
func (s *LocalTaskInstructions) AckWorkerInstructions(ctx context.Context, job WorkerJob, ids []string) error {
	if s == nil || job.ExecutionID <= 0 || len(ids) == 0 {
		return nil
	}
	acked := map[int64]bool{}
	for _, id := range ids {
		if n := instructionNumber(id); n > 0 && strings.HasPrefix(id, "v") {
			acked[n] = true
		}
	}
	if len(acked) == 0 {
		return nil
	}
	if _, err := s.Routes.GetRoute(ctx, job.WorkspaceID, job.ExecutionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	list, err := s.Reader.ListInstructions(ctx, job.WorkspaceID, job.ExecutionID)
	if err != nil {
		return err
	}
	var highest int64
	for _, in := range list {
		if acked[in.Version] {
			highest = max(highest, in.Version)
		}
	}
	if highest == 0 {
		return nil
	}
	return s.Routes.SetInstructionVersion(ctx, job.WorkspaceID, job.ExecutionID, highest)
}

func instructionNumber(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(id, "v"), 10, 64)
	return n
}

// WorkerJobStateText is the owner-facing (Vietnamese) state of a job.
func WorkerJobStateText(state string) string {
	switch state {
	case WorkerJobWaiting:
		return "chờ máy local"
	case WorkerJobLeased:
		return "máy local đã nhận"
	case WorkerJobRunning:
		return "đang chạy"
	case WorkerJobCancelling:
		return "đang huỷ"
	case WorkerJobInterrupted:
		return "bị gián đoạn"
	case WorkerJobSucceeded:
		return "hoàn tất"
	case WorkerJobFailed:
		return "thất bại"
	case WorkerJobNeedsInput:
		return "cần thêm thông tin"
	case WorkerJobCancelled:
		return "đã huỷ"
	}
	return state
}

// WorkerTaskUpkeep repairs the link between worker jobs and their workflow
// steps (a missed wake-up, a job of an ended execution).
type WorkerTaskUpkeep interface {
	SyncWorkerTasks(ctx context.Context) error
}

// WorkerTaskSweeper runs WorkerTaskUpkeep on an interval.
type WorkerTaskSweeper struct {
	Store    WorkerTaskUpkeep
	Interval time.Duration // default 15 s
}

// Run syncs until ctx ends. Errors are logged without detail and retried.
func (w *WorkerTaskSweeper) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.Store.SyncWorkerTasks(ctx); err != nil && ctx.Err() == nil {
			log.Print("worker tasks sweeper: sync failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
