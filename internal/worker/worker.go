package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

const (
	pollWait         = 55 * time.Second // W6
	cleanEvery       = time.Hour
	versionCacheFor  = time.Hour
	countersFileName = "counters.json"
	processLockName  = "worker.lock"
)

// Inhibitor holds a sleep/idle inhibitor while a job runs (REQ-17).
type Inhibitor interface {
	// Start takes the inhibitor; the returned func releases it.
	Start() func()
}

// SysInfo reads machine metrics for telemetry (REQ-18). Tests use a fake.
type SysInfo interface {
	// Snapshot fills CPU, RAM and disk (of dir); unknown values stay nil.
	Snapshot(dir string) behavior.WorkerTelemetry
}

// Worker is the laptop runtime of `nh worker run`. Every dependency is an
// interface so tests can drive it with a fake hub, a fake executor and a
// fake command runner.
type Worker struct {
	Cfg     *Config
	Hub     Hub
	Exec    Executor
	Repos   *Repos
	Runner  Runner // test commands
	ClickUp TaskFetcher
	Inhibit Inhibitor
	Sys     SysInfo
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	Rand    func(n int64) int64
	Log     *log.Logger

	mu        sync.Mutex
	counters  behavior.WorkerCounters
	version   string
	versionAt time.Time
	active    string
}

// New wires a Worker with the real hub client, executor, git/gh, ClickUp
// client, sleep inhibitor and system metrics.
func New(cfg *Config, hubToken string) *Worker {
	runner := OSRunner{}
	var exec Executor
	if cfg.Executor == ExecutorOpencode {
		exec = &OpencodeExecutor{Bin: cfg.ExecutorPaths.Opencode, Model: cfg.Opencode.Model, Agent: cfg.Opencode.Agent, Runner: runner}
	} else {
		exec = &ClaudeExecutor{Bin: cfg.ExecutorPaths.Claude, Model: cfg.Claude.Model, AllowedTools: cfg.Claude.AllowedTools, Runner: runner}
	}
	w := &Worker{
		Cfg:     cfg,
		Hub:     NewHTTPHub(cfg.HubURL, hubToken, nil),
		Exec:    exec,
		Repos:   &Repos{Git: cfg.ExecutorPaths.Git, GH: cfg.ExecutorPaths.GH, CloneWith: cfg.CloneWith, ReposDir: cfg.ReposDir, Runner: runner},
		Runner:  runner,
		Inhibit: systemInhibitor{},
		Sys:     systemInfo{},
	}
	if cfg.ClickUp.TokenFile != "" {
		w.ClickUp = &ClickUpClient{BaseURL: cfg.ClickUp.BaseURL, TokenFile: cfg.ClickUp.TokenFile}
	}
	return w
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

func (w *Worker) sender() *Sender {
	return &Sender{Hub: w.Hub, Sleep: w.Sleep, Back: Backoff{Base: time.Second, Max: time.Minute, Rand: w.Rand}}
}

func (w *Worker) inhibit() func() {
	if w.Inhibit == nil {
		return func() {}
	}
	return w.Inhibit.Start()
}

func (w *Worker) setActive(id string) {
	w.mu.Lock()
	w.active = id
	w.mu.Unlock()
}

// Run is `nh worker run`: one process per device (a lock file), recovery of
// journaled jobs, then the poll loop until ctx ends. It never polls while a
// job runs.
func (w *Worker) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.Cfg.StateDir, 0o700); err != nil {
		return errors.New("state directory not writable")
	}
	if err := os.MkdirAll(w.Cfg.ReposDir, 0o700); err != nil {
		return errors.New("repos_dir not writable")
	}
	unlock, err := lockFile(filepath.Join(w.Cfg.StateDir, processLockName))
	if err != nil {
		return errors.New("another worker is already running on this machine")
	}
	defer unlock()
	w.loadCounters()
	w.logf("worker: started (executor %s)", w.Exec.Name())
	if err := w.Recover(ctx); err != nil {
		return err
	}
	w.Clean(ctx, false)
	lastClean := w.now()
	back := Backoff{Base: time.Second, Max: time.Minute, Rand: w.Rand}
	online := true
	for ctx.Err() == nil {
		if w.now().Sub(lastClean) >= cleanEvery {
			w.Clean(ctx, false)
			lastClean = w.now()
		}
		lease, err := w.Hub.Poll(ctx, pollWait, w.telemetry(ctx, "idle"))
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if online {
				if errors.Is(err, ErrUnauthorized) {
					w.logf("worker: the hub rejected the device token; re-enroll with `nh worker enroll`")
				} else {
					w.logf("worker: hub unreachable; retrying with backoff")
				}
				online = false
			}
			d := back.Next()
			if errors.Is(err, ErrUnauthorized) && d < 5*time.Minute {
				d = 5 * time.Minute
			}
			if w.sleep(ctx, d) != nil {
				break
			}
			continue
		}
		if !online {
			w.logf("worker: connected to the hub")
			online = true
		}
		back.Reset()
		if lease == nil {
			continue
		}
		w.handleLease(ctx, lease)
	}
	w.logf("worker: stopped")
	return nil
}

