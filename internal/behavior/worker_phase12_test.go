package behavior

import (
	"strings"
	"testing"
)

// TC-25 (REQ-5): a Retry button pressed at the retry limit says so in
// Vietnamese, closes nothing (Cancel still works), tells the topic nothing,
// and a second press (double tap) gets the same answer.
func TestPhase12RetryButtonAtLimitKeepsButtonsOpen(t *testing.T) {
	e := b3Buttons(t, WorkerJobInterrupted)
	e.jobs.retryErr = ErrWorkerRetryLimit
	for i := 1; i <= 2; i++ {
		out := e.press(0, b1Chat)
		if out.code != WorkerReasonAttemptsExhausted || e.lastToast() != msgWorkerRetryLimit {
			t.Fatalf("press %d: code %q toast %q", i, out.code, e.lastToast())
		}
		if len(e.m.removed) != 0 {
			t.Fatalf("press %d: buttons were removed at the limit", i)
		}
		if len(e.queued.jobs) != 0 {
			t.Fatalf("press %d: topic told the job waits again", i)
		}
	}
	if len(e.jobs.retries) != 2 {
		t.Fatalf("retries %v", e.jobs.retries)
	}
	if out := e.press(1, b1Chat); out.code != "" || e.jobs.cancels != 1 || e.lastToast() != "Đã huỷ." {
		t.Fatalf("cancel after limit: %q cancels %d toast %q", out.code, e.jobs.cancels, e.lastToast())
	}
}

// TC-25 (REQ-5): the limit text is short, human, names the limit and the way
// out, and carries no internal error text.
func TestPhase12RetryLimitTextIsHumanAndShort(t *testing.T) {
	text := msgWorkerRetryLimit
	if !strings.Contains(text, "5") || !strings.Contains(text, "/task") {
		t.Fatalf("limit text %q lacks the limit or the next step", text)
	}
	if len([]rune(text)) > 120 {
		t.Fatalf("limit text is %d runes", len([]rune(text)))
	}
	for _, bad := range []string{"ErrWorkerRetryLimit", "retry limit reached", "conflict", "attempts_exhausted"} {
		if strings.Contains(text, bad) {
			t.Fatalf("limit text leaks %q: %q", bad, text)
		}
	}
	if WorkerMaxAttempts != 5 {
		t.Fatalf("WorkerMaxAttempts = %d, want 5", WorkerMaxAttempts)
	}
}

// TC-25 (REQ-5): /retry WF-n at the limit answers with the limit text and the
// workflow reference, and does not requeue or tell the topic.
func TestPhase12RetryCommandAtLimit(t *testing.T) {
	e := b3Buttons(t, WorkerJobFailed)
	e.jobs.job.State = WorkerJobInterrupted
	e.jobs.retryErr = ErrWorkerRetryLimit
	out, text := e.retryCmd("WF-41")
	if text != "WF-41: "+msgWorkerRetryLimit {
		t.Fatalf("text %q (code %q)", text, out.code)
	}
	if len(e.queued.jobs) != 0 {
		t.Fatal("topic told the job was requeued")
	}
	// A plain conflict is still the stale answer, not the limit text.
	e.jobs.retryErr = ErrConflict
	if _, text := e.retryCmd("WF-41"); text != msgStale {
		t.Fatalf("conflict text %q", text)
	}
}

// TC-26 (REQ-5): a job failed by the 24 h sweep (lease_expired) sends one
// message in plain Vietnamese with Retry and Cancel; no raw code leaks.
func TestPhase12LeaseExpiredFailureNotice(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	j := b3Job(WorkerJobFailed)
	j.Result = &WorkerResult{Status: "failed", ReasonCode: WorkerReasonLeaseExpired}
	e.change(WorkerChangeFinished, j)
	if len(e.m.sent) != 1 {
		t.Fatalf("messages %v, want exactly one", e.m.texts())
	}
	got := e.m.texts()[0]
	if !strings.HasPrefix(got, "WF-41: ") || !strings.Contains(got, "24 giờ") {
		t.Fatalf("text %q", got)
	}
	if strings.Contains(got, "mã lỗi") || strings.Contains(got, WorkerReasonLeaseExpired) {
		t.Fatalf("raw reason code leaked: %q", got)
	}
	if b := e.m.sent[0].Buttons; len(b) != 1 || len(b[0]) != 2 || b[0][0].Text != btnWorkerRetry || b[0][1].Text != btnWorkerCancel {
		t.Fatalf("buttons %+v", b)
	}
}
