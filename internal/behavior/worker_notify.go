package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// MessageEditor is the optional part of a Messenger that edits the text of a
// message the bot sent. Without it the progress of a local task is sent as
// new messages.
type MessageEditor interface {
	EditMessageText(ctx context.Context, to ChatRef, messageID int64, text string) error
}

// WorkerTelemetryReader reads the latest telemetry snapshot of each device.
type WorkerTelemetryReader interface {
	ListTelemetry(ctx context.Context, workspaceID string) ([]WorkerTelemetryRecord, error)
}

// workerTopics is the part of the topic router the notifier uses.
type workerTopics interface {
	Destination(ctx context.Context, base ChatRef, in TopicIntent) ChatRef
	Recover(ctx context.Context, chat ChatRef, err error) (ChatRef, bool)
	NoteSent(c ChatRef)
}

// Texts of the local task topic messages (owner-facing, Vietnamese).
const (
	msgWorkerWaitingOffline = "Đang chờ máy local online."
	btnWorkerRetry          = "Thử lại"
	btnWorkerCancel         = "Huỷ"
	// workerButtonLifetime is how long Retry and Cancel stay valid.
	workerButtonLifetime = 24 * time.Hour
	// workerProgressGap is the shortest time between two edits of the
	// progress message.
	workerProgressGap = 3 * time.Second
	workerNoticeQueue = 512
)

// Waiting notice states kept on the route so a notice is sent once.
const (
	workerNoticeOffline = "offline"
	workerNoticeBlocked = "blocked"
)

// WorkerNotifierDeps are the notifier's collaborators.
type WorkerNotifierDeps struct {
	Routes     LocalTaskRoutes
	Messenger  Messenger
	Topics     workerTopics // nil: messages go to the chat of the route
	Approvals  ApprovalStore
	Devices    WorkerAdmin
	Telemetry  WorkerTelemetryReader
	Thresholds WorkerThresholds
	Waker      LocalTaskWaker
}

// WorkerNotifier tells the owner, in the task topic, what happens to a local
// task: waiting for a machine, taken by a device, progress (one message
// edited in place), the result, and failures with Retry and Cancel buttons.
// It is a WorkerJobObserver: changes are queued and handled by Run, so the
// store never waits for Telegram. It also wakes the workflow step of a job
// that succeeded or was cancelled. Messages carry ids, fixed codes and the
// result summary only; none is logged.
type WorkerNotifier struct {
	d   WorkerNotifierDeps
	now func() time.Time

	queue chan workerNotice

	mu       sync.Mutex
	progress map[string]*WorkerJob // newest unprocessed progress per job
	lastEdit map[int64]time.Time   // per execution
}

type workerNotice struct {
	kind string // a WorkerChange* kind, or "queued"
	job  WorkerJob
}

var (
	_ WorkerJobObserver = (*WorkerNotifier)(nil)
	_ WorkerJobQueued   = (*WorkerNotifier)(nil)
)

// NewWorkerNotifier builds the notifier. Run must be started for it to work.
func NewWorkerNotifier(d WorkerNotifierDeps) *WorkerNotifier {
	return &WorkerNotifier{
		d: d, now: time.Now, queue: make(chan workerNotice, workerNoticeQueue),
		progress: map[string]*WorkerJob{}, lastEdit: map[int64]time.Time{},
	}
}

// WorkerJobChanged implements WorkerJobObserver. It never blocks.
func (n *WorkerNotifier) WorkerJobChanged(_ context.Context, kind string, job WorkerJob) {
	if n == nil || job.ExecutionID <= 0 {
		return
	}
	if kind == WorkerChangeProgress {
		// Only the newest progress of a job matters.
		n.mu.Lock()
		_, pending := n.progress[job.ID]
		j := job
		n.progress[job.ID] = &j
		n.mu.Unlock()
		if pending {
			return
		}
	}
	n.enqueue(workerNotice{kind: kind, job: job})
}

// WorkerJobQueued implements WorkerJobQueued.
func (n *WorkerNotifier) WorkerJobQueued(_ context.Context, job WorkerJob) {
	if n != nil {
		n.enqueue(workerNotice{kind: "queued", job: job})
	}
}

func (n *WorkerNotifier) enqueue(e workerNotice) {
	select {
	case n.queue <- e:
	default:
		log.Print("worker notifier: queue full; a topic message was dropped")
		if e.kind == WorkerChangeProgress {
			n.mu.Lock()
			delete(n.progress, e.job.ID)
			n.mu.Unlock()
		}
	}
}

