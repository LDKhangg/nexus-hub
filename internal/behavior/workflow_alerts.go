package behavior

import (
	"context"
	"log"
	"regexp"
	"sync"
	"time"
)

// Phase 12 REQ-10: Telegram alerts for stuck and failed workflow
// executions. The watcher scans the database every tick (the source of
// truth, so a restart never loses an alert), claims one dedup row per
// (execution, alert state) before it sends, and posts a short line to the
// workspace's General topic. Alerts carry ids, the behavior name, the
// state, an age and an error code only: never message text, prompts, tool
// inputs or outputs, or secrets.

// AlertState is the closed set of alert states.
type AlertState string

const (
	AlertStuckRunning    AlertState = "stuck_running"
	AlertStuckQueued     AlertState = "stuck_queued"
	AlertApprovalOverdue AlertState = "approval_overdue"
	AlertFailed          AlertState = "failed"
)

// Alert watcher defaults (Lead call L4).
const (
	DefaultAlertInterval     = time.Minute
	DefaultAlertStuckRunning = 15 * time.Minute
	DefaultAlertStuckQueued  = 30 * time.Minute
	DefaultAlertHourlyCap    = 20
	alertWindow              = time.Hour
	alertApprovalGrace       = 5 * time.Minute
	alertScanLimit           = 100
	alertKickDelay           = 5 * time.Second
	alertDefaultSendTimeout  = 10 * time.Second
	alertReleaseTimeout      = 5 * time.Second
)

// AlertFailedLookback bounds the failed executions that alert (and the
// failed alerts nh status counts as open): a first start never alerts
// for older failures.
const AlertFailedLookback = 24 * time.Hour

// WorkflowAlert is one execution in an alert state. Ids, a safe name, a
// state, a time and a code only.
type WorkflowAlert struct {
	WorkspaceID  string
	ExecutionID  int64
	State        AlertState
	BehaviorName string
	// Since is when the alert state began: the job's last change (stuck
	// running), its due time (stuck queued), the approval expiry (overdue)
	// or the finish (failed).
	Since     time.Time
	ErrorCode string
}

// AlertQuery bounds one candidate scan.
type AlertQuery struct {
	RunningBefore  time.Time
	QueuedBefore   time.Time
	ApprovalBefore time.Time
	FailedSince    time.Time
	Limit          int
}

// WorkflowAlertStore reads alert candidates and keeps the dedup rows.
type WorkflowAlertStore interface {
	// ListAlertCandidates returns executions in an alert state that have
	// no alert row yet, oldest first.
	ListAlertCandidates(ctx context.Context, q AlertQuery) ([]WorkflowAlert, error)
	// ClaimAlert inserts the dedup row of (execution, state); claimed is
	// false when the row already existed.
	ClaimAlert(ctx context.Context, a WorkflowAlert, at time.Time) (claimed bool, err error)
	// ReleaseAlert deletes the dedup row of a failed send.
	ReleaseAlert(ctx context.Context, a WorkflowAlert) error
}

// AlertMetrics counts sent and suppressed alerts (metrics.Hub).
type AlertMetrics interface {
	AlertSent(state string)
	AlertSuppressed()
}

// WorkflowAlerts is the alert watcher service.
type WorkflowAlerts struct {
	Store     WorkflowAlertStore
	Chats     ChatResolver
	Messenger Messenger
	Metrics   AlertMetrics // nil: not counted
	// Enabled is the alerts_enabled setting; false keeps the service idle.
	Enabled      bool
	StuckRunning time.Duration // default 15 min
	StuckQueued  time.Duration // default 30 min
	Interval     time.Duration // default 1 min
	HourlyCap    int           // default 20, hub-wide
	SendTimeout  time.Duration // default 10 s
	Now          func() time.Time

	mu          sync.Mutex
	kick        chan struct{}
	windowStart time.Time
	sent        int
	suppressed  map[string]int // workspace -> merged in this window
	summaries   map[string]int // workspace -> merged, summary not yet sent
	retryAfter  map[alertKey]time.Time
	// summaryRetry is when a failed summary send may be tried again.
	summaryRetry time.Time
}

