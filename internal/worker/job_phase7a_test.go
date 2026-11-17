package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Job flow tests (Batch B, plan T-B-80 .. T-B-95). Git is real (a local bare
// remote in a temp directory); the hub, the executor, the gh CLI and the
// repository test command are fakes.

const jobPRURL = "https://github.com/owner/name/pull/7"

// jobHub is a fake hub that records everything the worker sends.
type jobHub struct {
	mu       sync.Mutex
	hbCalls  int
	first    HeartbeatReply
	later    func(n int) (HeartbeatReply, error)
	acks     [][]string
	progress []behavior.WorkerProgress
	results  []behavior.WorkerResult
	leases   chan *Lease
}

func newJobHub() *jobHub { return &jobHub{leases: make(chan *Lease, 4)} }

func (h *jobHub) Poll(ctx context.Context, _ time.Duration, _ *behavior.WorkerTelemetry) (*Lease, error) {
	select {
	case l := <-h.leases:
		return l, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *jobHub) Heartbeat(ctx context.Context, jobID, lease string, t *behavior.WorkerTelemetry) (HeartbeatReply, error) {
	return h.HeartbeatAck(ctx, jobID, lease, t, nil)
}

func (h *jobHub) HeartbeatAck(_ context.Context, _, _ string, _ *behavior.WorkerTelemetry, ackIDs []string) (HeartbeatReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hbCalls++
	h.acks = append(h.acks, append([]string(nil), ackIDs...))
	if h.hbCalls == 1 {
		return h.first, nil
	}
	if h.later != nil {
		return h.later(h.hbCalls)
	}
	return HeartbeatReply{}, nil
}

func (h *jobHub) Progress(_ context.Context, _, _ string, p behavior.WorkerProgress) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.progress = append(h.progress, p)
	return nil
}

func (h *jobHub) Result(_ context.Context, _, _, _ string, r behavior.WorkerResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, r)
	return nil
}

func (h *jobHub) beats() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hbCalls
}

func (h *jobHub) resultList() []behavior.WorkerResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]behavior.WorkerResult(nil), h.results...)
}

func (h *jobHub) ackList() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.acks...)
}

func (h *jobHub) progressList() []behavior.WorkerProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]behavior.WorkerProgress(nil), h.progress...)
}

// fakeExec is a scripted executor.
type fakeExec struct {
	mu   sync.Mutex
	reqs []ExecRequest
	run  func(ctx context.Context, n int, req ExecRequest) (ExecResult, error)
}

func (f *fakeExec) Name() string                   { return ExecutorClaude }
func (f *fakeExec) Version(context.Context) string { return "1.0.0" }
func (f *fakeExec) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	n := len(f.reqs)
	f.mu.Unlock()
	if f.run == nil {
		return ExecResult{}, nil
	}
	return f.run(ctx, n, req)
}

func (f *fakeExec) requests() []ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExecRequest(nil), f.reqs...)
}

// routeRunner sends gh to a fake and everything else to the real OS runner.
type routeRunner struct{ gh *atomic.Int32 }

func (r routeRunner) Run(ctx context.Context, c Cmd) (int, error) {
	if c.Name == "gh" {
		r.gh.Add(1)
		if c.Stdout != nil {
			_, _ = c.Stdout.Write([]byte(jobPRURL + "\n"))
		}
		return 0, nil
	}
	return OSRunner{}.Run(ctx, c)
}

type countInhibitor struct{ started, released atomic.Int32 }

func (c *countInhibitor) Start() func() {
	c.started.Add(1)
	return func() { c.released.Add(1) }
}

type jobEnv struct {
	w       *Worker
	hub     *jobHub
	ex      *fakeExec
	testRun *fakeRunner
	inh     *countInhibitor
	gh      *atomic.Int32
	remote  string
	mainSHA string
}

const jobPayload = `{"repo":"owner/name","task_id":"FC-1","instruction":"add a file"}`

func gitIdentityEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

