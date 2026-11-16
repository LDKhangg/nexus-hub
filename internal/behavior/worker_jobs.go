package behavior

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Lease protocol constants (W6, W7).
const (
	WorkerPollMaxWait       = 55 * time.Second
	WorkerLeaseDuration     = 10 * time.Minute
	WorkerHeartbeatInterval = 20 * time.Second
	WorkerResumeWindow      = 24 * time.Hour
	WorkerTelemetryRetain   = 30 * 24 * time.Hour
	WorkerMaxPayloadBytes   = 16 << 10
	WorkerMaxSummaryRunes   = 2000
)

// Worker job states.
const (
	WorkerJobWaiting     = "WAITING_FOR_WORKER"
	WorkerJobLeased      = "LEASED"
	WorkerJobRunning     = "RUNNING"
	WorkerJobCancelling  = "CANCELLING"
	WorkerJobInterrupted = "INTERRUPTED"
	WorkerJobSucceeded   = "SUCCEEDED"
	WorkerJobFailed      = "FAILED"
	WorkerJobNeedsInput  = "NEEDS_INPUT"
	WorkerJobCancelled   = "CANCELLED"
)

// WorkerJobFinished reports whether state is terminal.
func WorkerJobFinished(state string) bool {
	switch state {
	case WorkerJobSucceeded, WorkerJobFailed, WorkerJobNeedsInput, WorkerJobCancelled:
		return true
	}
	return false
}

// WorkerJobActive reports whether a device holds a live lease in state.
func WorkerJobActive(state string) bool {
	switch state {
	case WorkerJobLeased, WorkerJobRunning, WorkerJobCancelling:
		return true
	}
	return false
}

// Worker audit event kinds (worker_events.kind). Reason codes are fixed
// snake_case codes; never free text.
const (
	WorkerEventJobCreated      = "job_created"
	WorkerEventLeased          = "leased"
	WorkerEventLeaseReissued   = "lease_reissued"
	WorkerEventResumed         = "resumed"
	WorkerEventLeaseLost       = "lease_lost"
	WorkerEventCancelRequested = "cancel_requested"
	WorkerEventCancelled       = "cancelled"
	WorkerEventResultPrefix    = "result_" // + status
	WorkerEventRejected        = "rejected"
	WorkerEventOffline         = "offline"
)

// Reason codes of rejected worker requests.
const (
	WorkerReasonStaleLease   = "stale_lease"
	WorkerReasonForeignJob   = "foreign_job"
	WorkerReasonLateResume   = "late_resume"
	WorkerReasonFinished     = "job_finished"
	WorkerReasonResultClash  = "result_conflict"
	WorkerReasonDeviceBusy   = "device_busy"
	WorkerReasonOwnerCancel  = "owner_cancelled"
	WorkerReasonLeaseExpired = "lease_expired"
	WorkerReasonNoContact    = "no_contact"
)

// WorkerProgress is one bounded progress event: a fixed phase and counts,
// never code, prompts or free text.
type WorkerProgress struct {
	Seq    int64                `json:"seq"`
	Phase  string               `json:"phase"`
	Counts WorkerProgressCounts `json:"counts"`
	At     time.Time            `json:"at"`
}

// WorkerProgressCounts are the only numbers a progress event carries.
type WorkerProgressCounts struct {
	FilesChanged int64 `json:"files_changed"`
	LinesAdded   int64 `json:"lines_added"`
	LinesRemoved int64 `json:"lines_removed"`
	TestsPassed  int64 `json:"tests_passed"`
	TestsFailed  int64 `json:"tests_failed"`
	Turns        int64 `json:"turns"`
}

var workerPhases = map[string]bool{"cloning": true, "editing": true, "testing": true, "pushing": true}

const workerMaxCount = 10_000_000

// Validate checks the phase and the count ranges.
func (p WorkerProgress) Validate() error {
	if p.Seq < 1 || p.Seq > 1<<53 {
		return fmt.Errorf("%w: seq", ErrInvalid)
	}
	if !workerPhases[p.Phase] {
		return fmt.Errorf("%w: phase", ErrInvalid)
	}
	c := p.Counts
	for _, v := range []int64{c.FilesChanged, c.LinesAdded, c.LinesRemoved, c.TestsPassed, c.TestsFailed, c.Turns} {
		if v < 0 || v > workerMaxCount {
			return fmt.Errorf("%w: counts", ErrInvalid)
		}
	}
	return nil
}

