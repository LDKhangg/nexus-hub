package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

// drainFunc is a serviceWorker that also drains (RunDrain), like the
// workflow, notice and inbox workers.
type drainFunc struct {
	run func(stop, hard context.Context) error
}

func (d drainFunc) Run(ctx context.Context) error { return d.run(ctx, ctx) }
func (d drainFunc) RunDrain(stop, hard context.Context) error {
	return d.run(stop, hard)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TC-14 (REQ-3): a failing startup pass is logged by name only, the next
// passes still run, and recoverAtStartup returns (not fatal).
func TestRecoverAtStartup_FailingPassIsLoggedNotFatal(t *testing.T) {
	buf := captureLog(t)
	var ran []string
	passes := []recoveryPass{
		{name: "first", run: func(context.Context) error {
			ran = append(ran, "first")
			return errors.New("db password=hunter2 token=abc")
		}},
		{name: "second", run: func(context.Context) error { ran = append(ran, "second"); return nil }},
	}
	recoverAtStartup(context.Background(), passes, time.Second)
	if strings.Join(ran, ",") != "first,second" {
		t.Fatalf("passes run = %v, want both", ran)
	}
	out := buf.String()
	if !strings.Contains(out, "first") || !strings.Contains(out, "failed=1") {
		t.Fatalf("failure not logged by name: %q", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "token=abc") {
		t.Fatalf("error text leaked into the log: %q", out)
	}
}

// TC-14 (REQ-3): a pass that hangs is cut by the timeout; the passes after
// it are counted as failed and not started; the call returns.
func TestRecoverAtStartup_HangingPassIsBoundedByTimeout(t *testing.T) {
	buf := captureLog(t)
	var laterRan atomic.Bool
	passes := []recoveryPass{
		{name: "hangs", run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{name: "later", run: func(context.Context) error { laterRan.Store(true); return nil }},
	}
	done := make(chan struct{})
	go func() { recoverAtStartup(context.Background(), passes, 50*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup recovery was not bounded by its timeout")
	}
	if laterRan.Load() {
		t.Fatal("a pass started after the recovery timeout ended")
	}
	if !strings.Contains(buf.String(), "failed=2") {
		t.Fatalf("log = %q, want failed=2", buf.String())
	}
}

// TC-14 (REQ-3): no passes and all-green passes log failed=0.
func TestRecoverAtStartup_AllGreen(t *testing.T) {
	buf := captureLog(t)
	recoverAtStartup(context.Background(), nil, time.Second)
	recoverAtStartup(context.Background(), []recoveryPass{{name: "ok", run: func(context.Context) error { return nil }}}, time.Second)
	if strings.Count(buf.String(), "failed=0") != 2 {
		t.Fatalf("log = %q", buf.String())
	}
}

// TC-15 (REQ-4): on a signal a draining worker stops claiming (stop ends),
// its in-flight work finishes inside shutdown_timeout without being cut
// (hard stays open), and runServices returns nil.
func TestRunServices_DrainsInFlightWorkAndReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	finish := make(chan struct{})
	var hardSeen atomic.Bool
	var finished atomic.Bool
	w := drainFunc{run: func(stop, hard context.Context) error {
		close(started)
		<-stop.Done()
		select {
		case <-finish:
			finished.Store(true)
		case <-hard.Done():
			hardSeen.Store(true)
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- runServices(ctx, echo.New(), []serviceWorker{w}, "127.0.0.1:0", 5*time.Second) }()
	<-started
	cancel()
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("runServices returned while in-flight work was running")
	default:
	}
	close(finish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit error = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runServices did not return after the work finished")
	}
	if !finished.Load() || hardSeen.Load() {
		t.Fatalf("finished=%v hardCut=%v, want finished and not cut", finished.Load(), hardSeen.Load())
	}
}

// TC-16 (REQ-4): work still running at shutdown_timeout is cut through hard,
// and runServices still returns nil.
func TestRunServices_CutsInFlightWorkAtDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var cutAfter atomic.Int64
	var cancelledAt atomic.Int64
	w := drainFunc{run: func(stop, hard context.Context) error {
		close(started)
		<-stop.Done()
		t0 := time.Now()
		<-hard.Done()
		cutAfter.Store(int64(time.Since(t0)))
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- runServices(ctx, echo.New(), []serviceWorker{w}, "127.0.0.1:0", 300*time.Millisecond) }()
	<-started
	cancelledAt.Store(time.Now().UnixNano())
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServices did not cut work at the deadline")
	}
	if d := time.Duration(cutAfter.Load()); d < 100*time.Millisecond {
		t.Fatalf("work cut after %v, before the deadline", d)
	}
	if el := time.Since(time.Unix(0, cancelledAt.Load())); el > 3*time.Second {
		t.Fatalf("shutdown took %v with a 300ms timeout", el)
	}
}

// TC-19 (REQ-4): after the signal the hub refuses new webhooks (the
// listener closes) while an in-flight request still gets its answer.
func TestRunServices_InFlightRequestAnsweredNewOnesRefused(t *testing.T) {
	addr := freeAddr(t)
	e := echo.New()
	var hits atomic.Int32
	inHandler := make(chan struct{})
	release := make(chan struct{})
	e.POST("/webhook/x", func(c echo.Context) error {
		if hits.Add(1) == 1 {
			close(inHandler)
			<-release
		}
		return c.String(http.StatusOK, "stored")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refused := make(chan bool, 1)
	w := drainFunc{run: func(stop, _ context.Context) error {
		<-stop.Done()
		time.Sleep(50 * time.Millisecond)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Post("http://"+addr+"/webhook/x", "application/json", strings.NewReader("{}"))
			if err != nil {
				refused <- true
				return nil
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			time.Sleep(20 * time.Millisecond)
		}
		refused <- false
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- runServices(ctx, e, []serviceWorker{w}, addr, 5*time.Second) }()
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if i > 100 {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	type result struct {
		code int
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+addr+"/webhook/x", "application/json", strings.NewReader("{}"))
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		res <- result{code: resp.StatusCode}
	}()
	<-inHandler
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case r := <-res:
		if r.err != nil || r.code != http.StatusOK {
			t.Fatalf("in-flight request: code=%d err=%v", r.code, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never answered")
	}
	select {
	case ok := <-refused:
		if !ok {
			t.Fatal("new webhooks were still accepted after shutdown began")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker never saw stop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServices did not return")
	}
}

// TC-32 (REQ-7): when the queued-job count cannot be read (database error)
// the starter keeps starting work, with one log line and no stall; a cap of 0
// or less turns backpressure off without touching the database.
func TestStartHeadroom_CountErrorStartsAnyway(t *testing.T) {
	buf := captureLog(t)
	// A pool that never connects: the count query fails at once.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nopass@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := storage.NewWorkflowStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	r := &workflowRuntime{store: store, queueMaxPending: 500}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := r.startHeadroom(ctx); got <= 0 {
		t.Fatalf("headroom = %d after a count error, want > 0 (start anyway)", got)
	}
	if r.backpressured {
		t.Fatal("a count error switched backpressure on")
	}
	if out := buf.String(); !strings.Contains(out, "queued job count unavailable") || strings.Contains(out, "nopass") {
		t.Fatalf("log = %q", out)
	}
	for _, off := range []int64{0, -1} {
		r := &workflowRuntime{store: store, queueMaxPending: off}
		if got := r.startHeadroom(ctx); got <= workflowStartBatch {
			t.Fatalf("cap %d: headroom %d, want unlimited", off, got)
		}
	}
}