func newJobEnv(t *testing.T, cfgExtra string) *jobEnv {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitIn(t, root, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-m", "init")
	gitIn(t, seed, "push", "-u", "origin", "main")

	cfg := mustConfig(t, cfgExtra)
	gh := &atomic.Int32{}
	repos := &Repos{Git: "git", GH: "gh", CloneWith: "git", ReposDir: cfg.ReposDir, Runner: routeRunner{gh: gh}, Env: gitIdentityEnv()}
	clone := repos.ClonePath("owner/name")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "clone", remote, clone)
	hub := newJobHub()
	ex := &fakeExec{}
	testRun := &fakeRunner{}
	inh := &countInhibitor{}
	w := &Worker{Cfg: cfg, Hub: hub, Exec: ex, Repos: repos, Runner: testRun, Inhibit: inh, Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Millisecond):
				return nil
			}
		},
		Rand: func(int64) int64 { return 0 },
		Log:  log.New(io.Discard, "", 0)}
	return &jobEnv{w: w, hub: hub, ex: ex, testRun: testRun, inh: inh, gh: gh, remote: remote, mainSHA: gitIn(t, remote, "rev-parse", "refs/heads/main")}
}

const defaultJobID = "0123abcd-0000-0000-0000-000000000001"

func jobLease(id, payload string) *Lease {
	return &Lease{JobID: id, Attempt: 1, LeaseToken: "lease-token", HeartbeatIntervalSeconds: 30, Payload: json.RawMessage(payload)}
}

// runJob runs one lease to its end and returns the single result the hub got.
func (e *jobEnv) runJob(t *testing.T) behavior.WorkerResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e.w.handleLease(ctx, jobLease(defaultJobID, jobPayload))
	rs := e.hub.resultList()
	if len(rs) != 1 {
		t.Fatalf("hub got %d results, want exactly 1: %+v", len(rs), rs)
	}
	return rs[0]
}

func (e *jobEnv) hasRef(ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = e.remote
	return cmd.Run() == nil
}

func (e *jobEnv) mainMoved(t *testing.T) bool {
	t.Helper()
	return gitIn(t, e.remote, "rev-parse", "refs/heads/main") != e.mainSHA
}

func (e *jobEnv) worktree() string {
	return e.w.Repos.WorktreePath(defaultJobID)
}

func addFile(e *jobEnv) {
	e.ex.run = func(_ context.Context, _ int, req ExecRequest) (ExecResult, error) {
		_ = os.WriteFile(filepath.Join(req.Dir, "new.go"), []byte("package x\n\nfunc A() {}\n"), 0o600)
		return ExecResult{FinalMessage: "added new.go", SessionID: "sess_1", Turns: 3, InputTokens: 10, OutputTokens: 5}, nil
	}
}