type alertKey struct {
	workspaceID string
	executionID int64
	state       AlertState
}

func (w *WorkflowAlerts) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *WorkflowAlerts) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return DefaultAlertInterval
}

// retryDelay is how long a failed send waits: 90% of the tick, so the
// next tick retries it but an early (kicked) scan never does.
func (w *WorkflowAlerts) retryDelay() time.Duration {
	return w.interval() * 9 / 10
}

func (w *WorkflowAlerts) kickCh() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
	return w.kick
}

// Kick asks for an early scan (a step failed for good). It never blocks;
// the scan runs a few seconds later, so the failed state is committed.
// The database scan stays the source of truth.
func (w *WorkflowAlerts) Kick() {
	if w == nil {
		return
	}
	select {
	case w.kickCh() <- struct{}{}:
	default:
	}
}

// Run scans every interval until ctx ends. Errors are logged without
// detail and retried on the next tick.
func (w *WorkflowAlerts) Run(ctx context.Context) error {
	if !w.Enabled {
		log.Print("workflow alerts: off (alerts_enabled is false)")
		<-ctx.Done()
		return nil
	}
	kick := w.kickCh()
	// A start scans at once, so alerts a restart interrupted are not late.
	w.Scan(ctx)
	t := time.NewTicker(w.interval())
	defer t.Stop()
	var delay *time.Timer
	var delayC <-chan time.Time
	defer func() {
		if delay != nil {
			delay.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.Scan(ctx)
		case <-kick:
			if delayC == nil {
				delay = time.NewTimer(alertKickDelay)
				delayC = delay.C
			}
		case <-delayC:
			delayC = nil
			w.Scan(ctx)
		}
	}
}

// Scan runs one pass: it rolls the rate window (sending due summaries)
// and alerts every new candidate.
func (w *WorkflowAlerts) Scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := w.now()
	w.roll(now)
	w.sendSummaries(ctx, now)
	running, queued := w.StuckRunning, w.StuckQueued
	if running <= 0 {
		running = DefaultAlertStuckRunning
	}
	if queued <= 0 {
		queued = DefaultAlertStuckQueued
	}
	list, err := w.Store.ListAlertCandidates(ctx, AlertQuery{
		RunningBefore:  now.Add(-running),
		QueuedBefore:   now.Add(-queued),
		ApprovalBefore: now.Add(-alertApprovalGrace),
		FailedSince:    now.Add(-AlertFailedLookback),
		Limit:          alertScanLimit,
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Print("workflow alerts: candidate scan failed")
		}
		return
	}
	for _, a := range list {
		if ctx.Err() != nil {
			return
		}
		w.handle(ctx, a, now)
	}
}

// roll starts a new rate window when the current one ended and moves its
// merged counts to the summaries to send.
func (w *WorkflowAlerts) roll(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.windowStart.IsZero() {
		w.windowStart = now
		return
	}
	if now.Sub(w.windowStart) < alertWindow {
		return
	}
	for ws, n := range w.suppressed {
		if n > 0 {
			if w.summaries == nil {
				w.summaries = map[string]int{}
			}
			w.summaries[ws] += n
		}
	}
	w.suppressed = nil
	w.sent = 0
	w.windowStart = now
	for k, at := range w.retryAfter {
		if !now.Before(at) {
			delete(w.retryAfter, k)
		}
	}
}

func (w *WorkflowAlerts) hourlyCap() int {
	if w.HourlyCap > 0 {
		return w.HourlyCap
	}
	return DefaultAlertHourlyCap
}

