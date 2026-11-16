package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- /worker through the inbox worker (T-60) ----

type fakeWorkerAdmin struct {
	mu      sync.Mutex
	devices []WorkerDevice
	revoked []string
	codeErr error
}

func (f *fakeWorkerAdmin) CreateEnrollCode(_ context.Context, _, _ string) (string, time.Time, error) {
	if f.codeErr != nil {
		return "", time.Time{}, f.codeErr
	}
	return "ABCD-EFGH-JKLM", time.Now().Add(WorkerEnrollCodeTTL), nil
}
func (f *fakeWorkerAdmin) ListDevices(context.Context, string) ([]WorkerDevice, error) {
	return f.devices, nil
}
func (f *fakeWorkerAdmin) RevokeDevice(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.devices {
		if d.Name == name && d.RevokedAt == nil {
			now := time.Now()
			f.devices[i].RevokedAt = &now
			f.revoked = append(f.revoked, name)
			return nil
		}
	}
	return ErrNotFound
}

type captureMessenger struct {
	mu    sync.Mutex
	texts []string
}

func (m *captureMessenger) SendMessage(_ context.Context, _ ChatRef, o OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.texts = append(m.texts, o.Text)
	return int64(len(m.texts)), nil
}
func (m *captureMessenger) RemoveButtons(context.Context, ChatRef, int64) error { return nil }
func (m *captureMessenger) AnswerCallback(context.Context, ChatRef, string, string) error {
	return nil
}

func workerCmd(t *testing.T, admin WorkerAdmin, args string) (string, string) {
	t.Helper()
	m := &captureMessenger{}
	d := InboxDeps{Messenger: m}
	if admin != nil {
		d.Workers = admin
	}
	w := NewInboxWorker(d, InboxSettings{})
	out := w.dispatch(context.Background(), InboundUpdate{
		WorkspaceID: "ws", IntegrationID: "bot", ChatID: 5, Kind: UpdateCommand, Command: "/worker", Args: args,
	}, "corr")
	if len(m.texts) != 1 {
		t.Fatalf("want one reply, got %v", m.texts)
	}
	return m.texts[0], out.code
}

// T-60 (REQ-1): /worker enroll shows the one-time code, its lifetime and no token.
func TestWorkerCommand_EnrollShowsCodeOnce(t *testing.T) {
	text, code := workerCmd(t, &fakeWorkerAdmin{}, "enroll")
	if code != "" || !strings.Contains(text, "ABCD-EFGH-JKLM") || !strings.Contains(text, "10 phút") || strings.Contains(text, "nhw_") {
		t.Fatalf("code=%q text=%q", code, text)
	}
	// human: upper case and padding must still work
	if text, _ := workerCmd(t, &fakeWorkerAdmin{}, "  ENROLL  "); !strings.Contains(text, "ABCD-EFGH-JKLM") {
		t.Fatalf("case/space sensitive: %q", text)
	}
}

// T-60 (REQ-1): list shows name, state, last seen; empty list explains how to enroll.
func TestWorkerCommand_ListAndEmpty(t *testing.T) {
	text, _ := workerCmd(t, &fakeWorkerAdmin{}, "list")
	if !strings.Contains(text, "/worker enroll") {
		t.Fatalf("empty list is not helpful: %q", text)
	}
	now := time.Now()
	revoked := now.Add(-time.Hour)
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{
		{Name: "laptop", State: "online", LastSeenAt: &now},
		{Name: "old-pc", RevokedAt: &revoked},
		{Name: "new-pc"},
	}}
	text, _ = workerCmd(t, admin, "")
	for _, want := range []string{"laptop · online", "old-pc · revoked", "new-pc · offline", "chưa từng kết nối"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "nhw_") {
		t.Fatal("token in list")
	}
}