// Worker result statuses.
const (
	WorkerStatusSucceeded  = "succeeded"
	WorkerStatusFailed     = "failed"
	WorkerStatusNeedsInput = "needs_input"
	WorkerStatusCancelled  = "cancelled"
)

// WorkerResultState maps a result status to its job state.
func WorkerResultState(status string) (string, bool) {
	switch status {
	case WorkerStatusSucceeded:
		return WorkerJobSucceeded, true
	case WorkerStatusFailed:
		return WorkerJobFailed, true
	case WorkerStatusNeedsInput:
		return WorkerJobNeedsInput, true
	case WorkerStatusCancelled:
		return WorkerJobCancelled, true
	}
	return "", false
}

// WorkerResult is the structured result contract (REQ-7). Summary is shown
// only to the owner and never logged.
type WorkerResult struct {
	Status       string      `json:"status"`
	ReasonCode   string      `json:"reason_code"`
	Summary      string      `json:"summary"`
	Branch       string      `json:"branch"`
	PRURL        string      `json:"pr_url"`
	TestOutcome  string      `json:"test_outcome"`
	ArtifactRefs []string    `json:"artifact_refs"`
	Usage        WorkerUsage `json:"usage"`
}

// WorkerUsage is the executor usage of one job.
type WorkerUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	Turns           int64 `json:"turns"`
	DurationSeconds int64 `json:"duration_seconds"`
}

var (
	workerCodeRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	workerBranchRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	workerArtifactRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$`)
	workerIdemKeyRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	workerTestOutcomes  = map[string]bool{"": true, "passed": true, "failed": true, "not_run": true}
	workerMaxArtifacts  = 20
	workerMaxUsageValue = int64(1) << 40
)

// ValidWorkerIdempotencyKey reports whether key is a bounded idempotency key.
func ValidWorkerIdempotencyKey(key string) bool { return workerIdemKeyRE.MatchString(key) }

// Validate checks every field of the result contract.
func (r WorkerResult) Validate() error {
	if _, ok := WorkerResultState(r.Status); !ok {
		return fmt.Errorf("%w: status", ErrInvalid)
	}
	if r.ReasonCode != "" && !workerCodeRE.MatchString(r.ReasonCode) {
		return fmt.Errorf("%w: reason_code", ErrInvalid)
	}
	if !utf8.ValidString(r.Summary) || utf8.RuneCountInString(r.Summary) > WorkerMaxSummaryRunes {
		return fmt.Errorf("%w: summary", ErrInvalid)
	}
	if r.Branch != "" && (!workerBranchRE.MatchString(r.Branch) || strings.Contains(r.Branch, "..")) {
		return fmt.Errorf("%w: branch", ErrInvalid)
	}
	if r.PRURL != "" && !validGitHubURL(r.PRURL) {
		return fmt.Errorf("%w: pr_url", ErrInvalid)
	}
	if !workerTestOutcomes[r.TestOutcome] {
		return fmt.Errorf("%w: test_outcome", ErrInvalid)
	}
	if len(r.ArtifactRefs) > workerMaxArtifacts {
		return fmt.Errorf("%w: artifact_refs", ErrInvalid)
	}
	for _, a := range r.ArtifactRefs {
		if !workerArtifactRE.MatchString(a) {
			return fmt.Errorf("%w: artifact_refs", ErrInvalid)
		}
	}
	u := r.Usage
	for _, v := range []int64{u.InputTokens, u.OutputTokens, u.Turns, u.DurationSeconds} {
		if v < 0 || v > workerMaxUsageValue {
			return fmt.Errorf("%w: usage", ErrInvalid)
		}
	}
	return nil
}

func validGitHubURL(raw string) bool {
	if len(raw) > 300 || !strings.HasPrefix(raw, "https://github.com/") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && len(u.Path) > 1
}

// WorkerJob is one unit of work for a local worker. Payload holds ids, repo
// coordinates and bounded context; never a secret.
type WorkerJob struct {
	ID                   string
	WorkspaceID          string
	ExecutionID          int64 // 0: none
	StepID               string
	RequiredCapabilities []string
	State                string
	DeviceID             string // "" until leased
	Attempt              int
	Payload              json.RawMessage
	Progress             *WorkerProgress
	Result               *WorkerResult
	LeasedAt             *time.Time
	LeaseExpiresAt       *time.Time
	InterruptedAt        *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	FinishedAt           *time.Time
}

// WorkerLease is what a poll hands the device. LeaseToken is shown once; the
// hub keeps only its hash.
type WorkerLease struct {
	Job        WorkerJob
	LeaseToken string
}

// WorkerEvent is one audit row: fixed codes only.
type WorkerEvent struct {
	ID         int64
	DeviceID   string
	JobID      string
	Kind       string
	ReasonCode string
	CreatedAt  time.Time
}

// WorkerTelemetry is one bounded metrics snapshot (REQ-18). A nil pointer is
// "unknown". It never holds paths, process lists, hostnames or addresses.
type WorkerTelemetry struct {
	State                string         `json:"state"` // idle | busy
	CPULoad              *float64       `json:"cpu_load,omitempty"`
	CPUCores             *int64         `json:"cpu_cores,omitempty"`
	RAMUsedBytes         *int64         `json:"ram_used_bytes,omitempty"`
	RAMTotalBytes        *int64         `json:"ram_total_bytes,omitempty"`
	DiskUsedBytes        *int64         `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes       *int64         `json:"disk_total_bytes,omitempty"`
	Executor             string         `json:"executor,omitempty"` // claude | opencode
	ExecutorVersion      string         `json:"executor_version,omitempty"`
	ClaudeSessionLeftPct *int64         `json:"claude_session_left_pct,omitempty"`
	ClaudeSessionResetAt *time.Time     `json:"claude_session_reset_at,omitempty"`
	ClaudeWeeklyLeftPct  *int64         `json:"claude_weekly_left_pct,omitempty"`
	ClaudeWeeklyResetAt  *time.Time     `json:"claude_weekly_reset_at,omitempty"`
	Counters             WorkerCounters `json:"counters"`
}

