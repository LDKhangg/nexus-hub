package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Regression tests for review finding R-5 (fix 316dbf8): a poll that started
// before a lease was issued must never rotate that lease.

// R-5: poll A starts, poll B leases, poll A rechecks with its earlier start and gets nothing.
func TestWorkerR5_StalePollCannotRotateLease(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	pollA := e.s.PollStart()
	e.advance(5 * time.Second)
	j := e.job(t, workspaceA, nil, "")
	l1 := e.mustLease(t, d) // poll B
	for i := 0; i < 3; i++ {
		l, ok, err := e.s.LeasePoll(e.ctx, d, pollA)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("stale poll got a lease: job %s token rotated=%v", l.Job.ID, l.LeaseToken != l1.LeaseToken)
		}
		e.advance(10 * time.Second) // later rechecks of the same stale poll
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l1.LeaseToken); err != nil {
		t.Fatalf("first token refused after stale rechecks: %v", err)
	}
	if hasAnyEvent(e.events(t), "lease_reissued") {
		t.Fatalf("stale poll recorded a reissue: %v", e.events(t))
	}
}

// R-5: same instant is not "after": a poll that started at the lease time is stale.
func TestWorkerR5_PollStartedAtLeaseInstantIsStale(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	pollA := e.s.PollStart()
	l1 := e.mustLease(t, d)
	if _, ok, err := e.s.LeasePoll(e.ctx, d, pollA); err != nil || ok {
		t.Fatalf("poll started at the lease instant rotated it: ok=%v err=%v", ok, err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l1.LeaseToken); err != nil {
		t.Fatalf("first token refused: %v", err)
	}
}

// R-5 over HTTP: a waiting poll that began before the job must not rotate the token another poll delivered.
func TestWorkerR5_HTTPWaitingPollDoesNotRotateDeliveredToken(t *testing.T) {
	h := newW7Hub(t)
	_, tok := h.enroll(t, workspaceA, "laptop")
	// Poll A reports a blocking snapshot so it cannot take the new job itself.
	blocked := map[string]any{
		"state": "idle", "claude_session_left_pct": 1,
		"claude_session_reset_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}
	type res struct {
		r    hubResp
		took time.Duration
	}
	done := make(chan res, 1)
	go func() {
		start := time.Now()
		r := h.poll(t, tok, 5, blocked)
		done <- res{r, time.Since(start)}
	}()
	time.Sleep(700 * time.Millisecond)
	j := h.job(t, workspaceA, nil, `{"instruction":"x"}`)    // wakes poll A
	time.Sleep(300 * time.Millisecond)                       // no clock change: poll A reads the clock concurrently
	rb := h.poll(t, tok, 0, map[string]any{"state": "idle"}) // poll B, unblocked
	if rb.code != 200 || rb.body["job_id"] != j.ID {
		t.Fatalf("poll B: %d %s", rb.code, rb.raw)
	}
	t1, _ := rb.body["lease_token"].(string)
	if t1 == "" {
		t.Fatal("poll B returned no lease token")
	}
	select {
	case got := <-done:
		if got.r.code != 204 {
			t.Fatalf("stale poll A ended with %d %s, want 204", got.r.code, got.r.raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("poll A never ended")
	}
	hb := "/worker/v1/jobs/" + j.ID + "/heartbeat"
	if r := h.do(t, hb, tok, map[string]any{"lease_token": t1}); r.code != 200 {
		t.Fatalf("first token heartbeat after stale poll: %d %s", r.code, r.raw)
	}
}

// R-5 keeps R-1: a poll that started after the lease still reissues it.
func TestWorkerR5_PollStartedAfterLeaseStillReissues(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l1 := e.mustLease(t, d)
	e.advance(time.Second)
	pollC := e.s.PollStart()
	l2, ok, err := e.s.LeasePoll(e.ctx, d, pollC)
	if err != nil || !ok {
		t.Fatalf("later poll got no reissue: ok=%v err=%v", ok, err)
	}
	if l2.Job.ID != j.ID || l2.Job.Attempt != l1.Job.Attempt || l2.LeaseToken == "" || l2.LeaseToken == l1.LeaseToken {
		t.Fatalf("bad reissue: %+v", l2.Job)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l1.LeaseToken); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("old token still valid: %v", err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l2.LeaseToken); err != nil {
		t.Fatalf("new token refused: %v", err)
	}
}

// R-5 over HTTP: R-1 stays: a later poll gets a new token, the old one is 409.
func TestWorkerR5_HTTPLaterPollReissues(t *testing.T) {
	h := newW7Hub(t)
	_, tok := h.enroll(t, workspaceA, "laptop")
	j := h.job(t, workspaceA, nil, `{"instruction":"x"}`)
	r1 := h.poll(t, tok, 0, nil)
	if r1.code != 200 {
		t.Fatalf("first poll: %d", r1.code)
	}
	h.advance(time.Second)
	r2 := h.poll(t, tok, 0, nil)
	if r2.code != 200 || r2.body["lease_token"] == r1.body["lease_token"] || r2.body["job_id"] != j.ID {
		t.Fatalf("second poll: %d %s", r2.code, r2.raw)
	}
	base := "/worker/v1/jobs/" + j.ID + "/heartbeat"
	if r := h.do(t, base, tok, map[string]any{"lease_token": r1.body["lease_token"]}); r.code != 409 {
		t.Fatalf("old token: %d", r.code)
	}
	if r := h.do(t, base, tok, map[string]any{"lease_token": r2.body["lease_token"]}); r.code != 200 {
		t.Fatalf("new token: %d", r.code)
	}
}

// R-5: a reissue counts as a new issuance, so a poll that started between T1 and T2 cannot rotate T2.
func TestWorkerR5_PollBetweenIssuesCannotRotateReissue(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l1 := e.mustLease(t, d)
	e.advance(time.Minute)
	pollMid := e.s.PollStart() // starts after T1, before T2
	e.advance(time.Minute)
	l2 := e.mustLease(t, d) // fresh poll reissues: T2
	if l2.LeaseToken == l1.LeaseToken {
		t.Fatal("no reissue")
	}
	for i := 0; i < 2; i++ {
		if _, ok, err := e.s.LeasePoll(e.ctx, d, pollMid); err != nil || ok {
			t.Fatalf("poll that began before T2 rotated it: ok=%v err=%v", ok, err)
		}
		e.advance(5 * time.Second)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l2.LeaseToken); err != nil {
		t.Fatalf("T2 refused: %v", err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l1.LeaseToken); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("T1 still valid: %v", err)
	}
}

// R-5: a LEASED job reissues once per poll generation: a poll after T2 may rotate it again.
func TestWorkerR5_PollAfterReissueRotatesAgain(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	e.job(t, workspaceA, nil, "")
	e.mustLease(t, d)
	e.advance(time.Minute)
	l2 := e.mustLease(t, d)
	e.advance(time.Minute)
	l3, ok, err := e.s.LeasePoll(e.ctx, d, e.s.PollStart())
	if err != nil || !ok || l3.LeaseToken == l2.LeaseToken {
		t.Fatalf("poll after T2 did not reissue: ok=%v err=%v", ok, err)
	}
}