// T-60 (REQ-1): revoke works, unknown name is "not found", typos get usage.
func TestWorkerCommand_RevokeAndMistakes(t *testing.T) {
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{{Name: "laptop"}}}
	text, code := workerCmd(t, admin, "revoke laptop")
	if code != "" || !strings.Contains(text, "laptop") || len(admin.revoked) != 1 {
		t.Fatalf("code=%q text=%q revoked=%v", code, text, admin.revoked)
	}
	if text, code = workerCmd(t, admin, "revoke laptop"); code != "not_found" || text != msgNotFound {
		t.Fatalf("second revoke: code=%q text=%q", code, text)
	}
	if _, code = workerCmd(t, admin, "revoke máy-😀"); code != "not_found" {
		t.Fatalf("unicode name: %q", code)
	}
	for _, args := range []string{"revoke", "revoke a b", "enroll now", "list all", "delete laptop", "nonsense"} {
		text, code := workerCmd(t, admin, args)
		if code != "usage" || !strings.Contains(text, "/worker enroll") {
			t.Fatalf("args %q: code=%q text=%q", args, code, text)
		}
	}
	if len(admin.revoked) != 1 {
		t.Fatalf("a malformed command revoked a device: %v", admin.revoked)
	}
}

// T-60: no store wired -> "not available"; a store error never leaks.
func TestWorkerCommand_UnavailableAndStoreFailure(t *testing.T) {
	if text, code := workerCmd(t, nil, "enroll"); code != "not_available" || text != msgNotAvailable {
		t.Fatalf("code=%q text=%q", code, text)
	}
	text, code := workerCmd(t, &fakeWorkerAdmin{codeErr: errors.New("pq: password=hunter2 relation")}, "enroll")
	if code != "handler_failed" || strings.Contains(text, "hunter2") || strings.Contains(text, "pq:") {
		t.Fatalf("code=%q text=%q", code, text)
	}
}

// ---- value rules (T-61, REQ-7/18/19) ----

func ptrI(v int64) *int64     { return &v }
func ptrF(v float64) *float64 { return &v }

func TestWorkerProgressValidate(t *testing.T) {
	ok := WorkerProgress{Seq: 1, Phase: "cloning"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []WorkerProgress{
		{Seq: 0, Phase: "cloning"},
		{Seq: -3, Phase: "cloning"},
		{Seq: 1, Phase: ""},
		{Seq: 1, Phase: "thinking about your secrets"},
		{Seq: 1, Phase: "editing", Counts: WorkerProgressCounts{FilesChanged: -1}},
		{Seq: 1, Phase: "editing", Counts: WorkerProgressCounts{LinesAdded: 10_000_001}},
	}
	for i, p := range bad {
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d accepted: %+v", i, p)
		}
	}
}

