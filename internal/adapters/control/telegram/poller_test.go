package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// The poller has no injectable clock: backoff tests use millisecond
// bounds and assert lower bounds (the waits really happened) plus a loose
// upper bound (the cap holds). Everything else is driven by channels.

const waitLong = 3 * time.Second

// syncBuffer captures the standard logger safely across goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

type getReply struct {
	updates []json.RawMessage
	err     error
}

type getCall struct {
	workspaceID, tokenRef string
	offset                int64
	timeout               int
	allowed               []string
	at                    time.Time
	reply                 chan getReply
	ctx                   context.Context
}

// fakeUpdatesAPI hands every GetUpdates call to the test, which answers it
// on call.reply. A call whose context ends returns ctx.Err() and is
// reported on canceled.
type fakeUpdatesAPI struct {
	calls    chan *getCall
	canceled chan string // tokenRef of a call stopped by its context
	inflight atomic.Int64

	mu           sync.Mutex
	fingerprints map[string]string // tokenRef -> fingerprint
	fpErr        map[string]error
	deletes      []string
	deleteErr    error
	onDelete     func() // called inside DeleteWebhook, before it returns
}

func newFakeAPI() *fakeUpdatesAPI {
	return &fakeUpdatesAPI{calls: make(chan *getCall, 64), canceled: make(chan string, 64), fingerprints: map[string]string{}, fpErr: map[string]error{}}
}

func (f *fakeUpdatesAPI) GetUpdates(ctx context.Context, workspaceID, tokenRef string, offset int64, timeoutSec int, allowed []string) ([]json.RawMessage, error) {
	f.inflight.Add(1)
	defer f.inflight.Add(-1)
	c := &getCall{workspaceID: workspaceID, tokenRef: tokenRef, offset: offset, timeout: timeoutSec, allowed: append([]string(nil), allowed...), at: time.Now(), reply: make(chan getReply, 1), ctx: ctx}
	select {
	case f.calls <- c:
	case <-ctx.Done():
		f.canceled <- tokenRef
		return nil, ctx.Err()
	}
	select {
	case r := <-c.reply:
		return r.updates, r.err
	case <-ctx.Done():
		f.canceled <- tokenRef
		return nil, ctx.Err()
	}
}

func (f *fakeUpdatesAPI) DeleteWebhook(_ context.Context, _, tokenRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, tokenRef)
	if f.onDelete != nil {
		f.onDelete()
	}
	return f.deleteErr
}

func (f *fakeUpdatesAPI) BotFingerprint(_ context.Context, _, tokenRef string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fpErr[tokenRef]; err != nil {
		return "", err
	}
	if fp, ok := f.fingerprints[tokenRef]; ok {
		return fp, nil
	}
	return "fp-" + tokenRef, nil
}

func (f *fakeUpdatesAPI) setFingerprint(ref, fp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fingerprints[ref] = fp
}

func (f *fakeUpdatesAPI) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

func (f *fakeUpdatesAPI) next(t *testing.T) *getCall {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(waitLong):
		t.Fatal("no getUpdates call")
		return nil
	}
}

// noCall asserts no getUpdates call arrives within d.
func (f *fakeUpdatesAPI) noCall(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case c := <-f.calls:
		t.Fatalf("unexpected getUpdates call token=%s offset=%d", c.tokenRef, c.offset)
	case <-time.After(d):
	}
}

type fakeTargets struct {
	mu      sync.Mutex
	targets []PollTarget
	err     error
	listed  chan struct{}
}

func newFakeTargets(ts ...PollTarget) *fakeTargets {
	return &fakeTargets{targets: ts, listed: make(chan struct{}, 1024)}
}

func (f *fakeTargets) TelegramPollTargets(context.Context) ([]PollTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.listed <- struct{}{}:
	default:
	}
	if f.err != nil {
		return nil, f.err
	}
	return append([]PollTarget(nil), f.targets...), nil
}

func (f *fakeTargets) set(ts ...PollTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = ts
}

// waitListed drops earlier listings, then waits for n new ones.
func (f *fakeTargets) waitListed(t *testing.T, n int) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-f.listed:
		default:
			drained = true
		}
	}
	for i := 0; i < n; i++ {
		select {
		case <-f.listed:
		case <-time.After(waitLong):
			t.Fatal("integration list not read again")
		}
	}
}