// T-B-80 (REQ-5, REQ-6, REQ-8, REQ-10, REQ-17): first heartbeat before the executor, credentials kept out, branch pushed, main untouched, draft PR, .nexus never committed.
func TestJob_SuccessPushesBranchAndOpensDraftPR(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_owner_secret_value")
	t.Setenv("SSH_AUTH_SOCK", "/run/owner-agent")
	e := newJobEnv(t, "")
	var beatsAtRun int
	e.ex.run = func(_ context.Context, _ int, req ExecRequest) (ExecResult, error) {
		beatsAtRun = e.hub.beats()
		_ = os.WriteFile(filepath.Join(req.Dir, "new.go"), []byte("package x\n\nfunc A() {}\n"), 0o600)
		return ExecResult{FinalMessage: "added new.go", SessionID: "sess_1", Turns: 3, InputTokens: 10, OutputTokens: 5}, nil
	}
	res := e.runJob(t)
	branch := BranchName("FC-1", defaultJobID)
	if res.Status != behavior.WorkerStatusSucceeded || res.PRURL != jobPRURL || res.Branch != branch || res.TestOutcome != "not_run" || res.Usage.Turns != 3 {
		t.Fatalf("result %+v", res)
	}
	if beatsAtRun < 1 {
		t.Fatal("the executor started before the first heartbeat")
	}
	if !e.hasRef("refs/heads/"+branch) || e.mainMoved(t) {
		t.Fatalf("branch on remote %v, main moved %v", e.hasRef("refs/heads/"+branch), e.mainMoved(t))
	}
	files := gitIn(t, e.remote, "ls-tree", "-r", "--name-only", branch)
	if strings.Contains(files, ".nexus") || !strings.Contains(files, "new.go") {
		t.Fatalf("pushed files:\n%s", files)
	}
	reqs := e.ex.requests()
	if len(reqs) != 1 || reqs[0].Dir != e.worktree() {
		t.Fatalf("executor requests %+v", reqs)
	}
	for _, kv := range reqs[0].Env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GH_TOKEN" || k == "SSH_AUTH_SOCK" {
			t.Errorf("executor env holds %s", k)
		}
	}
	ghDir, _ := envLast(reqs[0].Env, "GH_CONFIG_DIR")
	if entries, err := os.ReadDir(ghDir); err != nil || len(entries) != 0 || !strings.HasPrefix(ghDir, e.w.Cfg.StateDir) {
		t.Fatalf("GH_CONFIG_DIR %q must be an empty directory under the state dir (%v)", ghDir, err)
	}
	ps := e.hub.progressList()
	if len(ps) < 3 || ps[0].Phase != PhaseCloning || ps[len(ps)-1].Phase != PhasePushing {
		t.Fatalf("progress %+v", ps)
	}
	for i := 1; i < len(ps); i++ {
		if ps[i].Seq <= ps[i-1].Seq {
			t.Fatalf("progress seq not increasing: %+v", ps)
		}
	}
	if e.inh.started.Load() != 1 || e.inh.released.Load() != 1 {
		t.Fatalf("inhibitor started %d released %d", e.inh.started.Load(), e.inh.released.Load())
	}
	if e.w.counters.Jobs != 1 || e.w.counters.Failures != 0 {
		t.Fatalf("counters %+v", e.w.counters)
	}
	if _, err := os.Stat(filepath.Join(e.w.Cfg.StateDir, countersFileName)); err != nil {
		t.Fatalf("counters file: %v", err)
	}
	all, _ := json.Marshal(append(e.hub.progressList(), behavior.WorkerProgress{}))
	body, _ := json.Marshal(res)
	for _, leak := range []string{"ghp_owner_secret_value", "/run/owner-agent", "add a file"} {
		if strings.Contains(string(all)+string(body), leak) {
			t.Errorf("report leaks %q", leak)
		}
	}
}

// T-B-81 (REQ-8): a cancel in the first heartbeat reply ends the job as cancelled before the executor runs.
func TestJob_CancelledOnFirstHeartbeat(t *testing.T) {
	e := newJobEnv(t, "")
	e.hub.first = HeartbeatReply{Cancel: true}
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusCancelled || res.ReasonCode != ReasonCancelled {
		t.Fatalf("result %+v", res)
	}
	if len(e.ex.requests()) != 0 || e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("a cancelled job ran or pushed")
	}
}

// blockUntilStopped makes the executor hang until the worker cancels it.
func blockUntilStopped(ctx context.Context, _ int, req ExecRequest) (ExecResult, error) {
	_ = os.WriteFile(filepath.Join(req.Dir, "partial.go"), []byte("package x\n"), 0o600)
	select {
	case <-ctx.Done():
		return ExecResult{Cancelled: true}, nil
	case <-time.After(20 * time.Second):
		return ExecResult{}, errors.New("executor was never stopped")
	}
}

// T-B-82 (REQ-8, R-B-5): a heartbeat rejected with 401 (revoked device) stops the executor; lease_lost, nothing pushed, no PR.
func TestJob_HeartbeatUnauthorizedStopsJobAsLeaseLost(t *testing.T) {
	for name, rejected := range map[string]error{"401": ErrUnauthorized, "409": ErrConflict, "404": ErrNotFound} {
		e := newJobEnv(t, "")
		e.hub.later = func(int) (HeartbeatReply, error) { return HeartbeatReply{}, rejected }
		e.ex.run = blockUntilStopped
		res := e.runJob(t)
		if res.Status != behavior.WorkerStatusFailed || res.ReasonCode != ReasonLeaseLost {
			t.Fatalf("%s: result %+v", name, res)
		}
		if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) || e.mainMoved(t) {
			t.Fatalf("%s: a job that lost its lease pushed or opened a PR", name)
		}
	}
}

