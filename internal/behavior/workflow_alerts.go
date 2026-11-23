package behavior

import (
	"context"
	"errors"
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
	alertRetryLimit          = 20
	alertKickDelay           = 5 * time.Second
	alertDefaultSendTimeout  = 10 * time.Second
	alertMarkTimeout         = 5 * time.Second
)

// AlertMaxAttempts bounds the send attempts of one alert; after the last
// failed attempt the alert is failed and never sent again.
const AlertMaxAttempts = 5

// AlertClaimLease is when a freshly claimed (pending) alert is due again
// if the process stops before its first send finishes.
const AlertClaimLease = time.Minute

// alertBackoff is the wait after the n-th failed attempt (1-based).
var alertBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// AlertRetryDelay is the wait after the attempts-th failed send attempt;
// ok is false when the alert has no attempt left.
func AlertRetryDelay(attempts int) (time.Duration, bool) {
	if attempts >= AlertMaxAttempts {
		return 0, false
	}
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(alertBackoff) {
		return alertBackoff[len(alertBackoff)-1], true
	}
	return alertBackoff[attempts-1], true
}

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
	// Attempts is the send attempts already made (a retry; 0 when new).
	Attempts int
	// Obsolete marks a due retry of a stuck or overdue alert whose
	// condition no longer holds; it is closed without a send.
	Obsolete bool
}

// AlertQuery bounds one candidate scan.
type AlertQuery struct {
	RunningBefore  time.Time
	QueuedBefore   time.Time
	ApprovalBefore time.Time
	FailedSince    time.Time
	Limit          int
}

// AlertSummary is one workspace's suppressed alerts not yet summarised.
type AlertSummary struct {
	WorkspaceID string
	Count       int
	OldestAt    time.Time
}

// WorkflowAlertStore reads alert candidates and keeps the alert rows: one
// per (execution, alert state), for the alert's whole life.
type WorkflowAlertStore interface {
	// ListAlertCandidates returns executions in an alert state that have
	// no alert row yet (any status), oldest first.
	ListAlertCandidates(ctx context.Context, q AlertQuery) ([]WorkflowAlert, error)
	// ClaimAlert inserts the pending row of (execution, state), due again
	// at at+AlertClaimLease; claimed is false when the row already existed.
	ClaimAlert(ctx context.Context, a WorkflowAlert, at time.Time) (claimed bool, err error)
	// SuppressAlert inserts the row of (execution, state) as suppressed by
	// the hourly cap; claimed is false when the row already existed.
	SuppressAlert(ctx context.Context, a WorkflowAlert, at time.Time) (claimed bool, err error)
	// MarkAlertSent marks a pending alert delivered at at.
	MarkAlertSent(ctx context.Context, a WorkflowAlert, at time.Time) error
	// MarkAlertAttemptFailed counts one failed attempt of a pending alert
	// made after a.Attempts attempts, with a safe code: the alert is due
	// again at next, or failed for good when next is zero.
	MarkAlertAttemptFailed(ctx context.Context, a WorkflowAlert, code string, next time.Time) error
	// MarkAlertObsolete closes a pending stuck or overdue alert whose
	// condition ended, without sending it.
	MarkAlertObsolete(ctx context.Context, a WorkflowAlert) error
	// ListAlertRetries returns pending alerts due at or before due,
	// oldest first, at most limit, each with its Obsolete flag.
	ListAlertRetries(ctx context.Context, due time.Time, limit int) ([]WorkflowAlert, error)
	// CountAlertsSentSince counts alerts delivered since since, hub-wide.
	CountAlertsSentSince(ctx context.Context, since time.Time) (int, error)
	// ListAlertSummaries returns, per workspace, the suppressed alerts
	// claimed before before and not yet summarised.
	ListAlertSummaries(ctx context.Context, before time.Time) ([]AlertSummary, error)
	// MarkAlertsSummarised marks a workspace's suppressed alerts claimed
	// before before as summarised at at.
	MarkAlertsSummarised(ctx context.Context, workspaceID string, before, at time.Time) error
}

// AlertMetrics counts sent and suppressed alerts (metrics.Hub).
type AlertMetrics interface {
	AlertSent(state string)
	AlertSuppressed()
}

// alertFailureMetrics is the optional count of alerts whose attempts ran
// out (metrics.Hub).
type alertFailureMetrics interface {
	AlertFailed()
}

// WorkflowAlerts is the alert watcher service. Every number that decides
// what is sent (dedup, attempts, the hourly cap, merged alerts) lives in
// the database, so a restart keeps it.
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

	mu   sync.Mutex
	kick chan struct{}
	// summaryRetry and summaryFails pace a failing summary send (backoff
	// only; the merged alerts stay in the database until summarised).
	summaryRetry time.Time
	summaryFails int
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

// Scan runs one pass: it sends due summaries of merged alerts, retries
// due pending alerts (a small batch of its own, so failing alerts never
// starve new ones), then claims and alerts every new candidate.
func (w *WorkflowAlerts) Scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := w.now()
	w.sendSummaries(ctx, now)
	sent, err := w.Store.CountAlertsSentSince(ctx, now.Add(-alertWindow))
	if err != nil {
		if ctx.Err() == nil {
			log.Print("workflow alerts: rate window read failed")
		}
		return
	}
	limit := w.hourlyCap()
	retries, err := w.Store.ListAlertRetries(ctx, now, alertRetryLimit)
	if err != nil {
		if ctx.Err() == nil {
			log.Print("workflow alerts: retry scan failed")
		}
	}
	for _, a := range retries {
		if ctx.Err() != nil {
			return
		}
		if a.Obsolete && a.State != AlertFailed {
			if err := w.Store.MarkAlertObsolete(ctx, a); err != nil {
				if ctx.Err() == nil {
					log.Printf("workflow alerts: obsolete mark failed state=%s execution_id=%d", a.State, a.ExecutionID)
				}
				continue
			}
			log.Printf("workflow alerts: retry dropped; condition ended state=%s execution_id=%d", a.State, a.ExecutionID)
			continue
		}
		// Over the cap a due retry waits for room in the window; it is
		// not merged, since its claim was made under the cap.
		if sent >= limit {
			break
		}
		if w.attempt(ctx, a, now) {
			sent++
		}
	}
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
		if w.handle(ctx, a, now, sent >= limit) {
			sent++
		}
	}
}

