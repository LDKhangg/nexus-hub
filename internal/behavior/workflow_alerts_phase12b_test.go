package behavior

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type alt12bKey struct {
	ws    string
	id    int64
	state AlertState
}

type alt12bRow struct {
	alert      WorkflowAlert
	status     string
	attempts   int
	next       time.Time
	sent       time.Time
	created    time.Time
	code       string
	summarised time.Time
	hasSummary bool
}

// alt12bStore is an in-memory WorkflowAlertStore that mimics the SQL of
// migration 00040: one row per (execution, state) for the alert's whole
// life, whatever its status; the cap and the summaries read the rows.
type alt12bStore struct {
	mu       sync.Mutex
	cands    []WorkflowAlert
	rows     map[alt12bKey]*alt12bRow
	obsolete map[alt12bKey]bool // conditions that ended (stuck, overdue)
	lastQ    AlertQuery
	lists    int
	claims   int
	listErr  error
}

func alt12bK(a WorkflowAlert) alt12bKey { return alt12bKey{a.WorkspaceID, a.ExecutionID, a.State} }

func (s *alt12bStore) ensure() {
	if s.rows == nil {
		s.rows = map[alt12bKey]*alt12bRow{}
	}
}

// seed inserts a row as the database would hold it.
func (s *alt12bStore) seed(a WorkflowAlert, status string, attempts int, next, sent, created time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	s.rows[alt12bK(a)] = &alt12bRow{alert: a, status: status, attempts: attempts, next: next, sent: sent, created: created}
}

func (s *alt12bStore) row(a WorkflowAlert) alt12bRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[alt12bK(a)]; r != nil {
		return *r
	}
	return alt12bRow{}
}

func (s *alt12bStore) rowCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
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
		if s.rows[alt12bK(c)] == nil { // a row of any status hides it
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].ExecutionID < out[j].ExecutionID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *alt12bStore) insert(a WorkflowAlert, status string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	if s.rows[alt12bK(a)] != nil {
		return false
	}
	r := &alt12bRow{alert: a, status: status, created: at}
	if status == "pending" {
		r.next = at.Add(AlertClaimLease)
	}
	s.rows[alt12bK(a)] = r
	return true
}

func (s *alt12bStore) ClaimAlert(_ context.Context, a WorkflowAlert, at time.Time) (bool, error) {
	s.mu.Lock()
	s.claims++
	s.mu.Unlock()
	return s.insert(a, "pending", at), nil
}

func (s *alt12bStore) SuppressAlert(_ context.Context, a WorkflowAlert, at time.Time) (bool, error) {
	return s.insert(a, "suppressed", at), nil
}

func (s *alt12bStore) MarkAlertSent(_ context.Context, a WorkflowAlert, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[alt12bK(a)]; r != nil && r.status == "pending" {
		r.status, r.sent, r.next, r.code = "sent", at, time.Time{}, ""
		r.attempts++
	}
	return nil
}

func (s *alt12bStore) MarkAlertAttemptFailed(_ context.Context, a WorkflowAlert, code string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[alt12bK(a)]
	if r == nil || r.status != "pending" || r.attempts != a.Attempts {
		return nil
	}
	r.attempts++
	r.code = code
	r.next = next
	if next.IsZero() {
		r.status = "failed"
	}
	return nil
}

func (s *alt12bStore) MarkAlertObsolete(_ context.Context, a WorkflowAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[alt12bK(a)]; r != nil && r.status == "pending" && a.State != AlertFailed {
		r.status, r.next = "obsolete", time.Time{}
	}
	return nil
}

func (s *alt12bStore) ListAlertRetries(_ context.Context, due time.Time, limit int) ([]WorkflowAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []*alt12bRow
	for _, r := range s.rows {
		if r.status == "pending" && !r.next.After(due) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].next.Equal(rows[j].next) {
			return rows[i].next.Before(rows[j].next)
		}
		return rows[i].alert.ExecutionID < rows[j].alert.ExecutionID
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]WorkflowAlert, 0, len(rows))
	for _, r := range rows {
		a := r.alert
		a.Attempts = r.attempts
		a.Obsolete = a.State != AlertFailed && s.obsolete[alt12bK(a)]
		out = append(out, a)
	}
	return out, nil
}

func (s *alt12bStore) CountAlertsSentSince(_ context.Context, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if r.status == "sent" && !r.sent.Before(since) {
			n++
		}
	}
	return n, nil
}