type handled struct {
	integrationID string
	id            int64
}

type fakeHandler struct {
	mu     sync.Mutex
	seen   []handled
	result func(id int64) (string, error)
}

func (h *fakeHandler) HandleUpdate(_ context.Context, integrationID string, raw []byte) (string, error) {
	id := rawUpdateID(raw)
	h.mu.Lock()
	h.seen = append(h.seen, handled{integrationID, id})
	f := h.result
	h.mu.Unlock()
	if f == nil {
		return "queued", nil
	}
	return f(id)
}

func (h *fakeHandler) ids() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.seen))
	for _, s := range h.seen {
		out = append(out, s.id)
	}
	return out
}

func upd(id int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"update_id":%d,"message":{"text":"secret text %d"}}`, id, id))
}

var targetA = PollTarget{WorkspaceID: "ws-a", IntegrationID: "int-a", TokenRef: "env:A"}
var targetB = PollTarget{WorkspaceID: "ws-b", IntegrationID: "int-b", TokenRef: "env:B"}

// startPoller runs p and returns a stop function that cancels it and
// asserts Run returned nil promptly.
func startPoller(t *testing.T, p *Poller) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run returned %v", err)
				}
			case <-time.After(waitLong):
				t.Error("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func fastSettings() PollerSettings {
	return PollerSettings{PollTimeout: 2 * time.Second, Refresh: time.Hour, MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
}

// TC-P7-10: defaults and the getUpdates arguments (timeout seconds,
// allowed_updates message and callback_query, integration's token ref).
func TestPollerDefaultsAndArguments(t *testing.T) {
	p := NewPoller(nil, nil, nil, PollerSettings{})
	if p.s.PollTimeout != 50*time.Second || p.s.Refresh != time.Minute || p.s.MinBackoff != time.Second || p.s.MaxBackoff != 30*time.Second {
		t.Fatalf("defaults %+v", p.s)
	}
	p = NewPoller(nil, nil, nil, PollerSettings{PollTimeout: 500 * time.Millisecond, MinBackoff: 5 * time.Second, MaxBackoff: time.Second})
	if p.s.PollTimeout != 50*time.Second || p.s.MaxBackoff < p.s.MinBackoff {
		t.Fatalf("sub-second timeout or max<min not corrected: %+v", p.s)
	}

	api, targets := newFakeAPI(), newFakeTargets(targetA)
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, fastSettings()))
	c := api.next(t)
	if c.workspaceID != "ws-a" || c.tokenRef != "env:A" || c.offset != 0 || c.timeout != 2 {
		t.Fatalf("first call %+v", c)
	}
	if len(c.allowed) != 2 || c.allowed[0] != "message" || c.allowed[1] != "callback_query" {
		t.Fatalf("allowed_updates %v", c.allowed)
	}
	c.reply <- getReply{}
	if c2 := api.next(t); c2.offset != 0 {
		t.Fatalf("empty batch moved offset to %d", c2.offset)
	}
}

// TC-P7-11: the offset moves past an update only after the handler
// accepted it; a handler error keeps the offset (the update is offered
// again) and stops the batch; updates below the offset or without an
// update_id are not handled; invalid updates are confirmed and skipped.
func TestPollerOffset(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	var failOnce atomic.Bool
	failOnce.Store(true)
	h := &fakeHandler{result: func(id int64) (string, error) {
		switch {
		case id == 11 && failOnce.CompareAndSwap(true, false):
			return "", errors.New("store down")
		case id == 20:
			return updateInvalid, nil
		case id == 21:
			return "duplicate", nil
		case id == 22:
			return "ignored", nil
		}
		return "queued", nil
	}}
	startPoller(t, NewPoller(targets, api, h, fastSettings()))

	c := api.next(t)
	c.reply <- getReply{updates: []json.RawMessage{upd(10), upd(11), upd(12)}}
	c = api.next(t)
	if c.offset != 11 {
		t.Fatalf("after handler error offset=%d want 11", c.offset)
	}
	if got := h.ids(); fmt.Sprint(got) != "[10 11]" {
		t.Fatalf("handled %v: the batch must stop at the failed update", got)
	}
	c.reply <- getReply{updates: []json.RawMessage{upd(11), upd(12)}}
	c = api.next(t)
	if c.offset != 13 {
		t.Fatalf("offset=%d want 13", c.offset)
	}
	c.reply <- getReply{updates: []json.RawMessage{
		upd(5),                                 // below the offset: already confirmed
		json.RawMessage(`{"message":{}}`),      // no update_id
		json.RawMessage(`not json`),            // malformed
		json.RawMessage(`{"update_id":"abc"}`), // malformed id
		upd(20), upd(21), upd(22),
	}}
	c = api.next(t)
	if c.offset != 23 {
		t.Fatalf("offset=%d want 23 (invalid, duplicate and ignored are confirmed)", c.offset)
	}
	if got := h.ids(); fmt.Sprint(got) != "[10 11 11 12 20 21 22]" {
		t.Fatalf("handled %v", got)
	}
	for _, s := range h.seen {
		if s.integrationID != "int-a" {
			t.Fatalf("handler got integration %q", s.integrationID)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "update deferred integration=int-a update=11") || !strings.Contains(out, "invalid update skipped integration=int-a update=20") {
		t.Fatalf("logs %q", out)
	}
	if strings.Contains(out, "secret text") {
		t.Fatalf("message text logged: %q", out)
	}
}

// TC-P7-12: a handler error backs off before the batch is fetched again,
// and a batch with nothing to confirm (all below the offset) never spins.
func TestPollerHandlerErrorBacksOff(t *testing.T) {
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	h := &fakeHandler{result: func(id int64) (string, error) { return "", errors.New("down") }}
	s := fastSettings()
	s.MinBackoff, s.MaxBackoff = 60*time.Millisecond, 60*time.Millisecond
	startPoller(t, NewPoller(targets, api, h, s))
	c := api.next(t)
	sent := time.Now()
	c.reply <- getReply{updates: []json.RawMessage{upd(7)}}
	c = api.next(t)
	if gap := c.at.Sub(sent); gap < 60*time.Millisecond {
		t.Fatalf("refetched after %v, want >= 60ms backoff", gap)
	}
	if c.offset != 0 {
		t.Fatalf("offset moved to %d after handler error", c.offset)
	}
	// Recover, then a stale-only batch must also back off.
	h.mu.Lock()
	h.result = nil
	h.mu.Unlock()
	c.reply <- getReply{updates: []json.RawMessage{upd(7)}}
	c = api.next(t)
	if c.offset != 8 {
		t.Fatalf("offset=%d want 8", c.offset)
	}
	sent = time.Now()
	c.reply <- getReply{updates: []json.RawMessage{upd(3)}}
	c = api.next(t)
	if gap := c.at.Sub(sent); gap < 60*time.Millisecond || c.offset != 8 {
		t.Fatalf("stale batch: gap=%v offset=%d", gap, c.offset)
	}
}

// TC-P7-13: failures back off doubling from MinBackoff up to MaxBackoff;
// a success resets the backoff; retry_after is honoured when longer.
func TestPollerBackoff(t *testing.T) {
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	s := fastSettings()
	s.MinBackoff, s.MaxBackoff = 25*time.Millisecond, 400*time.Millisecond
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, s))
	serverErr := &relay.DeliveryError{Code: "server", Retryable: true}

	gaps := func(n int) []time.Duration {
		var out []time.Duration
		c := api.next(t)
		for i := 0; i < n; i++ {
			sent := time.Now()
			c.reply <- getReply{err: serverErr}
			c = api.next(t)
			out = append(out, c.at.Sub(sent))
		}
		c.reply <- getReply{} // success: reset
		return out
	}
	want := []time.Duration{25, 50, 100, 200, 400, 400, 400}
	got := gaps(len(want))
	for i, g := range got {
		if g < want[i]*time.Millisecond {
			t.Fatalf("gap %d=%v want >= %vms (gaps %v)", i, g, want[i], got)
		}
	}
	// Uncapped doubling would wait 1600ms on the 7th failure.
	if got[6] >= 1200*time.Millisecond {
		t.Fatalf("backoff not capped: %v", got)
	}
	// After the success the backoff starts again at MinBackoff.
	again := gaps(1)
	if again[0] < 25*time.Millisecond || again[0] >= 300*time.Millisecond {
		t.Fatalf("backoff not reset after success: %v", again)
	}

	t.Run("retry_after", func(t *testing.T) {
		c := api.next(t)
		sent := time.Now()
		c.reply <- getReply{err: &relay.DeliveryError{Code: "rate_limited", Retryable: true, RetryAfter: 300 * time.Millisecond}}
		c = api.next(t)
		if gap := c.at.Sub(sent); gap < 300*time.Millisecond {
			t.Fatalf("retry_after ignored: gap %v", gap)
		}
		c.reply <- getReply{}
	})
}

// TC-P7-14: a 409 conflict removes the webhook exactly once, logs the
// notice and polls again at once; later conflicts only back off.
func TestPollerConflictDeletesWebhookOnce(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	s := fastSettings()
	s.MinBackoff, s.MaxBackoff = 50*time.Millisecond, 50*time.Millisecond
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, s))
	conflict := &relay.DeliveryError{Code: "conflict", Retryable: true}

	c := api.next(t)
	c.reply <- getReply{err: conflict}
	c = api.next(t)
	if api.deleteCount() != 1 {
		t.Fatalf("deleteWebhook calls=%d", api.deleteCount())
	}
	if !strings.Contains(logs.String(), "telegram: webhook removed for polling") {
		t.Fatalf("notice missing: %q", logs.String())
	}
	for i := 0; i < 3; i++ {
		sent := time.Now()
		c.reply <- getReply{err: conflict}
		c = api.next(t)
		if gap := c.at.Sub(sent); gap < 50*time.Millisecond {
			t.Fatalf("conflict %d retried after %v without backoff", i+2, gap)
		}
	}
	if api.deleteCount() != 1 {
		t.Fatalf("deleteWebhook calls=%d, want exactly 1", api.deleteCount())
	}
	if n := strings.Count(logs.String(), "webhook removed for polling"); n != 1 {
		t.Fatalf("notice logged %d times", n)
	}
	c.reply <- getReply{}
}

// TC-P7-15 (R9): a failed deleteWebhook is logged with a safe code, backs
// off by the larger retry_after of getUpdates and deleteWebhook, and is
// tried again on the next conflict; after one successful delete a later
// conflict does not delete again.
func TestPollerConflictDeleteRetried(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	setDeleteErr := func(err error) {
		api.mu.Lock()
		api.deleteErr = err
		api.mu.Unlock()
	}
	setDeleteErr(&relay.DeliveryError{Code: "rate_limited", Retryable: true, RetryAfter: 150 * time.Millisecond})
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, fastSettings()))
	conflict := &relay.DeliveryError{Code: "conflict", Retryable: true}

	c := api.next(t)
	sent := time.Now()
	c.reply <- getReply{err: conflict}
	c = api.next(t)
	if gap := c.at.Sub(sent); gap < 150*time.Millisecond {
		t.Fatalf("deleteWebhook retry_after ignored: gap %v", gap)
	}
	if api.deleteCount() != 1 {
		t.Fatalf("deleteWebhook calls=%d want 1", api.deleteCount())
	}

	// Second conflict: the delete is tried again; getUpdates' retry_after
	// (250ms) is larger than deleteWebhook's (150ms) and wins.
	sent = time.Now()
	c.reply <- getReply{err: &relay.DeliveryError{Code: "conflict", Retryable: true, RetryAfter: 250 * time.Millisecond}}
	c = api.next(t)
	if gap := c.at.Sub(sent); gap < 250*time.Millisecond {
		t.Fatalf("larger retry_after not used: gap %v", gap)
	}
	if api.deleteCount() != 2 {
		t.Fatalf("failed delete not retried: calls=%d want 2", api.deleteCount())
	}
	out := logs.String()
	if n := strings.Count(out, "removing webhook failed integration=int-a code=rate_limited"); n != 2 {
		t.Fatalf("delete failure logged %d times: %q", n, out)
	}
	if strings.Contains(out, "webhook removed for polling") {
		t.Fatalf("notice logged although the delete failed: %q", out)
	}

	// Third conflict: the delete succeeds, the notice is logged and the
	// loop polls again at once.
	setDeleteErr(nil)
	c.reply <- getReply{err: conflict}
	c = api.next(t)
	if api.deleteCount() != 3 || strings.Count(logs.String(), "telegram: webhook removed for polling") != 1 {
		t.Fatalf("delete calls=%d logs=%q", api.deleteCount(), logs.String())
	}

	// Later conflicts never delete again.
	for i := 0; i < 2; i++ {
		c.reply <- getReply{err: conflict}
		c = api.next(t)
	}
	if api.deleteCount() != 3 {
		t.Fatalf("deleted again after success: calls=%d", api.deleteCount())
	}
	c.reply <- getReply{}
}

// TC-P7-15b (R9): a cancel while deleteWebhook runs stops the loop.
func TestPollerConflictDeleteCanceled(t *testing.T) {
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	api.deleteErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	api.onDelete = cancel
	done := make(chan error, 1)
	go func() { done <- NewPoller(targets, api, &fakeHandler{}, fastSettings()).Run(ctx) }()
	c := api.next(t)
	c.reply <- getReply{err: &relay.DeliveryError{Code: "conflict", Retryable: true}}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run=%v", err)
		}
	case <-time.After(waitLong):
		cancel()
		t.Fatal("Run did not return after cancel during deleteWebhook")
	}
	if api.deleteCount() != 1 || api.inflight.Load() != 0 {
		t.Fatalf("deletes=%d inflight=%d", api.deleteCount(), api.inflight.Load())
	}
}

// TC-P7-16: two integrations of the same bot (same fingerprint) are polled
// by one loop; the skip is logged once; when the polled one is removed the
// other takes over.
func TestPollerSharedBotPolledOnce(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA, targetB)
	api.setFingerprint("env:A", "same-bot")
	api.setFingerprint("env:B", "same-bot")
	s := fastSettings()
	s.Refresh = 20 * time.Millisecond
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, s))

	first := api.next(t)
	if first.tokenRef != "env:A" {
		t.Fatalf("first target polled %s", first.tokenRef)
	}
	targets.waitListed(t, 5)
	api.noCall(t, 50*time.Millisecond)
	if n := strings.Count(logs.String(), "integration=int-b reason=bot_shared"); n != 1 {
		t.Fatalf("bot_shared logged %d times: %q", n, logs.String())
	}

	targets.set(targetB)
	select {
	case ref := <-api.canceled:
		if ref != "env:A" {
			t.Fatalf("canceled %s", ref)
		}
	case <-time.After(waitLong):
		t.Fatal("removed integration's loop not stopped")
	}
	if c := api.next(t); c.tokenRef != "env:B" || c.workspaceID != "ws-b" {
		t.Fatalf("takeover call %+v", c)
	}
}

// TC-P7-17: a removed integration stops its loop; a changed token ref or a
// changed bot behind the same ref restarts it with a fresh offset; a
// failed listing keeps the running loops.
func TestPollerRefreshStopsAndRestarts(t *testing.T) {
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	s := fastSettings()
	s.Refresh = 20 * time.Millisecond
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, s))

	c := api.next(t)
	c.reply <- getReply{updates: []json.RawMessage{upd(40)}}
	c = api.next(t)
	if c.offset != 41 {
		t.Fatalf("offset=%d", c.offset)
	}

	// Listing fails: the loop keeps running (its call is not canceled).
	targets.mu.Lock()
	targets.err = errors.New("db down")
	targets.mu.Unlock()
	targets.waitListed(t, 3)
	select {
	case ref := <-api.canceled:
		t.Fatalf("listing failure stopped loop %s", ref)
	default:
	}
	targets.mu.Lock()
	targets.err = nil
	targets.mu.Unlock()

	// Token ref changes: old loop stops, new loop starts without offset.
	changed := targetA
	changed.TokenRef = "env:A2"
	targets.set(changed)
	if ref := waitCanceled(t, api); ref != "env:A" {
		t.Fatalf("canceled %s", ref)
	}
	c = api.next(t)
	if c.tokenRef != "env:A2" || c.offset != 0 {
		t.Fatalf("restarted call %+v", c)
	}

	// Same ref, other bot (token rotated): restart.
	api.setFingerprint("env:A2", "rotated")
	if ref := waitCanceled(t, api); ref != "env:A2" {
		t.Fatalf("canceled %s", ref)
	}
	c = api.next(t)
	if c.tokenRef != "env:A2" {
		t.Fatalf("after rotation %+v", c)
	}

	// Integration removed: loop stops and nothing restarts it.
	targets.set()
	if ref := waitCanceled(t, api); ref != "env:A2" {
		t.Fatalf("canceled %s", ref)
	}
	targets.waitListed(t, 3)
	api.noCall(t, 60*time.Millisecond)
}

func waitCanceled(t *testing.T, api *fakeUpdatesAPI) string {
	t.Helper()
	select {
	case ref := <-api.canceled:
		return ref
	case <-time.After(waitLong):
		t.Fatal("loop not stopped")
		return ""
	}
}

// TC-P7-18: a target whose token cannot be resolved is not polled and is
// logged once with a safe code; once it resolves it is polled.
func TestPollerUnresolvedTokenSkipped(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	api.mu.Lock()
	api.fpErr["env:A"] = &relay.DeliveryError{Code: "missing_credentials"}
	api.mu.Unlock()
	s := fastSettings()
	s.Refresh = 20 * time.Millisecond
	startPoller(t, NewPoller(targets, api, &fakeHandler{}, s))
	targets.waitListed(t, 4)
	api.noCall(t, 40*time.Millisecond)
	if n := strings.Count(logs.String(), "integration=int-a reason=missing_credentials"); n != 1 {
		t.Fatalf("skip logged %d times: %q", n, logs.String())
	}
	api.mu.Lock()
	delete(api.fpErr, "env:A")
	api.mu.Unlock()
	if c := api.next(t); c.tokenRef != "env:A" {
		t.Fatalf("call %+v", c)
	}
}

// TC-P7-19: Run returns nil after cancel, during a long poll, during a
// backoff wait and during a handler call, and leaves no goroutine behind.
func TestPollerRunStopsWithoutLeaks(t *testing.T) {
	base := runtime.NumGoroutine()
	for _, phase := range []string{"in getUpdates", "in backoff", "in handler"} {
		t.Run(phase, func(t *testing.T) {
			api, targets := newFakeAPI(), newFakeTargets(targetA, targetB)
			s := fastSettings()
			s.MinBackoff, s.MaxBackoff = time.Hour, time.Hour
			inHandler, release := make(chan struct{}, 1), make(chan struct{})
			h := &fakeHandler{result: func(int64) (string, error) {
				inHandler <- struct{}{}
				<-release
				return "", errors.New("stopping")
			}}
			stop := startPoller(t, NewPoller(targets, api, h, s))
			c1, c2 := api.next(t), api.next(t)
			switch phase {
			case "in backoff":
				c1.reply <- getReply{err: &relay.DeliveryError{Code: "server"}}
				c2.reply <- getReply{err: &relay.DeliveryError{Code: "server"}}
				time.Sleep(20 * time.Millisecond) // let both reach the hour-long wait
			case "in handler":
				c1.reply <- getReply{updates: []json.RawMessage{upd(1)}}
				<-inHandler
			}
			done := make(chan struct{})
			go func() { stop(); close(done) }()
			if phase == "in handler" {
				// Run must wait for the loop; the handler returns once released.
				select {
				case <-done:
					t.Fatal("Run returned while a handler call was still running")
				case <-time.After(30 * time.Millisecond):
				}
				close(release)
			}
			select {
			case <-done:
			case <-time.After(waitLong):
				t.Fatal("Run did not return")
			}
			if n := api.inflight.Load(); n != 0 {
				t.Fatalf("%d getUpdates calls still running after Run returned", n)
			}
			api.noCall(t, 30*time.Millisecond)
		})
	}
	deadline := time.Now().Add(waitLong)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutines %d > %d before", n, base)
	}
}

// TC-P7-22: end to end with the real client against a fake Bot API: the
// conflict path, a server error and updates with text; the logs never
// contain the token, the API URL or message text, and the handler gets
// each update once in order.
func TestPollerWithClientLogsNoSecrets(t *testing.T) {
	logs := captureLog(t)
	const secretText = "BÍ-MẬT nội dung"
	var mu sync.Mutex
	var methods []string
	var offsets []any
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		methods = append(methods, method)
		if method == "getUpdates" {
			offsets = append(offsets, body["offset"])
			step++
		}
		n := step
		mu.Unlock()
		switch {
		case method == "deleteWebhook":
			w.Write([]byte(`{"ok":true,"result":true}`))
		case n == 1:
			w.WriteHeader(409)
			w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: webhook is active"}`))
		case n == 2:
			w.WriteHeader(502)
			w.Write([]byte(p7Token))
		case n == 3:
			fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":900,"message":{"message_id":1,"chat":{"id":77},"text":%q}},{"update_id":901,"message":{"message_id":2,"chat":{"id":77},"text":%q}}]}`, secretText, secretText)
		default:
			select {
			case <-r.Context().Done():
			case <-time.After(waitLong):
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
		}
	}))
	defer srv.Close()
	client := NewClient(srv.Client(), &fakeResolver{value: p7Token})
	client.baseURL = srv.URL
	got := make(chan int64, 4)
	h := &fakeHandler{result: func(id int64) (string, error) { got <- id; return "queued", nil }}
	s := fastSettings()
	stop := startPoller(t, NewPoller(newFakeTargets(targetA), client, h, s))
	for _, want := range []int64{900, 901} {
		select {
		case id := <-got:
			if id != want {
				t.Fatalf("handled %d want %d", id, want)
			}
		case <-time.After(waitLong):
			t.Fatal("update not handled")
		}
	}
	// Wait for the 4th getUpdates (it confirms 900 and 901).
	deadline := time.Now().Add(waitLong)
	for {
		mu.Lock()
		n := len(offsets)
		mu.Unlock()
		if n >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(offsets) < 4 || offsets[0] != nil || offsets[3] != float64(902) {
		t.Fatalf("offsets %v", offsets)
	}
	deletes := 0
	for _, m := range methods {
		if m == "deleteWebhook" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("deleteWebhook calls %d (methods %v)", deletes, methods)
	}
	out := logs.String()
	if !strings.Contains(out, "telegram: webhook removed for polling") || !strings.Contains(out, "code=server") {
		t.Fatalf("expected log lines missing: %q", out)
	}
	for _, leak := range []string{p7Token, "P7-SECRET", srv.URL, "/bot", "BÍ-MẬT", "nội dung", "Conflict:"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log contains %q: %q", leak, out)
		}
	}
}