func TestWorkerResultValidate(t *testing.T) {
	good := WorkerResult{Status: "succeeded", Summary: "xong", Branch: "nexus/FC-1-abc", PRURL: "https://github.com/o/r/pull/9", TestOutcome: "passed", ArtifactRefs: []string{"pr:o/r#9"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	vi := strings.Repeat("ệ", WorkerMaxSummaryRunes)
	if err := (WorkerResult{Status: "failed", Summary: vi}).Validate(); err != nil {
		t.Fatalf("2000 Vietnamese runes (6000 bytes) must be accepted: %v", err)
	}
	mutate := func(f func(*WorkerResult)) WorkerResult { r := good; f(&r); return r }
	bad := map[string]WorkerResult{
		"summary 2001 runes": mutate(func(r *WorkerResult) { r.Summary = vi + "x" }),
		"summary bad utf8":   mutate(func(r *WorkerResult) { r.Summary = "a\xff\xfe" }),
		"status":             mutate(func(r *WorkerResult) { r.Status = "done" }),
		"empty status":       mutate(func(r *WorkerResult) { r.Status = "" }),
		"reason free text":   mutate(func(r *WorkerResult) { r.ReasonCode = "Tests failed!" }),
		"reason upper":       mutate(func(r *WorkerResult) { r.ReasonCode = "TestsFailed" }),
		"pr http":            mutate(func(r *WorkerResult) { r.PRURL = "http://github.com/o/r/pull/1" }),
		"pr lookalike host":  mutate(func(r *WorkerResult) { r.PRURL = "https://github.com.evil.example/o/r/pull/1" }),
		"pr userinfo":        mutate(func(r *WorkerResult) { r.PRURL = "https://github.com@evil.example/o/r" }),
		"pr other host":      mutate(func(r *WorkerResult) { r.PRURL = "https://evil.example/https://github.com/" }),
		"pr javascript":      mutate(func(r *WorkerResult) { r.PRURL = "javascript:alert(1)" }),
		"pr query":           mutate(func(r *WorkerResult) { r.PRURL = "https://github.com/o/r/pull/1?x=1" }),
		"pr fragment":        mutate(func(r *WorkerResult) { r.PRURL = "https://github.com/o/r/pull/1#x" }),
		"pr bare":            mutate(func(r *WorkerResult) { r.PRURL = "https://github.com/" }),
		"pr long":            mutate(func(r *WorkerResult) { r.PRURL = "https://github.com/" + strings.Repeat("a", 300) }),
		"branch dotdot":      mutate(func(r *WorkerResult) { r.Branch = "nexus/../main" }),
		"branch dash":        mutate(func(r *WorkerResult) { r.Branch = "-rf" }),
		"branch space":       mutate(func(r *WorkerResult) { r.Branch = "a b" }),
		"test outcome":       mutate(func(r *WorkerResult) { r.TestOutcome = "great" }),
		"21 artifacts": mutate(func(r *WorkerResult) {
			r.ArtifactRefs = make([]string, 21)
			for i := range r.ArtifactRefs {
				r.ArtifactRefs[i] = "a"
			}
		}),
		"artifact path":  mutate(func(r *WorkerResult) { r.ArtifactRefs = []string{"../../etc/passwd x"} }),
		"negative usage": mutate(func(r *WorkerResult) { r.Usage.InputTokens = -1 }),
		"huge usage":     mutate(func(r *WorkerResult) { r.Usage.Turns = 1 << 41 }),
	}
	for name, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %+v", name, r)
		}
	}
	for _, key := range []string{"", " ", "a b", strings.Repeat("k", 129), "-x", "k\n"} {
		if ValidWorkerIdempotencyKey(key) {
			t.Errorf("idempotency key %q accepted", key)
		}
	}
	if !ValidWorkerIdempotencyKey("job-1:attempt.2_x") {
		t.Error("normal idempotency key rejected")
	}
}

func TestWorkerTelemetryValidate(t *testing.T) {
	good := WorkerTelemetry{State: "idle", CPULoad: ptrF(1.5), CPUCores: ptrI(8), RAMUsedBytes: ptrI(4), RAMTotalBytes: ptrI(16),
		DiskUsedBytes: ptrI(10), DiskTotalBytes: ptrI(100), Executor: "claude", ExecutorVersion: "1.2.3", ClaudeSessionLeftPct: ptrI(0), ClaudeWeeklyLeftPct: ptrI(100)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (WorkerTelemetry{State: "busy"}).Validate(); err != nil {
		t.Fatalf("all-unknown snapshot must be valid: %v", err)
	}
	old := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	mut := func(f func(*WorkerTelemetry)) WorkerTelemetry { r := good; f(&r); return r }
	bad := map[string]WorkerTelemetry{
		"state":      mut(func(r *WorkerTelemetry) { r.State = "sleeping" }),
		"cores 0":    mut(func(r *WorkerTelemetry) { r.CPUCores = ptrI(0) }),
		"cpu neg":    mut(func(r *WorkerTelemetry) { r.CPULoad = ptrF(-1) }),
		"cpu huge":   mut(func(r *WorkerTelemetry) { r.CPULoad = ptrF(1e9) }),
		"ram>total":  mut(func(r *WorkerTelemetry) { r.RAMUsedBytes = ptrI(17) }),
		"disk>total": mut(func(r *WorkerTelemetry) { r.DiskUsedBytes = ptrI(101) }),
		"total 0":    mut(func(r *WorkerTelemetry) { r.DiskTotalBytes = ptrI(0); r.DiskUsedBytes = nil }),
		"left 101":   mut(func(r *WorkerTelemetry) { r.ClaudeSessionLeftPct = ptrI(101) }),
		"left -1":    mut(func(r *WorkerTelemetry) { r.ClaudeWeeklyLeftPct = ptrI(-1) }),
		"executor":   mut(func(r *WorkerTelemetry) { r.Executor = "/usr/bin/evil" }),
		"version":    mut(func(r *WorkerTelemetry) { r.ExecutorVersion = "1.0 /home/kane/secret" }),
		"reset old":  mut(func(r *WorkerTelemetry) { r.ClaudeSessionResetAt = &old }),
		"counter":    mut(func(r *WorkerTelemetry) { r.Counters.Jobs = -1 }),
	}
	for name, v := range bad {
		if err := v.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", name)
		}
	}
}