// Recover handles journaled jobs from an earlier run (REQ-16). A job with
// a queued result has its outbox delivered. A job that was running when
// the worker stopped is reported failed with worker_restarted: its
// executor is gone, the worktree is kept for a retry. The hub rejects
// (409) the report when the lease is no longer held, and it is dropped.
func (w *Worker) Recover(ctx context.Context) error {
	journals, err := ListJournals(w.Cfg.StateDir, w.Now)
	if err != nil {
		return err
	}
	for _, j := range journals {
		st := j.State()
		if st.Done {
			j.Close()
			continue
		}
		if st.Result == nil {
			res := behavior.WorkerResult{Status: behavior.WorkerStatusFailed, ReasonCode: ReasonWorkerRestarted,
				Summary: "The worker restarted while the job was running; the worktree is kept for a retry."}
			if st.Workspace != nil {
				res.Branch = st.Workspace.Branch
			}
			if err := j.EnqueueResult(res); err != nil {
				j.Close()
				return errors.New("job journal not writable")
			}
			w.countJob(res)
			w.logf("worker: reporting an interrupted job after restart")
		}
		if err := w.sender().Flush(ctx, j); err != nil {
			j.Close()
			return nil // shutting down; the next start continues
		}
		_ = j.MarkDone()
		j.Close()
	}
	return nil
}

// Clean removes the worktrees and state of finished jobs: those finished
// more than retention_days ago, or all finished ones when all is set
// (`nh worker clean`). Running or undelivered jobs are never touched. It
// returns how many jobs were cleaned.
func (w *Worker) Clean(ctx context.Context, all bool) int {
	journals, err := ListJournals(w.Cfg.StateDir, w.Now)
	if err != nil {
		return 0
	}
	retention := time.Duration(w.Cfg.RetentionDays) * 24 * time.Hour
	n := 0
	for _, j := range journals {
		st := j.State()
		dir := j.Dir()
		j.Close()
		if !st.Done || (!all && w.now().Sub(st.DoneAt) < retention) {
			continue
		}
		if st.Workspace != nil && st.Workspace.Worktree != "" && !st.Cleaned {
			if _, err := os.Stat(st.Workspace.Worktree); err == nil {
				if err := w.Repos.RemoveWorktree(ctx, st.Workspace.Clone, st.Workspace.Worktree); err != nil {
					w.logf("worker: a finished worktree could not be removed")
					continue
				}
			}
		}
		if err := os.RemoveAll(dir); err == nil {
			n++
		}
	}
	return n
}

// Status is the local state printed by `nh worker status`: no token, no
// instruction text, no summary.
type Status struct {
	HubURL        string `json:"hub_url"`
	Executor      string `json:"executor"`
	ReposDir      string `json:"repos_dir"`
	StateDir      string `json:"state_dir"`
	TokenPresent  bool   `json:"token_present"`
	Running       bool   `json:"running"`
	ActiveJob     string `json:"active_job,omitempty"`
	ActivePhase   string `json:"active_phase,omitempty"`
	PendingEvents int    `json:"pending_events"`
	Finished      int    `json:"finished_jobs_kept"`
	RetentionDays int    `json:"retention_days"`
	behavior.WorkerCounters
}

// LocalStatus reads the state directory without contacting the hub.
func LocalStatus(cfg *Config) Status {
	s := Status{HubURL: cfg.HubURL, Executor: cfg.Executor, ReposDir: cfg.ReposDir, StateDir: cfg.StateDir, RetentionDays: cfg.RetentionDays}
	s.TokenPresent = CheckSecretFile(cfg.TokenFile) == nil
	if unlock, err := lockFile(filepath.Join(cfg.StateDir, processLockName)); err == nil {
		unlock()
	} else if _, serr := os.Stat(cfg.StateDir); serr == nil {
		s.Running = true
	}
	journals, _ := ListJournals(cfg.StateDir, nil)
	for _, j := range journals {
		st := j.State()
		j.Close()
		s.PendingEvents += len(st.Pending)
		if st.Done {
			s.Finished++
			continue
		}
		s.ActiveJob = st.Lease.JobID
		s.ActivePhase = lastPhase(j.Dir())
	}
	if b, err := os.ReadFile(filepath.Join(cfg.StateDir, countersFileName)); err == nil {
		_ = json.Unmarshal(b, &s.WorkerCounters)
	}
	return s
}