// T-B-83 (REQ-8): a cancel arriving in a later heartbeat stops the running executor; cancelled, nothing pushed.
func TestJob_CancelDuringExecution(t *testing.T) {
	e := newJobEnv(t, "")
	e.hub.later = func(int) (HeartbeatReply, error) { return HeartbeatReply{Cancel: true}, nil }
	e.ex.run = blockUntilStopped
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusCancelled || res.ReasonCode != ReasonCancelled {
		t.Fatalf("result %+v", res)
	}
	if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("a cancelled job pushed")
	}
	if _, err := os.Stat(e.worktree()); err != nil {
		t.Fatalf("the worktree must be kept: %v", err)
	}
}

const testCmdCfg = "repos:\n  owner/name:\n    test_command: [go, test, ./...]\n"

// T-B-84 (REQ-8, REQ-5, R-B-3): failing tests give tests_failed and push nothing; the test command runs with an empty HOME and no owner credentials.
func TestJob_TestsFailedAndTestCommandEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_owner_secret_value")
	t.Setenv("SSH_AUTH_SOCK", "/run/owner-agent")
	t.Setenv("GITHUB_TOKEN", "ghp_other")
	e := newJobEnv(t, testCmdCfg)
	addFile(e)
	e.testRun.script = func(Cmd) (int, error) { return 1, nil }
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusFailed || res.ReasonCode != ReasonTestsFailed || res.TestOutcome != "failed" {
		t.Fatalf("result %+v", res)
	}
	if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("failing tests were pushed")
	}
	cmds := e.testRun.calls()
	if len(cmds) != 1 || cmds[0].Name != "go" || strings.Join(cmds[0].Args, " ") != "test ./..." || cmds[0].Dir != e.worktree() {
		t.Fatalf("test command %+v", cmds)
	}
	env := cmds[0].Env
	if home, _ := envLast(env, "HOME"); !strings.HasPrefix(home, e.w.Cfg.StateDir) {
		t.Fatalf("test HOME %q must be a sandbox under the state dir", home)
	}
	if v, _ := envLast(env, "GIT_CONFIG_GLOBAL"); v != os.DevNull {
		t.Errorf("GIT_CONFIG_GLOBAL %q", v)
	}
	if v, _ := envLast(env, "GIT_CONFIG_NOSYSTEM"); v != "1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM %q", v)
	}
	if v, _ := envLast(env, "GIT_TERMINAL_PROMPT"); v != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT %q", v)
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK"} {
		if _, ok := envLast(env, k); ok {
			t.Errorf("test env keeps %s", k)
		}
	}
	if dir, _ := envLast(env, "GH_CONFIG_DIR"); dir == "" {
		t.Error("GH_CONFIG_DIR not set to an empty directory")
	}
}

// T-B-85 (REQ-8, REQ-10): passing tests let the job publish and report test_outcome passed.
func TestJob_TestsPassedPublishes(t *testing.T) {
	e := newJobEnv(t, testCmdCfg)
	addFile(e)
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusSucceeded || res.TestOutcome != "passed" || res.PRURL != jobPRURL {
		t.Fatalf("result %+v", res)
	}
	if len(e.testRun.calls()) != 1 {
		t.Fatal("test command did not run exactly once")
	}
}

// T-B-86 (REQ-8, REQ-10): a diff over max_changed_lines is needs_input/diff_too_large and is never pushed.
func TestJob_DiffTooLarge(t *testing.T) {
	e := newJobEnv(t, "limits:\n  max_changed_lines: 50\n")
	e.ex.run = func(_ context.Context, _ int, req ExecRequest) (ExecResult, error) {
		writeFileNoT(req.Dir, "big.txt", strings.Repeat("line\n", 60))
		return ExecResult{FinalMessage: "wrote big.txt"}, nil
	}
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusNeedsInput || res.ReasonCode != ReasonDiffTooLarge {
		t.Fatalf("result %+v", res)
	}
	if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) || e.mainMoved(t) {
		t.Fatal("an oversized diff was pushed")
	}
}

func writeFileNoT(dir, name, content string) {
	_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
}