func (w *WorkflowAlerts) handle(ctx context.Context, a WorkflowAlert, now time.Time) {
	key := alertKey{workspaceID: a.WorkspaceID, executionID: a.ExecutionID, state: a.State}
	w.mu.Lock()
	if at, ok := w.retryAfter[key]; ok && now.Before(at) {
		w.mu.Unlock()
		return
	}
	overCap := w.sent >= w.hourlyCap()
	w.mu.Unlock()
	claimed, err := w.Store.ClaimAlert(ctx, a, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("workflow alerts: claim failed state=%s", a.State)
		}
		return
	}
	if !claimed {
		return
	}
	if overCap {
		w.mu.Lock()
		if w.suppressed == nil {
			w.suppressed = map[string]int{}
		}
		w.suppressed[a.WorkspaceID]++
		w.mu.Unlock()
		if w.Metrics != nil {
			w.Metrics.AlertSuppressed()
		}
		log.Printf("workflow alerts: suppressed by hourly cap state=%s execution_id=%d", a.State, a.ExecutionID)
		return
	}
	if err := w.send(ctx, a.WorkspaceID, alertText(a, now)); err != nil {
		w.release(a)
		w.mu.Lock()
		if w.retryAfter == nil {
			w.retryAfter = map[alertKey]time.Time{}
		}
		w.retryAfter[key] = now.Add(w.retryDelay())
		w.mu.Unlock()
		log.Printf("workflow alerts: send failed; retried next tick state=%s execution_id=%d", a.State, a.ExecutionID)
		return
	}
	w.mu.Lock()
	w.sent++
	delete(w.retryAfter, key)
	w.mu.Unlock()
	if w.Metrics != nil {
		w.Metrics.AlertSent(string(a.State))
	}
	log.Printf("workflow alerts: sent state=%s execution_id=%d", a.State, a.ExecutionID)
}

// release deletes the claim of a failed send, also while shutting down.
func (w *WorkflowAlerts) release(a WorkflowAlert) {
	rctx, cancel := context.WithTimeout(context.Background(), alertReleaseTimeout)
	defer cancel()
	if err := w.Store.ReleaseAlert(rctx, a); err != nil {
		log.Printf("workflow alerts: release failed; alert not retried state=%s execution_id=%d", a.State, a.ExecutionID)
	}
}

// sendSummaries sends one line per workspace with alerts merged by the
// cap in an ended window; a failed send is kept for the next tick.
func (w *WorkflowAlerts) sendSummaries(ctx context.Context, now time.Time) {
	w.mu.Lock()
	if now.Before(w.summaryRetry) {
		w.mu.Unlock()
		return
	}
	pending := make(map[string]int, len(w.summaries))
	for ws, n := range w.summaries {
		pending[ws] = n
	}
	w.mu.Unlock()
	for ws, n := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := w.send(ctx, ws, alertSummaryText(n, w.hourlyCap())); err != nil {
			log.Print("workflow alerts: summary send failed; retried next tick")
			w.mu.Lock()
			w.summaryRetry = now.Add(w.retryDelay())
			w.mu.Unlock()
			continue
		}
		w.mu.Lock()
		w.summaries[ws] -= n
		if w.summaries[ws] <= 0 {
			delete(w.summaries, ws)
		}
		w.mu.Unlock()
		log.Printf("workflow alerts: summary sent suppressed=%d", n)
	}
}

// send posts text to the workspace's General topic (the owner chat
// without a thread).
func (w *WorkflowAlerts) send(ctx context.Context, workspaceID, text string) error {
	timeout := w.SendTimeout
	if timeout <= 0 {
		timeout = alertDefaultSendTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	chat, err := w.Chats.OwnerChat(sctx, workspaceID, TriggerData{})
	if err != nil {
		return err
	}
	chat.ThreadID = 0
	_, err = w.Messenger.SendMessage(sctx, chat, OutMessage{Text: text})
	return err
}

var (
	alertCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	alertNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// alertText is the alert line: icon, execution id, behavior name, state,
// age and error code. Values outside their safe shape are left out.
func alertText(a WorkflowAlert, now time.Time) string {
	t := textsFor("")
	name := a.BehaviorName
	if !alertNameShape.MatchString(name) {
		name = ""
	}
	code := a.ErrorCode
	if !alertCodeShape.MatchString(code) {
		code = ""
	}
	age := time.Duration(0)
	if !a.Since.IsZero() && now.After(a.Since) {
		age = now.Sub(a.Since)
	}
	return t.workflowAlert(a.State, a.ExecutionID, name, age, code)
}

func alertSummaryText(n, limit int) string {
	return textsFor("").workflowAlertSummary(n, limit)
}