// Run handles queued changes until ctx ends.
func (n *WorkerNotifier) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-n.queue:
			n.handle(ctx, e)
		}
	}
}

func (n *WorkerNotifier) handle(ctx context.Context, e workerNotice) {
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	job := e.job
	if e.kind == WorkerChangeProgress {
		n.mu.Lock()
		latest := n.progress[job.ID]
		delete(n.progress, job.ID)
		n.mu.Unlock()
		if latest == nil {
			return
		}
		job = *latest
	}
	route, err := n.d.Routes.GetRoute(hctx, job.WorkspaceID, job.ExecutionID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Print("worker notifier: route not read")
		}
		return
	}
	switch e.kind {
	case "queued":
		n.queued(hctx, route, job)
	case WorkerChangeLeased:
		n.setNotice(hctx, route, "")
		n.say(hctx, route, fmt.Sprintf("%s: máy %s đã nhận việc (lần %d).", workflow.FormatRef(job.ExecutionID), n.deviceName(hctx, job), max(job.Attempt, 1)), nil)
	case WorkerChangeResumed:
		n.say(hctx, route, fmt.Sprintf("%s: máy %s tiếp tục công việc.", workflow.FormatRef(job.ExecutionID), n.deviceName(hctx, job)), nil)
	case WorkerChangeProgress:
		n.showProgress(hctx, route, job)
	case WorkerChangeCancelRequested:
		n.say(hctx, route, workflow.FormatRef(job.ExecutionID)+": đã yêu cầu huỷ, đang chờ máy local dừng.", nil)
	case WorkerChangeInterrupted:
		n.failed(hctx, route, job, "Máy local mất kết nối nên việc bị gián đoạn. Máy cũ có thể tiếp tục trong 24 giờ, hoặc chọn Thử lại / Huỷ.")
	case WorkerChangeFinished:
		n.finished(hctx, route, job)
	}
}

// --- messages ---

func (n *WorkerNotifier) finished(ctx context.Context, route LocalTaskRoute, job WorkerJob) {
	ref := workflow.FormatRef(job.ExecutionID)
	switch job.State {
	case WorkerJobSucceeded:
		n.say(ctx, route, ref+": hoàn tất.\n"+workerResultText(job.Result), nil)
	case WorkerJobCancelled:
		n.say(ctx, route, ref+": đã huỷ.", nil)
	case WorkerJobNeedsInput:
		text := "Cần thêm thông tin để tiếp tục."
		if job.Result != nil && strings.TrimSpace(job.Result.Summary) != "" {
			text += "\n" + capRunes(strings.TrimSpace(job.Result.Summary), 1000)
		}
		n.failed(ctx, route, job, text+"\nGửi thêm yêu cầu trong topic rồi bấm Thử lại, hoặc Huỷ.")
	case WorkerJobFailed:
		n.failed(ctx, route, job, workerFailureText(job.Result))
	}
	// A job that ended for good finishes its workflow step.
	if (job.State == WorkerJobSucceeded || job.State == WorkerJobCancelled) && n.d.Waker != nil {
		if err := n.d.Waker.WakeWorkerStep(ctx, job.WorkspaceID, job.ExecutionID, job.StepID); err != nil && !errors.Is(err, workflow.ErrNotEligible) {
			log.Print("worker notifier: workflow step not woken; the reconciler retries")
		}
	}
}

// workerResultText is the success summary: summary, branch, PR, test result.
func workerResultText(r *WorkerResult) string {
	if r == nil {
		return "Không có kết quả chi tiết."
	}
	var lines []string
	if s := strings.TrimSpace(r.Summary); s != "" {
		lines = append(lines, capRunes(s, WorkerMaxSummaryRunes))
	}
	if r.Branch != "" {
		lines = append(lines, "Nhánh: "+r.Branch)
	}
	if r.PRURL != "" {
		lines = append(lines, "PR: "+r.PRURL)
	}
	switch r.TestOutcome {
	case "passed":
		lines = append(lines, "Test: đạt")
	case "failed":
		lines = append(lines, "Test: không đạt")
	case "not_run":
		lines = append(lines, "Test: không chạy")
	}
	if len(lines) == 0 {
		return "Không có kết quả chi tiết."
	}
	return strings.Join(lines, "\n")
}