// T-32 (REQ-19) at unit level: boundaries are strict, unknown never blocks.
func TestWorkerThresholdsBlock(t *testing.T) {
	th := DefaultWorkerThresholds
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Minute)
	const gib = int64(1) << 30
	cases := []struct {
		name string
		t    *WorkerTelemetry
		want string
	}{
		{"nil snapshot", nil, ""},
		{"all unknown", &WorkerTelemetry{State: "idle"}, ""},
		{"session 9 blocks", &WorkerTelemetry{ClaudeSessionLeftPct: ptrI(9), ClaudeSessionResetAt: &future}, WorkerBlockClaudeSession},
		{"session 10 ok", &WorkerTelemetry{ClaudeSessionLeftPct: ptrI(10)}, ""},
		{"session 0 no reset time blocks", &WorkerTelemetry{ClaudeSessionLeftPct: ptrI(0)}, WorkerBlockClaudeSession},
		{"session low but reset passed", &WorkerTelemetry{ClaudeSessionLeftPct: ptrI(0), ClaudeSessionResetAt: &past}, ""},
		{"session low, reset exactly now", &WorkerTelemetry{ClaudeSessionLeftPct: ptrI(0), ClaudeSessionResetAt: &now}, ""},
		{"weekly 5 blocks", &WorkerTelemetry{ClaudeWeeklyLeftPct: ptrI(5), ClaudeWeeklyResetAt: &future}, WorkerBlockClaudeWeekly},
		{"disk 4 GiB free blocks", &WorkerTelemetry{DiskUsedBytes: ptrI(96 * gib), DiskTotalBytes: ptrI(100 * gib)}, WorkerBlockDisk},
		{"disk exactly 5 GiB ok", &WorkerTelemetry{DiskUsedBytes: ptrI(95 * gib), DiskTotalBytes: ptrI(100 * gib)}, ""},
		{"disk used unknown ok", &WorkerTelemetry{DiskTotalBytes: ptrI(1)}, ""},
		{"ram 91 blocks", &WorkerTelemetry{RAMUsedBytes: ptrI(91), RAMTotalBytes: ptrI(100)}, WorkerBlockRAM},
		{"ram exactly 90 ok", &WorkerTelemetry{RAMUsedBytes: ptrI(90), RAMTotalBytes: ptrI(100)}, ""},
	}
	for _, c := range cases {
		b := th.Block(c.t, now)
		got := ""
		if b != nil {
			got = b.Reason
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if b := th.Block(&WorkerTelemetry{ClaudeSessionLeftPct: ptrI(9), ClaudeSessionResetAt: &future}, now); b == nil || b.Until == nil || !b.Until.Equal(future) {
		t.Fatalf("block must report the reset time for the topic message: %+v", b)
	}
	strict := WorkerThresholds{MinClaudeLeftPct: 50, MinDiskFreeBytes: 1, MaxRAMUsedPct: 10}
	if strict.Block(&WorkerTelemetry{ClaudeSessionLeftPct: ptrI(40)}, now) == nil {
		t.Fatal("custom threshold ignored")
	}
}

func TestWorkerDeviceDisplayState(t *testing.T) {
	now := time.Now()
	recent, stale, revoked := now.Add(-30*time.Second), now.Add(-2*time.Minute), now
	cases := []struct {
		d    WorkerDevice
		want string
	}{
		{WorkerDevice{LastSeenAt: &recent, State: "online"}, "online"},
		{WorkerDevice{LastSeenAt: &recent, State: "busy"}, "busy"},
		{WorkerDevice{LastSeenAt: &stale, State: "online"}, "offline"},
		{WorkerDevice{}, "offline"},
		{WorkerDevice{LastSeenAt: &recent, RevokedAt: &revoked}, "revoked"},
	}
	for i, c := range cases {
		if got := c.d.DisplayState(now); got != c.want {
			t.Errorf("case %d: %s want %s", i, got, c.want)
		}
	}
}
