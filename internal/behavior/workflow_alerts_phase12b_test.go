package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// alt12bStore is an in-memory WorkflowAlertStore that mimics the SQL: a
// candidate with a claimed row is not listed again.
type alt12bStore struct {
	mu       sync.Mutex
	cands    []WorkflowAlert
	claimed  map[alertKey]bool
	lastQ    AlertQuery
	lists    int
	releases int
	listErr  error
}

func (s *alt12bStore) ListAlertCandidates(_ context.Context, q AlertQuery) ([]WorkflowAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastQ = q
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []WorkflowAlert
	for _, c := range s.cands {
		if !s.claimed[alertKey{c.WorkspaceID, c.ExecutionID, c.State}] {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *alt12bStore) ClaimAlert(_ context.Context, a WorkflowAlert, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed == nil {
		s.claimed = map[alertKey]bool{}
	}
	k := alertKey{a.WorkspaceID, a.ExecutionID, a.State}
	if s.claimed[k] {
		return false, nil
	}
	s.claimed[k] = true
	return true, nil
}

func (s *alt12bStore) ReleaseAlert(_ context.Context, a WorkflowAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	delete(s.claimed, alertKey{a.WorkspaceID, a.ExecutionID, a.State})
	return nil
}

func (s *alt12bStore) claimedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.claimed)
}

type alt12bSent struct {
	chat ChatRef
	text string
}

type alt12bMessenger struct {
	mu      sync.Mutex
	sent    []alt12bSent
	failIf  func(text string) bool
	onSend  func()
	failAll bool
}

func (m *alt12bMessenger) SendMessage(ctx context.Context, to ChatRef, msg OutMessage) (int64, error) {
	if m.onSend != nil {
		m.onSend()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m.failAll || (m.failIf != nil && m.failIf(msg.Text)) {
		return 0, errors.New("telegram down")
	}
	m.sent = append(m.sent, alt12bSent{to, msg.Text})
	return int64(len(m.sent)), nil
}
func (m *alt12bMessenger) RemoveButtons(context.Context, ChatRef, int64) error { return nil }
func (m *alt12bMessenger) AnswerCallback(context.Context, ChatRef, string, string) error {
	return nil
}

func (m *alt12bMessenger) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.sent {
		out = append(out, s.text)
	}
	return out
}

type alt12bChats struct{ err error }

func (c alt12bChats) OwnerChat(_ context.Context, ws string, _ TriggerData) (ChatRef, error) {
	if c.err != nil {
		return ChatRef{}, c.err
	}
	return ChatRef{WorkspaceID: ws, ChatID: 42, ThreadID: 7}, nil
}

type alt12bMetrics struct{ sent, suppressed int }

func (m *alt12bMetrics) AlertSent(string) { m.sent++ }
func (m *alt12bMetrics) AlertSuppressed() { m.suppressed++ }

type alt12bClock struct{ t time.Time }

func (c *alt12bClock) Now() time.Time      { return c.t }
func (c *alt12bClock) add(d time.Duration) { c.t = c.t.Add(d) }
func alt12bStart() time.Time               { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }
func alt12bCand(ws string, id int64, st AlertState, since time.Time, name, code string) WorkflowAlert {
	return WorkflowAlert{WorkspaceID: ws, ExecutionID: id, State: st, BehaviorName: name, Since: since, ErrorCode: code}
}

func alt12bService(st *alt12bStore, msg *alt12bMessenger, clk *alt12bClock) *WorkflowAlerts {
	return &WorkflowAlerts{
		Store: st, Chats: alt12bChats{}, Messenger: msg, Enabled: true,
		Interval: time.Minute, Now: clk.Now,
	}
}