// workerFailureReasons maps the reason codes of internal/worker to texts.
var workerFailureReasons = map[string]string{
	"executor_crash":   "trình thực thi bị lỗi",
	"executor_timeout": "quá thời gian cho phép (60 phút)",
	"turn_limit":       "chạm giới hạn số lượt",
	"tests_failed":     "test không đạt",
	"diff_too_large":   "thay đổi quá lớn (hơn 2.000 dòng), cần bạn xác nhận",
	"push_failed":      "không đẩy được nhánh",
	"pr_failed":        "không tạo được PR",
	"lease_lost":       "mất quyền giữ việc",
	"worker_restarted": "máy local khởi động lại giữa chừng",
	"cancelled":        "đã huỷ",
	"invalid_job":      "yêu cầu không hợp lệ cho máy local",
	"workspace_failed": "không chuẩn bị được thư mục làm việc",
	"no_changes":       "không có thay đổi nào",
}

func workerFailureText(r *WorkerResult) string {
	reason := "không rõ nguyên nhân"
	if r != nil && r.ReasonCode != "" {
		if t, ok := workerFailureReasons[r.ReasonCode]; ok {
			reason = t
		} else {
			reason = "mã lỗi " + r.ReasonCode
		}
	}
	text := "Việc thất bại: " + reason + "."
	if r != nil && strings.TrimSpace(r.Summary) != "" {
		text += "\n" + capRunes(strings.TrimSpace(r.Summary), 1000)
	}
	return text
}

// failed sends the failure text with Retry and Cancel buttons bound to this
// chat and attempt, valid for 24 hours.
func (n *WorkerNotifier) failed(ctx context.Context, route LocalTaskRoute, job WorkerJob, text string) {
	text = workflow.FormatRef(job.ExecutionID) + ": " + text
	if n.d.Approvals == nil {
		n.say(ctx, route, text, nil)
		return
	}
	nonce, err := NewNonce()
	if err != nil {
		n.say(ctx, route, text, nil)
		return
	}
	payload, _ := json.Marshal(workerJobPayload{ChatID: route.ChatID, JobID: job.ID, ExecutionID: job.ExecutionID, Attempt: job.Attempt})
	ap, err := n.d.Approvals.CreateApproval(ctx, job.WorkspaceID, NewApproval{
		Kind: ApprovalWorkerJob, Action: "retry_or_cancel", Resource: "worker_job:" + job.ID, Payload: payload,
		Nonce: nonce, ExpiresAt: n.now().Add(workerButtonLifetime),
	})
	if err != nil {
		n.say(ctx, route, text, nil)
		return
	}
	retry, err1 := (CallbackData{Kind: CallbackApprove, Nonce: ap.Nonce}).Encode()
	cancel, err2 := (CallbackData{Kind: CallbackReject, Nonce: ap.Nonce}).Encode()
	if err1 != nil || err2 != nil {
		n.say(ctx, route, text, nil)
		return
	}
	id := n.say(ctx, route, text, [][]Button{{{Text: btnWorkerRetry, Data: retry}, {Text: btnWorkerCancel, Data: cancel}}})
	if id > 0 {
		if err := n.d.Approvals.SetApprovalMessage(ctx, job.WorkspaceID, ap.ID, id); err != nil {
			log.Print("worker notifier: button message not saved")
		}
	}
}

// workerJobPayload is the payload of a worker_job approval.
type workerJobPayload struct {
	ChatID      int64  `json:"chat_id"`
	JobID       string `json:"job_id"`
	ExecutionID int64  `json:"execution_id"`
	Attempt     int    `json:"attempt"`
}

var workerPhaseText = map[string]string{
	"cloning": "đang tải mã", "editing": "đang sửa mã", "testing": "đang chạy test", "pushing": "đang đẩy nhánh",
}

func workerProgressText(job WorkerJob) string {
	p := job.Progress
	head := workflow.FormatRef(job.ExecutionID) + ": "
	if p == nil {
		return head + "đang chạy."
	}
	phase := workerPhaseText[p.Phase]
	if phase == "" {
		phase = p.Phase
	}
	c := p.Counts
	return head + fmt.Sprintf("%s · %d tệp · +%d/-%d dòng · test %d đạt, %d lỗi · %d lượt",
		phase, c.FilesChanged, c.LinesAdded, c.LinesRemoved, c.TestsPassed, c.TestsFailed, c.Turns)
}