// WorkerCounters are cumulative counters reported by the device.
type WorkerCounters struct {
	Jobs           int64 `json:"jobs"`
	Failures       int64 `json:"failures"`
	ExecutorTokens int64 `json:"executor_tokens"`
	BusySeconds    int64 `json:"busy_seconds"`
}

var workerVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)

const maxWorkerBytes = int64(1) << 50 // 1 PiB

// Validate checks ranges; it rejects a snapshot rather than store nonsense.
func (t WorkerTelemetry) Validate() error {
	bad := func(field string) error { return fmt.Errorf("%w: telemetry %s", ErrInvalid, field) }
	if t.State != "idle" && t.State != "busy" {
		return bad("state")
	}
	if t.CPULoad != nil && (*t.CPULoad < 0 || *t.CPULoad > 4096 || *t.CPULoad != *t.CPULoad) {
		return bad("cpu_load")
	}
	if t.CPUCores != nil && (*t.CPUCores < 1 || *t.CPUCores > 4096) {
		return bad("cpu_cores")
	}
	if !validUsedTotal(t.RAMUsedBytes, t.RAMTotalBytes) {
		return bad("ram")
	}
	if !validUsedTotal(t.DiskUsedBytes, t.DiskTotalBytes) {
		return bad("disk")
	}
	if t.Executor != "" && t.Executor != "claude" && t.Executor != "opencode" {
		return bad("executor")
	}
	if t.ExecutorVersion != "" && !workerVersionRE.MatchString(t.ExecutorVersion) {
		return bad("executor_version")
	}
	for _, p := range []*int64{t.ClaudeSessionLeftPct, t.ClaudeWeeklyLeftPct} {
		if p != nil && (*p < 0 || *p > 100) {
			return bad("claude_left")
		}
	}
	for _, r := range []*time.Time{t.ClaudeSessionResetAt, t.ClaudeWeeklyResetAt} {
		if r != nil && (r.Year() < 2000 || r.Year() > 3000) {
			return bad("reset_at")
		}
	}
	c := t.Counters
	for _, v := range []int64{c.Jobs, c.Failures, c.ExecutorTokens, c.BusySeconds} {
		if v < 0 || v > workerMaxUsageValue {
			return bad("counters")
		}
	}
	return nil
}