// panicValue is a panic value that carries text and a token.
func panicValue(id int64) string {
	return fmt.Sprintf("boom on secret text %d with %s", id, p7Token)
}

func assertNoPanicLeak(t *testing.T, out string) {
	t.Helper()
	for _, leak := range []string{"boom", "secret text", p7Token, "P7-SECRET", "goroutine "} {
		if strings.Contains(out, leak) {
			t.Fatalf("log contains %q: %q", leak, out)
		}
	}
}

// TC-P7-23 (R10): a panicking handler does not crash the poller; the panic
// is logged as handler_panic with ids only, the offset stays and the
// update is offered again after a backoff.
func TestPollerHandlerPanicRecovered(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	var panicked atomic.Bool
	h := &fakeHandler{result: func(id int64) (string, error) {
		if id == 60 && panicked.CompareAndSwap(false, true) {
			panic(panicValue(id))
		}
		return "queued", nil
	}}
	s := fastSettings()
	s.MinBackoff, s.MaxBackoff = 50*time.Millisecond, 50*time.Millisecond
	startPoller(t, NewPoller(targets, api, h, s))

	c := api.next(t)
	sent := time.Now()
	c.reply <- getReply{updates: []json.RawMessage{upd(60), upd(61)}}
	c = api.next(t)
	if c.offset != 0 {
		t.Fatalf("offset moved to %d after a panic", c.offset)
	}
	if gap := c.at.Sub(sent); gap < 50*time.Millisecond {
		t.Fatalf("no backoff after a panic: gap %v", gap)
	}
	if got := h.ids(); fmt.Sprint(got) != "[60]" {
		t.Fatalf("handled %v: the batch must stop at the panicking update", got)
	}
	out := logs.String()
	if !strings.Contains(out, "update deferred integration=int-a update=60 code=handler_panic") {
		t.Fatalf("panic not logged: %q", out)
	}
	assertNoPanicLeak(t, out)

	c.reply <- getReply{updates: []json.RawMessage{upd(60), upd(61)}}
	c = api.next(t)
	if c.offset != 62 {
		t.Fatalf("offset=%d want 62 after recovery", c.offset)
	}
	if got := h.ids(); fmt.Sprint(got) != "[60 60 61]" {
		t.Fatalf("handled %v", got)
	}
	c.reply <- getReply{}
}