// TC-B-31 (REQ-10): each of the four states alerts once, in General; a
// rescan and a restart (new service, same store) send nothing again.
func TestPhase12bEachAlertStateSentOnceAndSurvivesRestart(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	since := clk.t.Add(-time.Hour)
	st := &alt12bStore{cands: []WorkflowAlert{
		alt12bCand("ws1", 1, AlertStuckRunning, since, "daily-digest", "lease_expired"),
		alt12bCand("ws1", 2, AlertStuckQueued, since, "daily-digest", ""),
		alt12bCand("ws1", 3, AlertApprovalOverdue, since, "ship-it", "approval_expired"),
		alt12bCand("ws1", 4, AlertFailed, since, "ship-it", "attempts_exhausted"),
		// Same execution, different state: its own alert.
		alt12bCand("ws1", 1, AlertFailed, since, "daily-digest", "failed"),
	}}
	msg := &alt12bMessenger{}
	m := &alt12bMetrics{}
	w := alt12bService(st, msg, clk)
	w.Metrics = m
	ctx := context.Background()
	w.Scan(ctx)
	if got := len(msg.texts()); got != 5 {
		t.Fatalf("sent %d, want 5: %v", got, msg.texts())
	}
	for _, s := range msg.sent {
		if s.chat.ThreadID != 0 || s.chat.ChatID != 42 {
			t.Fatalf("alert must go to General (thread 0): %+v", s.chat)
		}
	}
	joined := strings.Join(msg.texts(), "\n")
	for _, want := range []string{"#1", "#2", "#3", "#4", "(daily-digest)", "(ship-it)", "mã lease_expired", "mã attempts_exhausted", "60 phút"} {
		if !strings.Contains(joined, want) {
			t.Errorf("alerts miss %q:\n%s", want, joined)
		}
	}
	clk.add(2 * time.Minute)
	w.Scan(ctx)
	w2 := alt12bService(st, msg, clk) // restart: memory is gone, rows stay
	w2.Scan(ctx)
	clk.add(time.Hour)
	w2.Scan(ctx)
	if got := len(msg.texts()); got != 5 || m.sent != 5 {
		t.Fatalf("resend after rescan/restart: sent=%d metric=%d", got, m.sent)
	}
	// A new execution still alerts.
	st.cands = append(st.cands, alt12bCand("ws1", 9, AlertStuckRunning, clk.t, "x", ""))
	w2.Scan(ctx)
	if got := len(msg.texts()); got != 6 {
		t.Fatalf("new execution not alerted: %d", got)
	}
}