func lastPhase(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return ""
	}
	phase := ""
	for _, line := range splitLines(data) {
		var r record
		if json.Unmarshal(line, &r) == nil && r.T == recProgress && r.Progress != nil {
			phase = r.Progress.Phase
		}
	}
	return phase
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// PrintStatus writes s as plain lines.
func PrintStatus(out io.Writer, s Status) {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(out, "hub:            %s\n", s.HubURL)
	fmt.Fprintf(out, "executor:       %s\n", s.Executor)
	fmt.Fprintf(out, "repos_dir:      %s\n", s.ReposDir)
	fmt.Fprintf(out, "device token:   %s\n", map[bool]string{true: "present (0600)", false: "missing or not mode 0600"}[s.TokenPresent])
	fmt.Fprintf(out, "worker running: %s\n", yn(s.Running))
	if s.ActiveJob != "" {
		fmt.Fprintf(out, "active job:     %s (%s)\n", s.ActiveJob, orDash(s.ActivePhase))
	} else {
		fmt.Fprintln(out, "active job:     none")
	}
	fmt.Fprintf(out, "outbox pending: %d\n", s.PendingEvents)
	fmt.Fprintf(out, "finished kept:  %d (removed after %d days)\n", s.Finished, s.RetentionDays)
	fmt.Fprintf(out, "counters:       jobs %d, failures %d, executor tokens %d, busy %.1f h\n",
		s.Jobs, s.Failures, s.ExecutorTokens, float64(s.BusySeconds)/3600)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// telemetry builds a bounded snapshot (REQ-18): metrics, executor and its
// version, counters. Claude subscription limits are left unknown (nil):
// the local CLI exposes no machine-readable form of them.
func (w *Worker) telemetry(ctx context.Context, state string) *behavior.WorkerTelemetry {
	var t behavior.WorkerTelemetry
	if w.Sys != nil {
		t = w.Sys.Snapshot(w.Cfg.ReposDir)
	}
	t.State = state
	if w.Exec != nil {
		t.Executor = w.Exec.Name()
		w.mu.Lock()
		stale := w.versionAt.IsZero() || w.now().Sub(w.versionAt) >= versionCacheFor
		w.mu.Unlock()
		if stale {
			v := w.Exec.Version(ctx)
			w.mu.Lock()
			w.version, w.versionAt = v, w.now()
			w.mu.Unlock()
		}
		w.mu.Lock()
		t.ExecutorVersion = w.version
		w.mu.Unlock()
	}
	t.ClaudeSessionLeftPct, t.ClaudeSessionResetAt, t.ClaudeWeeklyLeftPct, t.ClaudeWeeklyResetAt = nil, nil, nil, nil
	w.mu.Lock()
	t.Counters = w.counters
	w.mu.Unlock()
	if t.Validate() != nil {
		// Never send a snapshot the hub would reject: keep only the safe core.
		t = behavior.WorkerTelemetry{State: state, Executor: t.Executor, Counters: t.Counters}
		if t.Validate() != nil {
			t.Counters = behavior.WorkerCounters{}
		}
	}
	return &t
}

func (w *Worker) loadCounters() {
	b, err := os.ReadFile(filepath.Join(w.Cfg.StateDir, countersFileName))
	if err != nil {
		return
	}
	var c behavior.WorkerCounters
	if json.Unmarshal(b, &c) == nil {
		w.mu.Lock()
		w.counters = c
		w.mu.Unlock()
	}
}

func (w *Worker) countJob(res behavior.WorkerResult) {
	w.mu.Lock()
	w.counters.Jobs++
	if res.Status == behavior.WorkerStatusFailed {
		w.counters.Failures++
	}
	w.counters.ExecutorTokens += res.Usage.InputTokens + res.Usage.OutputTokens
	w.counters.BusySeconds += res.Usage.DurationSeconds
	c := w.counters
	w.mu.Unlock()
	b, _ := json.Marshal(c)
	path := filepath.Join(w.Cfg.StateDir, countersFileName)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