func (w *WorkflowAlerts) hourlyCap() int {
	if w.HourlyCap > 0 {
		return w.HourlyCap
	}
	return DefaultAlertHourlyCap
}

// handle claims a new candidate (suppressed when over the cap) and sends
// it; it reports whether the alert was delivered.
func (w *WorkflowAlerts) handle(ctx context.Context, a WorkflowAlert, now time.Time, overCap bool) bool {
	a.Attempts = 0
	if overCap {
		claimed, err := w.Store.SuppressAlert(ctx, a, now)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("workflow alerts: claim failed state=%s", a.State)
			}
			return false
		}
		if !claimed {
			return false
		}
		if w.Metrics != nil {
			w.Metrics.AlertSuppressed()
		}
		log.Printf("workflow alerts: suppressed by hourly cap state=%s execution_id=%d", a.State, a.ExecutionID)
		return false
	}
	claimed, err := w.Store.ClaimAlert(ctx, a, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("workflow alerts: claim failed state=%s", a.State)
		}
		return false
	}
	if !claimed {
		return false
	}
	return w.attempt(ctx, a, now)
}

// attempt sends a pending alert once and records the result: sent, or one
// more failed attempt (a timeout counts: delivery is at least once, bounded
// by AlertMaxAttempts). A send cut short by shutdown records nothing; the
// alert stays pending and is tried after the restart.
func (w *WorkflowAlerts) attempt(ctx context.Context, a WorkflowAlert, now time.Time) bool {
	sendErr := w.send(ctx, a.WorkspaceID, alertText(a, now))
	if sendErr != nil && ctx.Err() != nil {
		return false
	}
	mctx, cancel := context.WithTimeout(context.Background(), alertMarkTimeout)
	defer cancel()
	if sendErr == nil {
		if err := w.Store.MarkAlertSent(mctx, a, now); err != nil {
			log.Printf("workflow alerts: sent; mark failed state=%s execution_id=%d", a.State, a.ExecutionID)
		}
		if w.Metrics != nil {
			w.Metrics.AlertSent(string(a.State))
		}
		log.Printf("workflow alerts: sent state=%s execution_id=%d", a.State, a.ExecutionID)
		return true
	}
	code := alertSendErrorCode(sendErr)
	attempts := a.Attempts + 1
	var next time.Time
	if d, ok := AlertRetryDelay(attempts); ok {
		next = now.Add(d)
	}
	if err := w.Store.MarkAlertAttemptFailed(mctx, a, code, next); err != nil {
		log.Printf("workflow alerts: send failed; attempt not recorded state=%s execution_id=%d", a.State, a.ExecutionID)
		return false
	}
	if next.IsZero() {
		if m, ok := w.Metrics.(alertFailureMetrics); ok {
			m.AlertFailed()
		}
		log.Printf("workflow alerts: send failed for good state=%s execution_id=%d attempts=%d code=%s", a.State, a.ExecutionID, attempts, code)
		return false
	}
	log.Printf("workflow alerts: send failed; retried later state=%s execution_id=%d attempts=%d code=%s", a.State, a.ExecutionID, attempts, code)
	return false
}

// alertSendErrorCode is the closed code of a failed send.
func alertSendErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "send_timeout"
	case errors.Is(err, errAlertNoChat):
		return "chat_unavailable"
	default:
		return "send_failed"
	}
}

var errAlertNoChat = errors.New("workflow alerts: owner chat unavailable")

// sendSummaries sends one line per workspace with alerts merged by the
// cap, once the oldest of them is a rate window old; the merged alerts
// are marked summarised only after a delivered send.
func (w *WorkflowAlerts) sendSummaries(ctx context.Context, now time.Time) {
	w.mu.Lock()
	wait := now.Before(w.summaryRetry)
	w.mu.Unlock()
	if wait {
		return
	}
	list, err := w.Store.ListAlertSummaries(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Print("workflow alerts: summary read failed")
		}
		return
	}
	for _, s := range list {
		if ctx.Err() != nil {
			return
		}
		if s.Count <= 0 || now.Sub(s.OldestAt) < alertWindow {
			continue
		}
		if err := w.send(ctx, s.WorkspaceID, alertSummaryText(s.Count, w.hourlyCap())); err != nil {
			if ctx.Err() != nil {
				return
			}
			w.mu.Lock()
			w.summaryFails++
			d, ok := AlertRetryDelay(w.summaryFails)
			if !ok {
				d = alertBackoff[len(alertBackoff)-1]
			}
			w.summaryRetry = now.Add(d)
			w.mu.Unlock()
			log.Printf("workflow alerts: summary send failed; retried later code=%s", alertSendErrorCode(err))
			return
		}
		w.mu.Lock()
		w.summaryFails = 0
		w.mu.Unlock()
		mctx, cancel := context.WithTimeout(context.Background(), alertMarkTimeout)
		err := w.Store.MarkAlertsSummarised(mctx, s.WorkspaceID, now, now)
		cancel()
		if err != nil {
			log.Print("workflow alerts: summary sent; mark failed")
			continue
		}
		log.Printf("workflow alerts: summary sent suppressed=%d", s.Count)
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
		if errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errAlertNoChat
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
