package worker

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// TD-7A-4: a 401 during the outbox flush must neither drop nor duplicate an
// entry, must stop on shutdown with the entry still pending, and must log a
// fixed text without the token.
func TestSender_UnauthorizedDuringFlushKeepsEntryAcrossRestart(t *testing.T) {
	buf := &bytes.Buffer{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	j, state := newTestJournal(t, "job-401-restart")
	if err := j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{}); err != nil {
		t.Fatal(err)
	}
	if err := j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	// The hub keeps answering 401 (token revoked or expired); the worker is stopped after a few retries.
	ctx, cancel := context.WithCancel(context.Background())
	sleeps := 0
	hub := &scriptHub{failFor: func(string, int) error {
		return errors.Join(ErrUnauthorized, errors.New("HTTP 401 token=SECRET-DEVICE-TOKEN"))
	}}
	s := &Sender{Hub: hub, Sleep: func(c context.Context, _ time.Duration) error {
		sleeps++
		if sleeps == 3 {
			cancel()
		}
		return c.Err()
	}}
	if err := s.Flush(ctx, j); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush after shutdown: %v", err)
	}
	if hub.calls["progress"] != 3 || hub.calls["result"] != 0 {
		t.Fatalf("calls %v: the same first entry must be retried, the result must wait behind it", hub.calls)
	}
	if got := len(j.State().Pending); got != 2 {
		t.Fatalf("pending %d, a 401 must keep every entry", got)
	}
	if strings.Contains(buf.String(), "SECRET-DEVICE-TOKEN") || !strings.Contains(buf.String(), "device token rejected") {
		t.Fatalf("log %q", buf.String())
	}
	if len(hub.progress) != 0 || len(hub.results) != 0 {
		t.Fatalf("nothing was accepted: progress=%v results=%v", hub.progress, hub.results)
	}
	j.Close()

	// After a restart with a good token the same entries go out once, in order, with the original key.
	j2, err := OpenJournal(filepath.Join(jobsDir(state), "job-401-restart"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if got := len(j2.State().Pending); got != 2 {
		t.Fatalf("pending after restart %d", got)
	}
	good := &scriptHub{}
	if err := (&Sender{Hub: good, Sleep: fastSleep}).Flush(context.Background(), j2); err != nil {
		t.Fatal(err)
	}
	if len(good.progress) != 1 || len(good.results) != 1 || good.results[0] != "job-401-restart:1:result" {
		t.Fatalf("progress=%v results=%v", good.progress, good.results)
	}
	if len(j2.State().Pending) != 0 {
		t.Fatal("outbox not empty")
	}
}