// TC-P7-24 (R10): the third panic in a row on one update_id skips it with
// update_skipped_after_panics, and the next update is handled.
func TestPollerSkipsUpdateAfterThreePanics(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	h := &fakeHandler{result: func(id int64) (string, error) {
		if id == 70 {
			panic(panicValue(id))
		}
		return "queued", nil
	}}
	startPoller(t, NewPoller(targets, api, h, fastSettings()))

	c := api.next(t)
	for i := 1; i <= 2; i++ {
		c.reply <- getReply{updates: []json.RawMessage{upd(70), upd(71)}}
		c = api.next(t)
		if c.offset != 0 {
			t.Fatalf("panic %d: offset=%d want 0", i, c.offset)
		}
	}
	if strings.Contains(logs.String(), "update_skipped_after_panics") {
		t.Fatalf("skipped before the third panic: %q", logs.String())
	}
	c.reply <- getReply{updates: []json.RawMessage{upd(70), upd(71)}}
	c = api.next(t)
	if c.offset != 72 {
		t.Fatalf("offset=%d want 72 (70 skipped, 71 handled)", c.offset)
	}
	if got := h.ids(); fmt.Sprint(got) != "[70 70 70 71]" {
		t.Fatalf("handled %v", got)
	}
	out := logs.String()
	if strings.Count(out, "code=handler_panic") != 3 || strings.Count(out, "update_skipped_after_panics integration=int-a update=70") != 1 {
		t.Fatalf("logs %q", out)
	}
	assertNoPanicLeak(t, out)
	c.reply <- getReply{}
}

