package main

import (
	"context"
	"github.com/labstack/echo/v4"
	"net"
	"testing"
	"time"
)

type workerFunc func(context.Context) error

func (f workerFunc) Run(ctx context.Context) error { return f(ctx) }
func TestServiceShutdownWaitsForWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	finished := make(chan struct{})
	worker := workerFunc(func(ctx context.Context) error { close(started); <-ctx.Done(); close(finished); return nil })
	done := make(chan error, 1)
	go func() { done <- runServices(ctx, echo.New(), []serviceWorker{worker}, "127.0.0.1:0", time.Second) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked")
	}
	select {
	case <-finished:
	default:
		t.Fatal("returned before worker stopped")
	}
}
func TestServerListenFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	worker := workerFunc(func(context.Context) error { t.Error("worker started after bind failure"); return nil })
	if err := runServices(context.Background(), echo.New(), []serviceWorker{worker}, listener.Addr().String(), time.Second); err == nil {
		t.Fatal("listen error swallowed")
	}
}

func TestRunServices_AnyWorkerStopShutsDown(t *testing.T) {
	stoppedOther := make(chan struct{})
	quitter := workerFunc(func(context.Context) error { return nil })
	other := workerFunc(func(ctx context.Context) error { <-ctx.Done(); close(stoppedOther); return nil })
	done := make(chan error, 1)
	go func() {
		done <- runServices(context.Background(), echo.New(), []serviceWorker{other, quitter}, "127.0.0.1:0", time.Second)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("worker exit not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("one stopped worker did not shut the service down")
	}
	select {
	case <-stoppedOther:
	default:
		t.Fatal("returned before the other worker stopped")
	}
}

func TestRunServices_WaitsForAllWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var finished [3]chan struct{}
	var workers []serviceWorker
	for i := range finished {
		ch := make(chan struct{})
		finished[i] = ch
		delay := time.Duration(i*50) * time.Millisecond
		workers = append(workers, workerFunc(func(ctx context.Context) error { <-ctx.Done(); time.Sleep(delay); close(ch); return nil }))
	}
	done := make(chan error, 1)
	go func() { done <- runServices(ctx, echo.New(), workers, "127.0.0.1:0", time.Second) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i, ch := range finished {
		select {
		case <-ch:
		default:
			t.Fatalf("worker %d still running after return", i)
		}
	}
}