func (s *alt12bStore) ListAlertSummaries(_ context.Context, before time.Time) ([]AlertSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	by := map[string]*AlertSummary{}
	for _, r := range s.rows {
		if r.status != "suppressed" || r.hasSummary || !r.created.Before(before) {
			continue
		}
		sm := by[r.alert.WorkspaceID]
		if sm == nil {
			sm = &AlertSummary{WorkspaceID: r.alert.WorkspaceID, OldestAt: r.created}
			by[r.alert.WorkspaceID] = sm
		}
		sm.Count++
		if r.created.Before(sm.OldestAt) {
			sm.OldestAt = r.created
		}
	}
	var out []AlertSummary
	for _, sm := range by {
		out = append(out, *sm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkspaceID < out[j].WorkspaceID })
	return out, nil
}

func (s *alt12bStore) MarkAlertsSummarised(_ context.Context, ws string, before, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.alert.WorkspaceID == ws && r.status == "suppressed" && !r.hasSummary && r.created.Before(before) {
			r.hasSummary, r.summarised = true, at
		}
	}
	return nil
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
	block   bool // wait for the send context (a hung Telegram)
	calls   int
}

func (m *alt12bMessenger) SendMessage(ctx context.Context, to ChatRef, msg OutMessage) (int64, error) {
	if m.onSend != nil {
		m.onSend()
	}
	m.mu.Lock()
	m.calls++
	block := m.block
	m.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
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

type alt12bMetrics struct{ sent, suppressed, failed int }

func (m *alt12bMetrics) AlertFailed() { m.failed++ }

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

// TC-B-37 (REQ-10, R-B-1): a failed send keeps the row pending with one
// attempt and a backoff; a kicked (early) scan does not retry; the scan at
// the due time does, and sends exactly once.
func TestPhase12bSendFailureKeepsRowAndRetriesWhenDue(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	cand := alt12bCand("ws1", 5, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")
	st := &alt12bStore{cands: []WorkflowAlert{cand}}
	msg := &alt12bMessenger{failAll: true}
	w := alt12bService(st, msg, clk)
	ctx := context.Background()
	t0 := clk.t
	w.Scan(ctx)
	r := st.row(cand)
	if r.status != "pending" || r.attempts != 1 || r.code != "send_failed" || !r.next.Equal(t0.Add(time.Minute)) || len(msg.texts()) != 0 {
		t.Fatalf("after failed send: %+v sent=%d", r, len(msg.texts()))
	}
	msg.mu.Lock()
	msg.failAll = false
	msg.mu.Unlock()
	clk.add(5 * time.Second) // a kicked scan comes seconds later
	w.Scan(ctx)
	if len(msg.texts()) != 0 {
		t.Fatal("an early scan must not retry a failed send")
	}
	clk.add(time.Minute) // due
	w.Scan(ctx)
	if len(msg.texts()) != 1 {
		t.Fatalf("the due scan must send once, got %d", len(msg.texts()))
	}
	if r := st.row(cand); r.status != "sent" || !r.sent.Equal(clk.t) {
		t.Fatalf("after the retry: %+v", r)
	}
	clk.add(time.Hour)
	w.Scan(ctx)
	if len(msg.texts()) != 1 {
		t.Fatal("sent twice")
	}
}

// TC-B-38 (REQ-10, R-B-1): the send is cut (shutdown) after the claim; the
// row stays pending and is due after the claim lease, so a restart sends
// it once and not earlier.
func TestPhase12bCutSendIsRetriedAfterTheLease(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	cand := alt12bCand("ws1", 6, AlertStuckQueued, clk.t.Add(-time.Hour), "job", "")
	st := &alt12bStore{cands: []WorkflowAlert{cand}}
	ctx, cancel := context.WithCancel(context.Background())
	msg := &alt12bMessenger{onSend: cancel}
	w := alt12bService(st, msg, clk)
	t0 := clk.t
	w.Scan(ctx)
	if r := st.row(cand); r.status != "pending" || r.attempts != 0 || !r.next.Equal(t0.Add(AlertClaimLease)) {
		t.Fatalf("cut send must leave a pending claim: %+v", r)
	}
	// Restart before the lease ended: not yet.
	msg2 := &alt12bMessenger{}
	clk.add(AlertClaimLease - time.Second)
	alt12bService(st, msg2, clk).Scan(context.Background())
	if len(msg2.texts()) != 0 {
		t.Fatal("retried before the claim lease ended")
	}
	clk.add(time.Second)
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

// TC-B-39 (REQ-10, R-B-4): hourly cap. 25 alerts of two workspaces: 20
// sent, 5 suppressed rows, one summary per workspace once the oldest merged
// alert is an hour old, counted from the rows; a failed summary leaves the
// rows unsummarised and is retried, never duplicated.
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
	if len(msg.texts()) != 20 || m.sent != 20 || m.suppressed != 5 || st.rowCount() != 25 {
		t.Fatalf("sent=%d metric sent=%d suppressed=%d rows=%d", len(msg.texts()), m.sent, m.suppressed, st.rowCount())
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
	summaries := func() []alt12bSent {
		var out []alt12bSent
		for _, s := range msg.sent {
			if strings.Contains(s.text, "gộp") {
				out = append(out, s)
			}
		}
		return out
	}
	w.Scan(ctx)
	if got := len(summaries()); got != 0 {
		t.Fatalf("the first summary failed, %d sent", got)
	}
	if lst, _ := st.ListAlertSummaries(ctx, clk.t); len(lst) != 2 {
		t.Fatalf("a failed summary must keep both workspaces unsummarised: %+v", lst)
	}
	clk.add(5 * time.Second) // early scan: backoff
	w.Scan(ctx)
	if len(summaries()) != 0 {
		t.Fatal("failed summary retried too early")
	}
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
	for _, r := range st.rows {
		if r.status == "suppressed" && !r.hasSummary {
			t.Fatalf("suppressed row left unsummarised: %+v", r)
		}
	}
	// Later scans and a restart send no second summary.
	clk.add(time.Minute)
	w.Scan(ctx)
	alt12bService(st, msg, clk).Scan(ctx)
	if len(summaries()) != 2 {
		t.Fatalf("summary repeated: %d", len(summaries()))
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
// chat that cannot be resolved counts as a failed attempt, not a loss.
func TestPhase12bDisabledIdleAndUnresolvedChatKeepsAlert(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	cand := alt12bCand("ws1", 1, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")
	st := &alt12bStore{cands: []WorkflowAlert{cand}}
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
	if st.lists != 0 || len(msg.texts()) != 0 || st.rowCount() != 0 {
		t.Fatalf("disabled service acted: lists=%d sent=%d rows=%d", st.lists, len(msg.texts()), st.rowCount())
	}
	// Unresolved owner chat: no panic, nothing sent, one attempt recorded.
	bad := alt12bService(st, msg, clk)
	bad.Chats = alt12bChats{err: errors.New("no owner chat")}
	bad.Scan(context.Background())
	if r := st.row(cand); r.status != "pending" || r.attempts != 1 || r.code != "chat_unavailable" || len(msg.texts()) != 0 || msg.calls != 0 {
		t.Fatalf("unresolved chat: %+v sent=%d calls=%d", r, len(msg.texts()), msg.calls)
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

// R-B-1 (fix B): five failed attempts follow the 1, 5, 15, 60 minute
// backoff, then the alert is failed for good: no sixth send, one metric.
func TestPhase12bFixBAttemptsBackoffThenFailedForGood(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	cand := alt12bCand("ws1", 7, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")
	st := &alt12bStore{cands: []WorkflowAlert{cand}}
	msg := &alt12bMessenger{failAll: true}
	m := &alt12bMetrics{}
	w := alt12bService(st, msg, clk)
	w.Metrics = m
	ctx := context.Background()
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	for i := 0; i < AlertMaxAttempts; i++ {
		w.Scan(ctx)
		r := st.row(cand)
		if r.attempts != i+1 || msg.calls != i+1 {
			t.Fatalf("scan %d: attempts=%d calls=%d", i+1, r.attempts, msg.calls)
		}
		if i < len(want) {
			if r.status != "pending" || !r.next.Equal(clk.t.Add(want[i])) {
				t.Fatalf("after attempt %d: %+v, want pending next +%v", i+1, r, want[i])
			}
			// Not due yet one second earlier.
			clk.t = r.next.Add(-time.Second)
			w.Scan(ctx)
			if msg.calls != i+1 {
				t.Fatalf("retried before it was due (attempt %d)", i+1)
			}
			clk.t = r.next
		} else if r.status != "failed" || !r.next.IsZero() {
			t.Fatalf("after the last attempt: %+v", r)
		}
	}
	if m.failed != 1 || m.sent != 0 {
		t.Fatalf("metrics: failed=%d sent=%d", m.failed, m.sent)
	}
	for i := 0; i < 5; i++ {
		clk.add(100 * time.Hour)
		w.Scan(ctx)
	}
	if msg.calls != AlertMaxAttempts || len(msg.texts()) != 0 {
		t.Fatalf("a failed alert was sent again: calls=%d", msg.calls)
	}
}

// R-B-1 (fix B): a send that hangs until its timeout counts as an attempt.
func TestPhase12bFixBTimeoutCountsAsAttempt(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	cand := alt12bCand("ws1", 8, AlertStuckRunning, clk.t.Add(-time.Hour), "job", "")
	st := &alt12bStore{cands: []WorkflowAlert{cand}}
	msg := &alt12bMessenger{block: true}
	w := alt12bService(st, msg, clk)
	w.SendTimeout = 20 * time.Millisecond
	w.Scan(context.Background())
	r := st.row(cand)
	if r.status != "pending" || r.attempts != 1 || r.code != "send_timeout" || !r.next.Equal(clk.t.Add(time.Minute)) {
		t.Fatalf("a timed-out send: %+v", r)
	}
}

// R-B-1 (fix B): a row of any status hides its candidate: nothing is
// claimed or sent for it, while a new execution still alerts.
func TestPhase12bFixBExistingRowOfAnyStatusSkipsCandidate(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{}
	var cands []WorkflowAlert
	for i, status := range []string{"pending", "sent", "failed", "suppressed", "obsolete"} {
		c := alt12bCand("ws1", int64(i+1), AlertFailed, clk.t.Add(-time.Hour), "job", "failed")
		cands = append(cands, c)
		next, sent := time.Time{}, time.Time{}
		if status == "pending" {
			next = clk.t.Add(time.Hour) // not due: a candidate scan must still skip it
		}
		if status == "sent" {
			sent = clk.t.Add(-2 * time.Hour)
		}
		created := clk.t.Add(-2 * time.Hour)
		if status == "suppressed" {
			created = clk.t.Add(-10 * time.Minute) // inside the hour: no summary is due yet
		}
		st.seed(c, status, 1, next, sent, created)
	}
	st.cands = cands
	msg := &alt12bMessenger{}
	w := alt12bService(st, msg, clk)
	w.Scan(context.Background())
	if st.claims != 0 || msg.calls != 0 {
		t.Fatalf("a candidate with a row was handled: claims=%d calls=%d", st.claims, msg.calls)
	}
	st.cands = append(st.cands, alt12bCand("ws1", 99, AlertFailed, clk.t, "job", "failed"))
	w.Scan(context.Background())
	if len(msg.texts()) != 1 || !strings.Contains(msg.texts()[0], "#99") {
		t.Fatalf("a new execution must alert once: %v", msg.texts())
	}
}

// R-B-1 (fix B): 120 old failing rows neither fill the candidate batch nor
// the retry batch: a new alert goes out in the same scan, and retries are
// bounded to 20 per scan.
func TestPhase12bFixBOldFailingRowsDoNotBlockNewAlert(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{}
	for i := int64(1); i <= 120; i++ {
		c := alt12bCand("ws1", i, AlertFailed, clk.t.Add(-10*time.Hour), "job", "failed")
		st.cands = append(st.cands, c)
		st.seed(c, "pending", 1, clk.t.Add(-time.Minute), time.Time{}, clk.t.Add(-10*time.Hour))
	}
	fresh := alt12bCand("ws1", 9999, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")
	st.cands = append(st.cands, fresh)
	msg := &alt12bMessenger{failIf: func(text string) bool { return !strings.Contains(text, "#9999") }}
	w := alt12bService(st, msg, clk)
	w.Scan(context.Background())
	if got := msg.texts(); len(got) != 1 || !strings.Contains(got[0], "#9999") {
		t.Fatalf("the new alert was blocked: %v", got)
	}
	if want := alertRetryLimit + 1; msg.calls != want {
		t.Fatalf("send attempts = %d, want %d (20 retries + 1 new)", msg.calls, want)
	}
	if r := st.row(fresh); r.status != "sent" {
		t.Fatalf("new alert row = %+v", r)
	}
}

// R-B-4 (fix B): after a restart the cap is read from the rows: 20 sent in
// the last hour suppress a new alert; its summary comes from the rows,
// once; a failed summary keeps the rows and a restart retries it.
func TestPhase12bFixBCapAndSummaryFromRowsAcrossRestart(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	st := &alt12bStore{}
	for i := int64(1); i <= DefaultAlertHourlyCap; i++ {
		c := alt12bCand("ws1", i, AlertFailed, clk.t.Add(-2*time.Hour), "job", "failed")
		st.seed(c, "sent", 1, time.Time{}, clk.t.Add(-10*time.Minute), clk.t.Add(-11*time.Minute))
	}
	late := alt12bCand("ws1", 500, AlertFailed, clk.t.Add(-time.Minute), "job", "failed")
	st.cands = []WorkflowAlert{late}
	msg := &alt12bMessenger{}
	m := &alt12bMetrics{}
	w := alt12bService(st, msg, clk) // a fresh service: no memory of the sends
	w.Metrics = m
	ctx := context.Background()
	t0 := clk.t
	w.Scan(ctx)
	if len(msg.texts()) != 0 || m.suppressed != 1 || st.row(late).status != "suppressed" {
		t.Fatalf("over the cap after a restart: sent=%v suppressed=%d row=%+v", msg.texts(), m.suppressed, st.row(late))
	}
	// 61 minutes later the summary is due; the first send fails.
	clk.t = t0.Add(61 * time.Minute)
	msg.mu.Lock()
	msg.failAll = true
	msg.mu.Unlock()
	alt12bService(st, msg, clk).Scan(ctx)
	if r := st.row(late); r.hasSummary {
		t.Fatalf("a failed summary marked the row: %+v", r)
	}
	// Restart: the retry state is gone, the rows are not.
	msg.mu.Lock()
	msg.failAll = false
	msg.mu.Unlock()
	alt12bService(st, msg, clk).Scan(ctx)
	got := msg.texts()
	if len(got) != 1 || !strings.Contains(got[0], "1 cảnh báo bị gộp") {
		t.Fatalf("summary after restart: %v", got)
	}
	if r := st.row(late); !r.hasSummary || !r.summarised.Equal(clk.t) || r.status != "suppressed" {
		t.Fatalf("summarised row = %+v", r)
	}
	clk.add(time.Hour)
	alt12bService(st, msg, clk).Scan(ctx)
	if len(msg.texts()) != 1 {
		t.Fatalf("summary repeated: %v", msg.texts())
	}
}

// R-B-1 (fix B): a pending stuck alert whose condition ended is closed as
// obsolete without a send, before the cap check; a pending failed alert in
// the same pass is still retried.
func TestPhase12bFixBObsoleteRetriesCloseWithoutSending(t *testing.T) {
	clk := &alt12bClock{t: alt12bStart()}
	stuck := alt12bCand("ws1", 1, AlertStuckRunning, clk.t.Add(-time.Hour), "job", "")
	failed := alt12bCand("ws1", 2, AlertFailed, clk.t.Add(-time.Hour), "job", "failed")
	st := &alt12bStore{obsolete: map[alt12bKey]bool{alt12bK(stuck): true, alt12bK(failed): true}}
	st.seed(stuck, "pending", 1, clk.t.Add(-2*time.Minute), time.Time{}, clk.t.Add(-time.Hour))
	st.seed(failed, "pending", 1, clk.t.Add(-time.Minute), time.Time{}, clk.t.Add(-time.Hour))
	msg := &alt12bMessenger{}
	alt12bService(st, msg, clk).Scan(context.Background())
	if r := st.row(stuck); r.status != "obsolete" || !r.next.IsZero() || !r.sent.IsZero() {
		t.Fatalf("stuck row = %+v, want obsolete without next or sent", r)
	}
	if got := msg.texts(); len(got) != 1 || !strings.Contains(got[0], "#2") {
		t.Fatalf("only the failed alert may be sent: %v", got)
	}
	if r := st.row(failed); r.status != "sent" {
		t.Fatalf("failed alert row = %+v (the obsolete flag must not apply to it)", r)
	}

	// With the hourly cap reached the obsolete one is still closed.
	st2 := &alt12bStore{obsolete: map[alt12bKey]bool{alt12bK(stuck): true}}
	for i := int64(10); i < 10+DefaultAlertHourlyCap; i++ {
		c := alt12bCand("ws1", i, AlertFailed, clk.t.Add(-2*time.Hour), "job", "failed")
		st2.seed(c, "sent", 1, time.Time{}, clk.t.Add(-5*time.Minute), clk.t.Add(-6*time.Minute))
	}
	st2.seed(stuck, "pending", 1, clk.t.Add(-2*time.Minute), time.Time{}, clk.t.Add(-time.Hour))
	msg2 := &alt12bMessenger{}
	alt12bService(st2, msg2, clk).Scan(context.Background())
	if r := st2.row(stuck); r.status != "obsolete" || msg2.calls != 0 {
		t.Fatalf("over the cap: row=%+v calls=%d", r, msg2.calls)
	}
}