// TC-P7-25 (R10): the panic count is per update_id in a row: a panic on
// another update resets it, so the first update needs three new panics
// before it is skipped.
func TestPollerPanicCountResetsOnOtherUpdate(t *testing.T) {
	logs := captureLog(t)
	api, targets := newFakeAPI(), newFakeTargets(targetA)
	h := &fakeHandler{result: func(id int64) (string, error) {
		if id == 80 || id == 81 {
			panic(panicValue(id))
		}
		return "queued", nil
	}}
	startPoller(t, NewPoller(targets, api, h, fastSettings()))

	c := api.next(t)
	for i, batch := range [][]json.RawMessage{
		{upd(80)}, {upd(80)}, // 80: 2 panics
		{upd(81)},            // 81: 1 panic, resets the count
		{upd(80)}, {upd(80)}, // 80: 2 panics again, not yet skipped
	} {
		c.reply <- getReply{updates: batch}
		c = api.next(t)
		if c.offset != 0 {
			t.Fatalf("batch %d: offset=%d want 0 (nothing skipped yet)", i, c.offset)
		}
	}
	if strings.Contains(logs.String(), "update_skipped_after_panics") {
		t.Fatalf("count not reset by the other update: %q", logs.String())
	}
	c.reply <- getReply{updates: []json.RawMessage{upd(80)}}
	c = api.next(t)
	if c.offset != 81 {
		t.Fatalf("offset=%d want 81 after the third panic in a row", c.offset)
	}
	if n := strings.Count(logs.String(), "update_skipped_after_panics integration=int-a update=80"); n != 1 {
		t.Fatalf("skip logged %d times: %q", n, logs.String())
	}
	assertNoPanicLeak(t, logs.String())
	c.reply <- getReply{}
}
