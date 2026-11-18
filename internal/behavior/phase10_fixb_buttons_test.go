package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase 10 Fix B regression R-B-6 (commit b317aa1): Cancel on a failed /task
// message keeps only "📤 Đăng lên ClickUp" and the button still makes a draft.
// Fakes only: the real notifier makes the failure message and its approval,
// the real worker handles the presses.

type p10BSet struct {
	id   int64
	rows [][]Button
}

// p10BKbd is the worker-job messenger with a keyboard editor (the optional MessageKeyboardEditor).
type p10BKbd struct {
	*b3Msgr
	kmu    sync.Mutex
	sets   []p10BSet
	setErr error
}

func (m *p10BKbd) SetButtons(_ context.Context, _ ChatRef, id int64, rows [][]Button) error {
	m.kmu.Lock()
	defer m.kmu.Unlock()
	m.sets = append(m.sets, p10BSet{id, rows})
	return m.setErr
}

func (m *p10BKbd) setCount() int {
	m.kmu.Lock()
	defer m.kmu.Unlock()
	return len(m.sets)
}

type p10BFail struct {
	*p10REnv
	n       *b3NotifierEnv
	kbd     *p10BKbd
	jobs    *b3Jobs
	w       *InboxWorker
	buttons [][]Button
}

// newP10BFail makes a failed /task of ClickUp task 86abc and its failure message. withReport wires the report
// button; plain makes the worker's messenger one that cannot edit a keyboard.
func newP10BFail(t *testing.T, withReport, plain bool) *p10BFail {
	t.Helper()
	r := newP10REnv(t)
	key := p10XKey(p4WS, 41)
	r.exec.kinds[key] = WorkflowSourceLocalTask
	r.rts.routes[key] = LocalTaskRoute{WorkspaceID: p4WS, ExecutionID: 41, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: b1Thread,
		Repo: "o/r", TaskRef: "86abc", ClickUpTaskID: "86abc"}
	job := b3Job(WorkerJobFailed)
	job.Result = &WorkerResult{Status: "failed", ReasonCode: "tests_failed", Summary: "2 test lỗi", Branch: "fix/x"}
	r.jobs.jobs[key] = job
	n := b3Notifier(&fakeWorkerAdmin{}, nil)
	if withReport {
		n.n.d.ReportButtons = r.rep
	}
	n.change(WorkerChangeFinished, job)
	if len(n.m.sent) != 1 {
		t.Fatalf("failure messages %v", n.m.texts())
	}
	f := &p10BFail{p10REnv: r, n: n, kbd: &p10BKbd{b3Msgr: n.m}, jobs: &b3Jobs{job: job, found: true}, buttons: n.m.sent[0].Buttons}
	var msgr Messenger = f.kbd
	if plain {
		msgr = n.m
	}
	f.w = NewInboxWorker(InboxDeps{Messenger: msgr, Approvals: n.ap, WorkerJobs: f.jobs, WorkerNotifier: &b3Queued{},
		ExecutionReports: r.rep, ReportDrafts: r.d}, InboxSettings{})
	f.w.now = func() time.Time { return *n.clock }
	return f
}

func (f *p10BFail) press(data string, chat int64) outcome {
	return f.w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: chat, ThreadID: b1Thread,
		Kind: UpdateCallback, Command: data, Args: "q1 701"}, "corr")
}

func (f *p10BFail) removed() []int64 {
	f.n.m.mu.Lock()
	defer f.n.m.mu.Unlock()
	return append([]int64(nil), f.n.m.removed...)
}

func (f *p10BFail) requireOnlyReportButton(t *testing.T, set p10BSet) {
	t.Helper()
	if set.id != 701 || len(set.rows) != 1 || len(set.rows[0]) != 1 || set.rows[0][0].Text != "📤 Đăng lên ClickUp" || set.rows[0][0].Data != "xr:41" {
		t.Fatalf("keyboard %+v", set)
	}
}

// R-B-6: Cancel keeps only 📤 (no RemoveButtons), and the kept button still makes a draft.
func TestP10FixB_CancelKeepsReportButtonAndItStillDrafts(t *testing.T) {
	f := newP10BFail(t, true, false)
	if got := p10ButtonRows(f.buttons); len(got) != 2 || got[0] != "Thử lại|Huỷ" || got[1] != "📤 Đăng lên ClickUp" {
		t.Fatalf("buttons %v", got)
	}
	if out := f.press(f.buttons[0][1].Data, b1Chat); out.code != "" || f.jobs.cancels != 1 || f.n.m.toasts[len(f.n.m.toasts)-1] != "Đã huỷ." {
		t.Fatalf("cancel %+v cancels %d toasts %q", out, f.jobs.cancels, f.n.m.toasts)
	}
	if rm := f.removed(); len(rm) != 0 {
		t.Fatalf("the buttons were removed: %v", rm)
	}
	if f.kbd.setCount() != 1 {
		t.Fatalf("keyboard edits %d", f.kbd.setCount())
	}
	f.requireOnlyReportButton(t, f.kbd.sets[0])
	// The kept button still works: it makes a draft that says the job failed.
	if out := f.press("xr:41", b1Chat); out.code != "" {
		t.Fatalf("📤 press %+v", out)
	}
	rows := f.drafts()
	if len(rows) != 1 || rows[0].ExecutionID != 41 || rows[0].TaskID != "86abc" || !strings.Contains(rows[0].Body, msgReportOutcomeFailed) || !strings.Contains(rows[0].Body, "2 test lỗi") {
		t.Fatalf("drafts %+v", rows)
	}
	if len(f.tool.calls) != 0 {
		t.Fatalf("%d ClickUp calls before Đăng", len(f.tool.calls))
	}
}