// T-B-87 (REQ-8, D-B-1): a small binary file already counts 100 lines, so it trips a 50-line limit.
func TestJob_BinaryFileCountsAsWeightedLines(t *testing.T) {
	e := newJobEnv(t, "limits:\n  max_changed_lines: 50\n")
	e.ex.run = func(_ context.Context, _ int, req ExecRequest) (ExecResult, error) {
		_ = os.WriteFile(filepath.Join(req.Dir, "blob.bin"), []byte{0, 1, 2, 3, 0, 255}, 0o600)
		return ExecResult{FinalMessage: "added a blob"}, nil
	}
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusNeedsInput || res.ReasonCode != ReasonDiffTooLarge || !strings.Contains(res.Summary, "binary") {
		t.Fatalf("result %+v", res)
	}
	if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("a binary change over the limit was pushed")
	}
}

// T-B-88 (REQ-8): an executor that changes nothing gives needs_input/no_changes and pushes nothing.
func TestJob_NoChanges(t *testing.T) {
	e := newJobEnv(t, "")
	e.ex.run = func(context.Context, int, ExecRequest) (ExecResult, error) {
		return ExecResult{FinalMessage: "nothing to do"}, nil
	}
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusNeedsInput || res.ReasonCode != ReasonNoChanges {
		t.Fatalf("result %+v", res)
	}
	if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("an empty change was pushed")
	}
}

// T-B-89 (REQ-8): executor crash, timeout, turn limit and a binary that will not start map to fixed reason codes; the worktree is kept and nothing is pushed.
func TestJob_ExecutorFailureReasonCodes(t *testing.T) {
	cases := map[string]struct {
		res  ExecResult
		err  error
		code string
	}{
		"exit code":   {res: ExecResult{ExitCode: 2}, code: ReasonExecutorCrash},
		"timeout":     {res: ExecResult{TimedOut: true}, code: ReasonExecutorTimeout},
		"turn limit":  {res: ExecResult{TurnLimit: true}, code: ReasonTurnLimit},
		"not started": {err: ErrNotStarted, code: ReasonExecutorCrash},
	}
	for name, c := range cases {
		e := newJobEnv(t, "")
		e.ex.run = func(context.Context, int, ExecRequest) (ExecResult, error) { return c.res, c.err }
		res := e.runJob(t)
		if res.Status != behavior.WorkerStatusFailed || res.ReasonCode != c.code {
			t.Errorf("%s: result %+v", name, res)
		}
		if e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
			t.Errorf("%s: pushed", name)
		}
		if _, err := os.Stat(e.worktree()); err != nil {
			t.Errorf("%s: worktree not kept: %v", name, err)
		}
	}
}