// showProgress keeps one message per attempt and edits it in place; edits
// are spaced and skipped when nothing changed.
func (n *WorkerNotifier) showProgress(ctx context.Context, route LocalTaskRoute, job WorkerJob) {
	text := workerProgressText(job)
	editor, _ := n.d.Messenger.(MessageEditor)
	if editor != nil && route.ProgressMessageID > 0 && route.ProgressAttempt == job.Attempt {
		n.mu.Lock()
		last := n.lastEdit[job.ExecutionID]
		n.mu.Unlock()
		if n.now().Sub(last) < workerProgressGap {
			return
		}
		to := ChatRef{WorkspaceID: route.WorkspaceID, IntegrationID: route.IntegrationID, ChatID: route.ChatID, ThreadID: route.ThreadID}
		if err := editor.EditMessageText(ctx, to, route.ProgressMessageID, text); err == nil {
			n.mu.Lock()
			n.lastEdit[job.ExecutionID] = n.now()
			n.mu.Unlock()
			return
		}
		// Edit refused (text unchanged, message gone): fall through to a new message.
	}
	if n.d.Messenger == nil {
		return
	}
	dest, id := n.send(ctx, route, OutMessage{Text: text})
	if id <= 0 {
		return
	}
	if err := n.d.Routes.SetProgressMessage(ctx, route.WorkspaceID, route.ExecutionID, id, job.Attempt, dest.ThreadID); err != nil {
		log.Print("worker notifier: progress message not saved")
	}
	n.mu.Lock()
	n.lastEdit[job.ExecutionID] = n.now()
	n.mu.Unlock()
}

// queued says that the task waits for a machine: none online, or every
// matching one is held back by the telemetry thresholds. A notice is sent
// once until a device takes the job.
func (n *WorkerNotifier) queued(ctx context.Context, route LocalTaskRoute, job WorkerJob) {
	text, notice := n.waitingNotice(ctx, job)
	if notice == "" || notice == route.WaitingNotice {
		return
	}
	n.setNotice(ctx, route, notice)
	n.say(ctx, route, workflow.FormatRef(job.ExecutionID)+": "+text, nil)
}

func (n *WorkerNotifier) setNotice(ctx context.Context, route LocalTaskRoute, notice string) {
	if err := n.d.Routes.SetWaitingNotice(ctx, route.WorkspaceID, route.ExecutionID, notice); err != nil {
		log.Print("worker notifier: notice state not saved")
	}
}

// waitingNotice returns the text and notice state for a job that waits, or
// "" when a matching device is online and free to take it.
func (n *WorkerNotifier) waitingNotice(ctx context.Context, job WorkerJob) (string, string) {
	if n.d.Devices == nil {
		return "", ""
	}
	devices, err := n.d.Devices.ListDevices(ctx, job.WorkspaceID)
	if err != nil {
		return "", ""
	}
	snaps := map[string]WorkerTelemetry{}
	if n.d.Telemetry != nil {
		if recs, err := n.d.Telemetry.ListTelemetry(ctx, job.WorkspaceID); err == nil {
			for _, r := range recs {
				snaps[r.DeviceID] = r.Snapshot
			}
		}
	}
	now := n.now()
	online := 0
	var firstBlock *WorkerBlock
	for _, d := range devices {
		state := d.DisplayState(now)
		if state != "online" && state != "busy" || !coversCapabilities(d.Capabilities, job.RequiredCapabilities) {
			continue
		}
		online++
		var snap *WorkerTelemetry
		if s, ok := snaps[d.ID]; ok {
			snap = &s
		}
		b := n.d.Thresholds.Block(snap, now)
		if b == nil {
			return "", "" // a free device will lease it at its next poll
		}
		if firstBlock == nil {
			firstBlock = b
		}
	}
	if online == 0 {
		return msgWorkerWaitingOffline, workerNoticeOffline
	}
	return workerBlockText(firstBlock), workerNoticeBlocked
}

func coversCapabilities(have, need []string) bool {
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[c] = true
	}
	for _, c := range need {
		if !set[c] {
			return false
		}
	}
	return true
}

func workerBlockText(b *WorkerBlock) string {
	if b == nil {
		return msgWorkerWaitingOffline
	}
	switch b.Reason {
	case WorkerBlockClaudeSession, WorkerBlockClaudeWeekly:
		if b.Until != nil {
			return "máy local sắp hết hạn mức Claude, chờ reset lúc " + b.Until.UTC().Format("2006-01-02 15:04") + " UTC."
		}
		return "máy local sắp hết hạn mức Claude, chờ reset."
	case WorkerBlockDisk:
		return "máy local sắp hết dung lượng ổ đĩa, chờ giải phóng."
	case WorkerBlockRAM:
		return "máy local đang dùng quá nhiều RAM, chờ giảm tải."
	}
	return msgWorkerWaitingOffline
}

func (n *WorkerNotifier) deviceName(ctx context.Context, job WorkerJob) string {
	if n.d.Devices != nil && job.DeviceID != "" {
		if devices, err := n.d.Devices.ListDevices(ctx, job.WorkspaceID); err == nil {
			for _, d := range devices {
				if d.ID == job.DeviceID {
					return d.Name
				}
			}
		}
	}
	return "local"
}