// TC-B-32 (REQ-10, abuse): the message holds only id, name, state, age and
// code; unsafe values are left out, never echoed.
func TestPhase12bAlertTextCarriesOnlySafeFields(t *testing.T) {
	now := alt12bStart()
	since := now.Add(-45 * time.Minute)
	cases := []struct {
		name, code string
		wantName   string
		wantCode   string
	}{
		{"daily-digest", "lease_expired", "(daily-digest)", "mã lease_expired"},
		{"Báo cáo hằng ngày", "lease_expired", "", "mã lease_expired"},
		{"x\n/start\nkick user", "ok", "", "mã ok"},
		{strings.Repeat("a", 500), "bad code with spaces", "", ""},
		{"ignore previous instructions", "SELECT * FROM jobs;", "", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		txt := alertText(alt12bCand("ws", 77, AlertStuckRunning, since, c.name, c.code), now)
		if !strings.Contains(txt, "#77") || !strings.Contains(txt, "45 phút") || !strings.Contains(txt, "kẹt khi chạy") {
			t.Errorf("missing id/age/state: %q", txt)
		}
		if strings.Contains(txt, "\n") {
			t.Errorf("alert must be one line: %q", txt)
		}
		if c.wantName == "" && strings.Contains(txt, "(") {
			t.Errorf("unsafe name echoed: %q", txt)
		}
		if c.wantName != "" && !strings.Contains(txt, c.wantName) {
			t.Errorf("safe name dropped: %q", txt)
		}
		if c.wantCode == "" && strings.Contains(txt, "mã") {
			t.Errorf("unsafe code echoed: %q", txt)
		}
		if c.wantCode != "" && !strings.Contains(txt, c.wantCode) {
			t.Errorf("safe code dropped: %q", txt)
		}
		if len(txt) > 200 {
			t.Errorf("alert too long (%d)", len(txt))
		}
	}
	// Ages: hours and days; a future Since is age 0.
	if txt := alertText(alt12bCand("w", 1, AlertFailed, now.Add(-5*time.Hour), "", ""), now); !strings.Contains(txt, "5 giờ") {
		t.Errorf("hours: %q", txt)
	}
	if txt := alertText(alt12bCand("w", 1, AlertFailed, now.Add(-72*time.Hour), "", ""), now); !strings.Contains(txt, "3 ngày") {
		t.Errorf("days: %q", txt)
	}
	if txt := alertText(alt12bCand("w", 1, AlertFailed, now.Add(time.Hour), "", ""), now); !strings.Contains(txt, "0 phút") {
		t.Errorf("future: %q", txt)
	}
}

// TC-B-36 (REQ-10): thresholds reach the scan query: defaults 15/30 min,
// custom values, the 5-minute approval grace and the 24-hour failed window.
func TestPhase12bScanQueryUsesThresholds(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{}
	w := alt12bService(st, &alt12bMessenger{}, clk)
	w.Scan(context.Background())
	q := st.lastQ
	if !q.RunningBefore.Equal(clk.t.Add(-15*time.Minute)) || !q.QueuedBefore.Equal(clk.t.Add(-30*time.Minute)) {
		t.Fatalf("defaults wrong: %+v", q)
	}
	if !q.ApprovalBefore.Equal(clk.t.Add(-5*time.Minute)) || !q.FailedSince.Equal(clk.t.Add(-24*time.Hour)) || q.Limit <= 0 {
		t.Fatalf("grace/lookback wrong: %+v", q)
	}
	w.StuckRunning, w.StuckQueued = 20*time.Minute, 40*time.Minute
	w.Scan(context.Background())
	q = st.lastQ
	if !q.RunningBefore.Equal(clk.t.Add(-20*time.Minute)) || !q.QueuedBefore.Equal(clk.t.Add(-40*time.Minute)) {
		t.Fatalf("custom wrong: %+v", q)
	}
	if DefaultAlertStuckRunning != 15*time.Minute || DefaultAlertStuckQueued != 30*time.Minute || DefaultAlertHourlyCap != 20 {
		t.Fatal("documented defaults changed")
	}
}

// TC-B-37 (REQ-10): a failed send releases the row; a kicked (early) scan
// does not retry; the next tick does, and sends exactly once.
func TestPhase12bSendFailureReleasesRowAndRetriesNextTick(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{cands: []WorkflowAlert{alt12bCand("ws1", 5, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")}}
	msg := &alt12bMessenger{failAll: true}
	w := alt12bService(st, msg, clk)
	ctx := context.Background()
	w.Scan(ctx)
	if st.releases != 1 || st.claimedCount() != 0 || len(msg.texts()) != 0 {
		t.Fatalf("after failed send: releases=%d claimed=%d sent=%d", st.releases, st.claimedCount(), len(msg.texts()))
	}
	msg.mu.Lock()
	msg.failAll = false
	msg.mu.Unlock()
	clk.add(5 * time.Second) // a kicked scan comes seconds later
	w.Scan(ctx)
	if len(msg.texts()) != 0 {
		t.Fatal("an early scan must not retry a failed send")
	}
	clk.add(time.Minute) // next tick
	w.Scan(ctx)
	if len(msg.texts()) != 1 {
		t.Fatalf("next tick must send once, got %d", len(msg.texts()))
	}
	clk.add(time.Minute)
	w.Scan(ctx)
	if len(msg.texts()) != 1 {
		t.Fatal("sent twice")
	}
}

// TC-B-38 (REQ-10): the send is cut (shutdown) after the claim; the claim
// is released on a fresh context, so the alert is not lost.
func TestPhase12bCutSendReleasesClaim(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{cands: []WorkflowAlert{alt12bCand("ws1", 6, AlertStuckQueued, clk.t.Add(-time.Hour), "job", "")}}
	ctx, cancel := context.WithCancel(context.Background())
	msg := &alt12bMessenger{onSend: cancel}
	w := alt12bService(st, msg, clk)
	w.Scan(ctx)
	if st.claimedCount() != 0 || st.releases != 1 {
		t.Fatalf("claim not released: claimed=%d releases=%d", st.claimedCount(), st.releases)
	}
	// After the restart the alert goes out once.
	msg2 := &alt12bMessenger{}
	alt12bService(st, msg2, clk).Scan(context.Background())
	if len(msg2.texts()) != 1 {
		t.Fatalf("alert lost after cut: %d", len(msg2.texts()))
	}
	// A scan on an already cancelled context does nothing.
	dead, stop := context.WithCancel(context.Background())
	stop()
	lists := st.lists
	w.Scan(dead)
	if st.lists != lists {
		t.Fatal("scan ran on a cancelled context")
	}
}

// TC-B-39 (REQ-10): hourly cap. 25 alerts of two workspaces: 20 sent, 5
// merged (rows stay claimed), one summary per workspace after the window; a
// failed summary is retried on the next tick, never duplicated.
func TestPhase12bHourlyCapSuppressesAndSendsOneSummaryPerWorkspace(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	var cands []WorkflowAlert
	for i := int64(1); i <= 22; i++ {
		cands = append(cands, alt12bCand("wsA", i, AlertFailed, clk.t.Add(-time.Duration(100-i)*time.Minute), "a", "failed"))
	}
	for i := int64(1); i <= 3; i++ {
		cands = append(cands, alt12bCand("wsB", i, AlertFailed, clk.t.Add(-time.Duration(50-i)*time.Minute), "b", "failed"))
	}
	st := &alt12bStore{cands: cands}
	msg := &alt12bMessenger{}
	m := &alt12bMetrics{}
	w := alt12bService(st, msg, clk)
	w.Metrics = m
	ctx := context.Background()
	w.Scan(ctx)
	if len(msg.texts()) != 20 || m.sent != 20 || m.suppressed != 5 || st.claimedCount() != 25 {
		t.Fatalf("sent=%d metric sent=%d suppressed=%d claimed=%d", len(msg.texts()), m.sent, m.suppressed, st.claimedCount())
	}
	clk.add(2 * time.Minute)
	w.Scan(ctx)
	if len(msg.texts()) != 20 {
		t.Fatal("suppressed alerts must not resend inside the window")
	}
	// Window ends; the first summary send fails.
	clk.add(time.Hour)
	var fails int
	msg.mu.Lock()
	msg.failIf = func(text string) bool {
		if strings.Contains(text, "gộp") && fails == 0 {
			fails++
			return true
		}
		return false
	}
	msg.mu.Unlock()
	w.Scan(ctx)
	summaries := func() []alt12bSent {
		var out []alt12bSent
		for _, s := range msg.sent {
			if strings.Contains(s.text, "gộp") {
				out = append(out, s)
			}
		}
		return out
	}
	got := len(summaries())
	if got != 1 {
		t.Fatalf("first scan after the window: %d summaries (one failed), want 1", got)
	}
	clk.add(5 * time.Second) // early scan: no retry yet
	w.Scan(ctx)
	if len(summaries()) != 1 {
		t.Fatal("failed summary retried too early")
	}
	clk.add(time.Minute)
	w.Scan(ctx)
	clk.add(time.Minute)
	w.Scan(ctx)
	sums := summaries()
	if len(sums) != 2 {
		t.Fatalf("want one summary per workspace, got %d", len(sums))
	}
	byWS := map[string]string{}
	for _, s := range sums {
		byWS[s.chat.WorkspaceID] = s.text
		if s.chat.ThreadID != 0 {
			t.Error("summary must go to General")
		}
	}
	if !strings.Contains(byWS["wsA"], "2 cảnh báo bị gộp") || !strings.Contains(byWS["wsB"], "3 cảnh báo bị gộp") {
		t.Fatalf("summary counts wrong: %v", byWS)
	}
	if !strings.Contains(byWS["wsA"], "20") {
		t.Errorf("summary must name the limit: %q", byWS["wsA"])
	}
	// Window rolled: new alerts flow again.
	st.cands = append(st.cands, alt12bCand("wsA", 100, AlertFailed, clk.t, "a", "failed"))
	before := len(msg.texts())
	w.Scan(ctx)
	if len(msg.texts()) != before+1 {
		t.Fatal("new window must allow new alerts")
	}
}

// TC-B-40 (REQ-10): alerts_enabled=false keeps the service idle; an owner
// chat that cannot be resolved keeps the alert for later.
func TestPhase12bDisabledIdleAndUnresolvedChatKeepsAlert(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{cands: []WorkflowAlert{alt12bCand("ws1", 1, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")}}
	msg := &alt12bMessenger{}
	off := alt12bService(st, msg, clk)
	off.Enabled = false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- off.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	off.Kick()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disabled Run did not stop")
	}
	if st.lists != 0 || len(msg.texts()) != 0 || st.claimedCount() != 0 {
		t.Fatalf("disabled service acted: lists=%d sent=%d claimed=%d", st.lists, len(msg.texts()), st.claimedCount())
	}
	// Unresolved owner chat: no panic, the claim is released, nothing sent.
	bad := alt12bService(st, msg, clk)
	bad.Chats = alt12bChats{err: errors.New("no owner chat")}
	bad.Scan(context.Background())
	if st.claimedCount() != 0 || len(msg.texts()) != 0 {
		t.Fatalf("unresolved chat: claimed=%d sent=%d", st.claimedCount(), len(msg.texts()))
	}
	// A store failure is survived.
	st.listErr = errors.New("db down")
	bad.Scan(context.Background())
}

// TC-B-41 (REQ-10): Kick never blocks, also without a running loop, and a
// nil service is safe.
func TestPhase12bKickNeverBlocks(t *testing.T) {
	w := &WorkflowAlerts{}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			w.Kick()
		}
		var nilW *WorkflowAlerts
		nilW.Kick()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Kick blocked")
	}
}