func validUsedTotal(used, total *int64) bool {
	if used != nil && (*used < 0 || *used > maxWorkerBytes) {
		return false
	}
	if total != nil && (*total < 1 || *total > maxWorkerBytes) {
		return false
	}
	return used == nil || total == nil || *used <= *total
}

// DiskFreeBytes is total minus used, or nil when either is unknown.
func (t WorkerTelemetry) DiskFreeBytes() *int64 {
	if t.DiskUsedBytes == nil || t.DiskTotalBytes == nil {
		return nil
	}
	v := *t.DiskTotalBytes - *t.DiskUsedBytes
	return &v
}

// RAMUsedPct is RAM in use as a percentage, or nil when unknown.
func (t WorkerTelemetry) RAMUsedPct() *float64 {
	if t.RAMUsedBytes == nil || t.RAMTotalBytes == nil || *t.RAMTotalBytes <= 0 {
		return nil
	}
	v := float64(*t.RAMUsedBytes) * 100 / float64(*t.RAMTotalBytes)
	return &v
}

// WorkerThresholds gate assignment by telemetry (REQ-19).
type WorkerThresholds struct {
	MinClaudeLeftPct int64 // worker_min_claude_left
	MinDiskFreeBytes int64 // worker_min_disk_free
	MaxRAMUsedPct    int64 // worker_max_ram_used
}

// DefaultWorkerThresholds are the setting defaults.
var DefaultWorkerThresholds = WorkerThresholds{MinClaudeLeftPct: 10, MinDiskFreeBytes: 5 << 30, MaxRAMUsedPct: 90}

// Worker assignment block reasons.
const (
	WorkerBlockClaudeSession = "claude_session_low"
	WorkerBlockClaudeWeekly  = "claude_weekly_low"
	WorkerBlockDisk          = "disk_low"
	WorkerBlockRAM           = "ram_high"
)

// WorkerBlock explains why a device may not take a job now.
type WorkerBlock struct {
	Reason string     // one of WorkerBlock*
	Until  *time.Time // Claude limit reset time, when known
}

// Block returns why t blocks assignment at now, or nil. A nil snapshot and
// unknown values never block; a Claude limit whose reset time has passed no
// longer blocks.
func (th WorkerThresholds) Block(t *WorkerTelemetry, now time.Time) *WorkerBlock {
	if t == nil {
		return nil
	}
	claude := func(left *int64, reset *time.Time, reason string) *WorkerBlock {
		if left == nil || *left >= th.MinClaudeLeftPct {
			return nil
		}
		if reset != nil && !now.Before(*reset) {
			return nil
		}
		return &WorkerBlock{Reason: reason, Until: reset}
	}
	if b := claude(t.ClaudeSessionLeftPct, t.ClaudeSessionResetAt, WorkerBlockClaudeSession); b != nil {
		return b
	}
	if b := claude(t.ClaudeWeeklyLeftPct, t.ClaudeWeeklyResetAt, WorkerBlockClaudeWeekly); b != nil {
		return b
	}
	if free := t.DiskFreeBytes(); free != nil && *free < th.MinDiskFreeBytes {
		return &WorkerBlock{Reason: WorkerBlockDisk}
	}
	if pct := t.RAMUsedPct(); pct != nil && *pct > float64(th.MaxRAMUsedPct) {
		return &WorkerBlock{Reason: WorkerBlockRAM}
	}
	return nil
}

// WorkerTelemetryRecord is the latest snapshot of a device.
type WorkerTelemetryRecord struct {
	DeviceID   string
	Snapshot   WorkerTelemetry
	ReceivedAt time.Time
}