// --- sending ---

func (n *WorkerNotifier) say(ctx context.Context, route LocalTaskRoute, text string, buttons [][]Button) int64 {
	if n.d.Messenger == nil {
		return 0
	}
	_, id := n.send(ctx, route, OutMessage{Text: text, Buttons: buttons})
	return id
}

// send delivers to the task's topic (created on demand); a rejected topic
// send is retried once where the router says (reopened topic or General).
func (n *WorkerNotifier) send(ctx context.Context, route LocalTaskRoute, m OutMessage) (ChatRef, int64) {
	chat := ChatRef{WorkspaceID: route.WorkspaceID, IntegrationID: route.IntegrationID, ChatID: route.ChatID}
	if n.d.Topics != nil {
		chat = n.d.Topics.Destination(ctx, chat, TopicIntent{Mode: ReportTopicTask, ExecutionID: route.ExecutionID, TaskID: route.ClickUpTaskID})
	}
	id, err := n.d.Messenger.SendMessage(ctx, chat, m)
	if err == nil {
		if n.d.Topics != nil {
			n.d.Topics.NoteSent(chat)
		}
		return chat, id
	}
	if chat.ThreadID != 0 && n.d.Topics != nil {
		if again, ok := n.d.Topics.Recover(context.WithoutCancel(ctx), chat, err); ok {
			chat = again
			if id, err = n.d.Messenger.SendMessage(ctx, chat, m); err == nil {
				n.d.Topics.NoteSent(chat)
				return chat, id
			}
		}
	}
	log.Print("worker notifier: topic message not sent")
	return chat, 0
}

// --- button handling (called by the inbound worker) ---

// workerJobCallback applies Retry (approve) or Cancel (reject) of a failure
// message. It returns the toast, the update code and whether to remove the
// buttons.
func (w *InboxWorker) workerJobCallback(ctx context.Context, u InboundUpdate, data CallbackData, a Approval, actor string) (string, string, bool) {
	var p workerJobPayload
	if json.Unmarshal(a.Payload, &p) != nil || a.WorkspaceID != u.WorkspaceID || p.ChatID != u.ChatID || p.JobID == "" {
		return msgStale, "stale", true
	}
	if w.d.WorkerJobs == nil || (data.Kind != CallbackApprove && data.Kind != CallbackReject) {
		return msgNotAvailable, "not_available", false
	}
	decision, toast := DecisionApproved, "Đang thử lại."
	var err error
	if data.Kind == CallbackApprove {
		_, err = w.d.WorkerJobs.RetryJob(ctx, u.WorkspaceID, p.JobID, p.Attempt)
	} else {
		decision, toast = DecisionRejected, "Đã huỷ."
		_, err = w.d.WorkerJobs.CancelJob(ctx, u.WorkspaceID, p.JobID)
	}
	switch {
	case errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound):
		// Already retried, finished or cancelled: the buttons are stale.
		if _, derr := w.d.Approvals.Decide(ctx, u.WorkspaceID, a.Nonce, DecisionExpired, "", actor, w.now()); derr != nil {
			log.Print("worker job buttons: stale approval not closed")
		}
		return msgStale, "stale", true
	case err != nil:
		return "Không xử lý được, hãy bấm lại.", "worker_job_retry", false
	}
	if _, err := w.d.Approvals.Decide(ctx, u.WorkspaceID, a.Nonce, decision, "", actor, w.now()); err != nil {
		text, code := handlerText("", err)
		if code == "stale" {
			return text, code, true
		}
	}
	if data.Kind == CallbackApprove {
		w.noteWorkerRetried(ctx, u.WorkspaceID, p)
	}
	return toast, "", true
}

// noteWorkerRetried tells the topic that the job waits again (and why, when
// no machine can take it).
func (w *InboxWorker) noteWorkerRetried(ctx context.Context, workspaceID string, p workerJobPayload) {
	if w.d.WorkerNotifier == nil || w.d.WorkerJobs == nil {
		return
	}
	job, err := w.d.WorkerJobs.GetJob(ctx, workspaceID, p.JobID)
	if err != nil {
		return
	}
	w.d.WorkerNotifier.WorkerJobQueued(ctx, job)
}

// parseJobExecution is used by /retry: "WF-n" -> execution id.
func parseJobExecution(arg string) (int64, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 0, false
	}
	if id, ok := workflow.ParseRef(arg); ok {
		return id, true
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	return id, err == nil && id > 0
}
