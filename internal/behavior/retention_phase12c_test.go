package behavior

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type phase12cRetentionStore struct {
	mu     sync.Mutex
	calls  int
	nows   []time.Time
	batch  []int32
	result RetentionResult
	err    error
	called chan struct{}
}

func (s *phase12cRetentionStore) CountRetention(context.Context, string, RetentionRules, time.Time) (map[string]int64, error) {
	return nil, errors.New("unexpected count")
}

func (s *phase12cRetentionStore) SweepRetention(_ context.Context, _ string, _ RetentionRules, now time.Time, batch int32) (RetentionResult, error) {
	s.mu.Lock()
	s.calls++
	s.nows = append(s.nows, now)
	s.batch = append(s.batch, batch)
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	return s.result, s.err
}

type phase12cRetentionMetrics struct {
	mu   sync.Mutex
	seen map[string]int64
}

func (m *phase12cRetentionMetrics) RetentionDeleted(table string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[table] += n
}

// TC-C-14..15 (REQ-11): immediate/ticked lifecycle, cancellation, generic logs, and closed metrics input.
func TestPhase12cRetentionLifecycleAndObservability(t *testing.T) {
	if err := (&Retention{}).Run(context.Background()); err == nil {
		t.Fatal("nil store accepted")
	}
	fixed := time.Date(2026, 11, 20, 1, 2, 3, 0, time.FixedZone("private", 3600))
	store := &phase12cRetentionStore{called: make(chan struct{}, 4), result: RetentionResult{
		Counts: map[string]int64{"tool_runs": 2, "message_logs": 1},
		Issues: []RetentionIssue{{Table: "workflow_executions", Code: "23503"}},
	}}
	metrics := &phase12cRetentionMetrics{seen: map[string]int64{}}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Retention{Store: store, Metrics: metrics, Enabled: true, Interval: 5 * time.Millisecond, Now: func() time.Time { return fixed }}).Run(ctx)
	}()
	for range 2 {
		select {
		case <-store.called:
		case <-time.After(time.Second):
			t.Fatal("retention pass did not run")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if store.calls < 2 || !store.nows[0].Equal(fixed.UTC()) || store.batch[0] != DefaultRetentionBatch {
		t.Errorf("calls=%d now=%v batch=%v", store.calls, store.nows, store.batch)
	}
	store.mu.Unlock()
	out := logs.String()
	for _, want := range []string{"message_logs=1", "tool_runs=2", "workflow_executions_code=23503"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q: %s", want, out)
		}
	}
	for _, secret := range []string{"private", fixed.String()} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaked %q", secret)
		}
	}
	metrics.mu.Lock()
	if metrics.seen["tool_runs"] < 2 || metrics.seen["message_logs"] < 1 {
		t.Errorf("metrics = %v", metrics.seen)
	}
	metrics.mu.Unlock()

	disabled := &phase12cRetentionStore{called: make(chan struct{}, 1)}
	disabledCtx, stop := context.WithCancel(context.Background())
	disabledDone := make(chan error, 1)
	go func() { disabledDone <- (&Retention{Store: disabled, Enabled: false}).Run(disabledCtx) }()
	stop()
	if err := <-disabledDone; err != nil || disabled.calls != 0 {
		t.Fatalf("disabled calls=%d err=%v", disabled.calls, err)
	}
}

func TestPhase12cRetentionDatabaseErrorIsSanitized(t *testing.T) {
	store := &phase12cRetentionStore{called: make(chan struct{}, 1), err: errors.New("password=hunter2 prompt=private")}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	ctx, cancel := context.WithCancel(context.Background())
	store.called = make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- (&Retention{Store: store, Enabled: true, Interval: time.Hour}).Run(ctx) }()
	<-store.called
	cancel()
	<-done
	if got := logs.String(); !strings.Contains(got, "database_error") || strings.Contains(got, "hunter2") || strings.Contains(got, "private") {
		t.Fatalf("unsafe log: %q", got)
	}
}