// WorkerJobs is the hub's side of worker jobs (workflow steps, /cancel, CLI).
type WorkerJobs interface {
	// CreateJob queues a WAITING_FOR_WORKER job. payload is a JSON object of
	// at most 16 KiB without secret-looking keys.
	CreateJob(ctx context.Context, workspaceID string, executionID int64, stepID string, requiredCaps []string, payload json.RawMessage) (WorkerJob, error)
	// CancelJob: waiting/interrupted → CANCELLED, leased/running →
	// CANCELLING. ErrNotFound if unknown; ErrConflict if already finished.
	CancelJob(ctx context.Context, workspaceID, jobID string) (WorkerJob, error)
	GetJob(ctx context.Context, workspaceID, jobID string) (WorkerJob, error)
	ListJobs(ctx context.Context, workspaceID string, limit int) ([]WorkerJob, error)
	ListEvents(ctx context.Context, workspaceID string, limit int) ([]WorkerEvent, error)
	ListTelemetry(ctx context.Context, workspaceID string) ([]WorkerTelemetryRecord, error)
}

// WorkerLeases is the device's side of the lease protocol. Errors:
// ErrNotFound (unknown job or another device's/workspace's job), ErrConflict
// (stale or foreign lease token, finished job, resume window passed),
// ErrInvalid (bad input).
type WorkerLeases interface {
	// LeaseJob leases the oldest matching waiting job, or returns ok=false
	// (none, device busy, or blocked by telemetry thresholds).
	LeaseJob(ctx context.Context, device WorkerDevice) (lease WorkerLease, ok bool, err error)
	// JobsChanged is closed when jobs may have become available.
	JobsChanged() <-chan struct{}
	Heartbeat(ctx context.Context, device WorkerDevice, jobID, leaseToken string) (WorkerJob, error)
	// ReportProgress stores the event; duplicate=true when seq was seen.
	ReportProgress(ctx context.Context, device WorkerDevice, jobID, leaseToken string, p WorkerProgress) (job WorkerJob, duplicate bool, err error)
	// ReportResult records the result once; the same idempotency key returns
	// the first result with duplicate=true.
	ReportResult(ctx context.Context, device WorkerDevice, jobID, leaseToken, idempotencyKey string, r WorkerResult) (job WorkerJob, duplicate bool, err error)
	RecordTelemetry(ctx context.Context, device WorkerDevice, t WorkerTelemetry) error
}

// Worker job change kinds passed to observers.
const (
	WorkerChangeLeased          = "leased"
	WorkerChangeProgress        = "progress"
	WorkerChangeResumed         = "resumed"
	WorkerChangeInterrupted     = "interrupted"
	WorkerChangeCancelRequested = "cancel_requested"
	WorkerChangeFinished        = "finished"
)

// WorkerJobObserver is told about job changes after they commit (W-4 edits
// the topic message). It must return quickly and never block.
type WorkerJobObserver interface {
	WorkerJobChanged(ctx context.Context, kind string, job WorkerJob)
}

// WorkerInstruction is an owner follow-up delivered with a heartbeat.
type WorkerInstruction struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// WorkerInstructionSource supplies pending owner instructions for a job
// (filled by W-4). Nil means none.
type WorkerInstructionSource interface {
	PendingWorkerInstructions(ctx context.Context, job WorkerJob) ([]WorkerInstruction, error)
}

// WorkerMaintenance is the periodic upkeep of the lease protocol.
type WorkerMaintenance interface {
	ExpireLeases(ctx context.Context) (int, error)
	MarkDevicesOffline(ctx context.Context) (int, error)
	PruneTelemetry(ctx context.Context) (int64, error)
}

// WorkerSweeper runs WorkerMaintenance on an interval.
type WorkerSweeper struct {
	Store    WorkerMaintenance
	Interval time.Duration // default 15 s
}

// Run sweeps until ctx ends. Errors are logged without detail and retried.
func (w *WorkerSweeper) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		if _, err := w.Store.ExpireLeases(ctx); err != nil && ctx.Err() == nil {
			log.Print("worker sweeper: lease expiry failed")
		}
		if _, err := w.Store.MarkDevicesOffline(ctx); err != nil && ctx.Err() == nil {
			log.Print("worker sweeper: presence sweep failed")
		}
		if time.Since(lastPrune) >= time.Hour {
			if _, err := w.Store.PruneTelemetry(ctx); err != nil && ctx.Err() == nil {
				log.Print("worker sweeper: telemetry prune failed")
			} else {
				lastPrune = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