// R-B-6: a double tap on Cancel still keeps 📤 (the second press is stale but the keyboard stays).
func TestP10FixB_CancelTwiceKeepsReportButton(t *testing.T) {
	f := newP10BFail(t, true, false)
	f.press(f.buttons[0][1].Data, b1Chat)
	out := f.press(f.buttons[0][1].Data, b1Chat)
	if out.code != "stale" || f.jobs.cancels != 1 {
		t.Fatalf("second cancel %+v cancels %d", out, f.jobs.cancels)
	}
	if rm := f.removed(); len(rm) != 0 {
		t.Fatalf("the buttons were removed: %v", rm)
	}
	if f.kbd.setCount() != 2 {
		t.Fatalf("keyboard edits %d", f.kbd.setCount())
	}
	f.requireOnlyReportButton(t, f.kbd.sets[1])
}

// R-B-6: Retry is unchanged: it removes the buttons and edits nothing.
func TestP10FixB_RetryStillRemovesButtons(t *testing.T) {
	f := newP10BFail(t, true, false)
	if out := f.press(f.buttons[0][0].Data, b1Chat); out.code != "" || len(f.jobs.retries) != 1 {
		t.Fatalf("retry %+v retries %v", out, f.jobs.retries)
	}
	if rm := f.removed(); len(rm) != 1 || rm[0] != 701 || f.kbd.setCount() != 0 {
		t.Fatalf("removed %v edits %d", rm, f.kbd.setCount())
	}
}

// R-B-6: a failure message without the report button (an approval without the flag) loses all buttons on Cancel.
func TestP10FixB_CancelWithoutReportFlagRemovesAll(t *testing.T) {
	f := newP10BFail(t, false, false)
	if got := p10ButtonRows(f.buttons); len(got) != 1 || got[0] != "Thử lại|Huỷ" {
		t.Fatalf("buttons %v", got)
	}
	if out := f.press(f.buttons[0][1].Data, b1Chat); out.code != "" || f.jobs.cancels != 1 {
		t.Fatalf("cancel %+v", out)
	}
	if rm := f.removed(); len(rm) != 1 || rm[0] != 701 || f.kbd.setCount() != 0 {
		t.Fatalf("removed %v edits %d", rm, f.kbd.setCount())
	}
}

// R-B-6: a messenger that cannot edit a keyboard falls back to removing all buttons.
func TestP10FixB_CancelWithoutKeyboardEditorRemovesAll(t *testing.T) {
	f := newP10BFail(t, true, true)
	if out := f.press(f.buttons[0][1].Data, b1Chat); out.code != "" {
		t.Fatalf("cancel %+v", out)
	}
	if rm := f.removed(); len(rm) != 1 || rm[0] != 701 {
		t.Fatalf("removed %v", rm)
	}
}

// R-B-6: a failed keyboard edit never loses the report button: nothing is removed, and the next Cancel tries again.
func TestP10FixB_FailedKeyboardEditKeepsButtons(t *testing.T) {
	f := newP10BFail(t, true, false)
	f.kbd.setErr = errors.New("telegram down")
	f.press(f.buttons[0][1].Data, b1Chat)
	if rm := f.removed(); len(rm) != 0 || f.kbd.setCount() != 1 {
		t.Fatalf("removed %v edits %d", rm, f.kbd.setCount())
	}
	f.kbd.setErr = nil
	f.press(f.buttons[0][1].Data, b1Chat)
	if f.kbd.setCount() != 2 || len(f.removed()) != 0 {
		t.Fatalf("edits %d removed %v", f.kbd.setCount(), f.removed())
	}
	f.requireOnlyReportButton(t, f.kbd.sets[1])
}

// R-B-6 (abuse): a Cancel press from another chat edits no keyboard and does not cancel the job.
func TestP10FixB_CancelFromOtherChatEditsNothing(t *testing.T) {
	f := newP10BFail(t, true, false)
	f.press(f.buttons[0][1].Data, b1Chat+1)
	if f.jobs.cancels != 0 || f.kbd.setCount() != 0 {
		t.Fatalf("cancels %d edits %d", f.jobs.cancels, f.kbd.setCount())
	}
}
