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
	go func() { done <- runServices(ctx, echo.New(), worker, "127.0.0.1:0", time.Second) }()
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
	if err := runServices(context.Background(), echo.New(), worker, listener.Addr().String(), time.Second); err == nil {
		t.Fatal("listen error swallowed")
	}
}