// T-B-90 (REQ-6, R-B-4): an owner instruction is resent by the hub until acked; the worker applies it once and acks its id in later heartbeats.
func TestJob_InstructionAppliedOnceAndAcked(t *testing.T) {
	e := newJobEnv(t, "")
	in := behavior.WorkerInstruction{ID: "v1", Text: "also add docs"}
	e.hub.first = HeartbeatReply{Instructions: []behavior.WorkerInstruction{in}}
	e.hub.later = func(int) (HeartbeatReply, error) {
		return HeartbeatReply{Instructions: []behavior.WorkerInstruction{in}}, nil // not acked yet: resent
	}
	e.ex.run = func(ctx context.Context, _ int, req ExecRequest) (ExecResult, error) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for _, a := range e.hub.ackList() {
				for _, id := range a {
					if id == "v1" {
						writeFileNoT(req.Dir, "new.go", "package x\n")
						return ExecResult{FinalMessage: "done"}, nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return ExecResult{Cancelled: true}, nil
			case <-time.After(5 * time.Millisecond):
			}
		}
		return ExecResult{}, errors.New("instruction was never acknowledged")
	}
	res := e.runJob(t)
	if res.Status != behavior.WorkerStatusSucceeded {
		t.Fatalf("result %+v", res)
	}
	acks := e.hub.ackList()
	if len(acks[0]) != 0 {
		t.Fatalf("first heartbeat acked %v before receiving anything", acks[0])
	}
	data, err := os.ReadFile(filepath.Join(e.worktree(), privateDirName, instructionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "also add docs"); n != 1 || !strings.Contains(string(data), "Follow-up") {
		t.Fatalf("instruction applied %d times:\n%s", n, data)
	}
}

// T-B-91 (REQ-16): a job that was running when the worker stopped is reported worker_restarted exactly once after restart.
func TestWorker_RecoverReportsRestartedJob(t *testing.T) {
	e := newJobEnv(t, "")
	j, err := CreateJournal(e.w.Cfg.StateDir, LeaseRecord{JobID: "job-crashed", Attempt: 1, LeaseToken: "lt", HeartbeatIntervalSeconds: 30, LeasedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	ctx := context.Background()
	if err := e.w.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	rs := e.hub.resultList()
	if len(rs) != 1 || rs[0].Status != behavior.WorkerStatusFailed || rs[0].ReasonCode != ReasonWorkerRestarted {
		t.Fatalf("results %+v", rs)
	}
	if err := e.w.Recover(ctx); err != nil || len(e.hub.resultList()) != 1 {
		t.Fatalf("second recover re-reported the job: %v %d", err, len(e.hub.resultList()))
	}
}

// T-B-92 (REQ-10, REQ-16): Clean keeps a fresh finished job and, with all, removes its worktree and state.
func TestWorker_CleanRemovesFinishedJobs(t *testing.T) {
	e := newJobEnv(t, "")
	addFile(e)
	if res := e.runJob(t); res.Status != behavior.WorkerStatusSucceeded {
		t.Fatalf("result %+v", res)
	}
	ctx := context.Background()
	if n := e.w.Clean(ctx, false); n != 0 {
		t.Fatalf("Clean removed %d fresh jobs", n)
	}
	if _, err := os.Stat(e.worktree()); err != nil {
		t.Fatalf("fresh worktree gone: %v", err)
	}
	if n := e.w.Clean(ctx, true); n != 1 {
		t.Fatalf("Clean(all) removed %d jobs", n)
	}
	if _, err := os.Stat(e.worktree()); err == nil {
		t.Fatal("worktree still exists after Clean(all)")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.w.Cfg.StateDir, "jobs")); len(entries) != 0 {
		t.Fatalf("job state left behind: %d entries", len(entries))
	}
}

// T-B-93 (REQ-17, R-5): the run loop polls, runs a leased job, and a second worker process on the same state directory is refused by the lock.
func TestWorker_RunLoopAndSingleProcessLock(t *testing.T) {
	e := newJobEnv(t, "")
	addFile(e)
	e.hub.leases <- jobLease(defaultJobID, jobPayload)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.w.Run(ctx) }()
	deadline := time.Now().Add(60 * time.Second)
	for len(e.hub.resultList()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if rs := e.hub.resultList(); len(rs) != 1 || rs[0].Status != behavior.WorkerStatusSucceeded {
		t.Fatalf("results %+v", rs)
	}
	second := &Worker{Cfg: e.w.Cfg, Hub: newJobHub(), Exec: &fakeExec{}, Repos: e.w.Repos, Runner: e.testRun, Inhibit: e.inh, Now: time.Now,
		Sleep: e.w.Sleep, Log: log.New(io.Discard, "", 0)}
	if err := second.Run(ctx); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second worker: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if e.inh.started.Load() != 1 || e.inh.released.Load() != 1 {
		t.Fatalf("inhibitor started %d released %d", e.inh.started.Load(), e.inh.released.Load())
	}
}

// T-B-94 (REQ-5): a hostile job id from the hub cannot place the worktree outside the worktrees directory.
func TestJob_HostileJobIDStaysInsideReposDir(t *testing.T) {
	e := newJobEnv(t, "")
	addFile(e)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// CreateJournal refuses the id outright, so the job does not start.
	e.w.handleLease(ctx, jobLease("../../escape", jobPayload))
	if rs := e.hub.resultList(); len(rs) != 0 {
		t.Fatalf("a job with a hostile id ran: %+v", rs)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.w.Cfg.ReposDir), "escape")); err == nil {
		t.Fatal("a path outside the repos dir was created")
	}
}
