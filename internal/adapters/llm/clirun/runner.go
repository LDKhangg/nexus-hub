// Package clirun runs a local AI CLI binary as an LLM provider: no shell, a
// fresh empty working directory per call, a minimal environment, the prompt
// on stdin, its own process group killed on timeout or cancel, a bounded
// output and a concurrency cap.
//
// The runner never logs and never puts stdin, stdout or stderr into an
// error: they may hold prompts, context or answers.
package clirun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// MaxOutputBytes caps stdout. A larger output is an error.
	MaxOutputBytes = 4 << 20
	// maxStderrBytes caps stderr, kept only to classify failures.
	maxStderrBytes = 64 << 10
	// MaxConcurrency is the highest allowed concurrency.
	MaxConcurrency = 4
	// DefaultTimeout applies when Config.Timeout is not set.
	DefaultTimeout = 120 * time.Second
	// waitDelay bounds how long Wait waits for the output pipes after the
	// child exits or is killed (a grandchild may hold them open).
	waitDelay = 2 * time.Second
	// FollowUpTimeout bounds a call's follow-up command.
	FollowUpTimeout = 10 * time.Second
	// followUpOutputBytes caps a follow-up's captured output (discarded).
	followUpOutputBytes = 64 << 10
)

// EnvKeys are the only variables the child receives (when set in the
// hub's environment). HOME, PATH and LANG always; the XDG directories so
// the CLI finds its own sign-in.
var EnvKeys = []string{"HOME", "PATH", "LANG", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}

var (
	// ErrTimeout: the call ran longer than the configured timeout and the
	// process group was killed.
	ErrTimeout = errors.New("clirun: timeout")
	// ErrOutputTooLarge: stdout exceeded MaxOutputBytes.
	ErrOutputTooLarge = errors.New("clirun: output too large")
	// ErrStart: the binary could not be started (missing, not executable).
	ErrStart = errors.New("clirun: cannot start binary")
	// ErrWorkDir: the temporary working directory could not be prepared.
	ErrWorkDir = errors.New("clirun: cannot prepare working directory")
	// ErrFollowUp: a follow-up command failed. Child output is never exposed.
	ErrFollowUp = errors.New("clirun: follow-up failed")
)

// Config configures a Runner.
type Config struct {
	// Path is the absolute path of an executable file.
	Path string
	// Timeout bounds one call (DefaultTimeout when zero).
	Timeout time.Duration
	// Concurrency is the number of calls that may run at once (1..4;
	// 1 when zero).
	Concurrency int
	// TmpRoot is the parent of the per-call working directories
	// (os.TempDir() when empty).
	TmpRoot string
	// LookupEnv reads the hub's environment (os.LookupEnv when nil).
	LookupEnv func(string) (string, bool)
}

// Call is one invocation. Args start after the binary and are passed
// verbatim (no shell). Files are written into the fresh working directory
// before start, by base name, mode 0600 (for example a config file the CLI
// reads from its working directory).
//
// EmptyDirEnv names environment variables the child gets pointing at a
// fresh, empty, hub-owned directory inside the working directory (for
// example XDG_CONFIG_HOME, so the CLI loads no user configuration). They
// replace the same variables from EnvKeys. Names must be environment
// variable names; values are never chosen by the caller.
//
// FollowUp, when set, is called with the stdout captured so far once the
// main process has ended (exited, failed, timed out or been cancelled).
// When it returns arguments, the same binary runs with them, in the same
// working directory and environment, with no stdin, bounded per attempt by
// FollowUpTimeout, before the directory is deleted. A failed command is
// retried once; its output is discarded and the final failure is reported
// on Result without replacing the main command's outcome.
type Call struct {
	Args        []string
	Stdin       []byte
	Files       map[string][]byte
	EmptyDirEnv []string
	FollowUp    func(stdout []byte) []string
}

var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// Result is the outcome of a call whose process ran to exit. A non-zero
// exit is not an error: the caller decides. Stdout and Stderr must never be
// logged or shown.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
	Duration       time.Duration
	// FollowUpError is ErrFollowUp when both follow-up attempts failed.
	FollowUpError error
}

// Runner runs one configured binary.
type Runner struct {
	path    string
	timeout time.Duration
	tmpRoot string
	lookup  func(string) (string, bool)
	sem     chan struct{}
}

// CheckExecutable reports whether path is an absolute path to an existing
// regular executable file (symlinks are followed).
func CheckExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("clirun: path must be absolute and clean")
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return errors.New("clirun: path is not an executable file")
	}
	return nil
}

// New verifies the binary and returns a Runner.
func New(cfg Config) (*Runner, error) {
	if err := CheckExecutable(cfg.Path); err != nil {
		return nil, err
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > MaxConcurrency {
		return nil, fmt.Errorf("clirun: concurrency must be at most %d", MaxConcurrency)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if cfg.TmpRoot != "" && !filepath.IsAbs(cfg.TmpRoot) {
		return nil, errors.New("clirun: tmp root must be absolute")
	}
	lookup := cfg.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &Runner{path: cfg.Path, timeout: timeout, tmpRoot: cfg.TmpRoot, lookup: lookup, sem: make(chan struct{}, conc)}, nil
}

// Path returns the verified binary path.
func (r *Runner) Path() string { return r.path }

// Timeout returns the per-call timeout.
func (r *Runner) Timeout() time.Duration { return r.timeout }

// Env returns the child's environment: only EnvKeys that are set.
func (r *Runner) Env() []string {
	return r.env(nil)
}

// env returns EnvKeys that are set, with the overrides (name → value)
// replacing or adding entries.
func (r *Runner) env(override map[string]string) []string {
	env := []string{}
	for _, k := range EnvKeys {
		if _, ok := override[k]; ok {
			continue
		}
		if v, ok := r.lookup(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	names := make([]string, 0, len(override))
	for k := range override {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		env = append(env, k+"="+override[k])
	}
	return env
}

// Run executes one call. Errors: ctx.Err() (cancelled while waiting for a
// slot or while running; the group is killed), ErrTimeout, ErrStart,
// ErrWorkDir, ErrOutputTooLarge. None of them carries child output.
func (r *Runner) Run(ctx context.Context, c Call) (*Result, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	root := r.tmpRoot
	if root == "" {
		root = os.TempDir()
	}
	dir, err := os.MkdirTemp(root, "nh-llm-")
	if err != nil {
		return nil, ErrWorkDir
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, ErrWorkDir
	}
	for name, data := range c.Files {
		if name == "" || name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsRune(name, 0) {
			return nil, ErrWorkDir
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, ErrWorkDir
		}
	}
	var override map[string]string
	for i, name := range c.EmptyDirEnv {
		if !envNameRe.MatchString(name) {
			return nil, ErrWorkDir
		}
		// A Files entry with the same name makes Mkdir fail (ErrWorkDir).
		sub := filepath.Join(dir, fmt.Sprintf(".nh-env-%d", i))
		if err := os.Mkdir(sub, 0o700); err != nil {
			return nil, ErrWorkDir
		}
		if override == nil {
			override = map[string]string{}
		}
		override[name] = sub
	}
	env := r.env(override)

	rctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.Command(r.path, c.Args...)
	cmd.Dir = dir
	cmd.Env = env // never nil: the hub's environment is never inherited
	cmd.Stdin = bytes.NewReader(c.Stdin)
	stdout := &capWriter{max: MaxOutputBytes}
	stderr := &capWriter{max: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = waitDelay
	// No Pdeathsig: it fires when the forking OS thread exits, which Go may do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, ErrStart
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	stopped := false
	select {
	case waitErr = <-done:
	case <-rctx.Done():
		stopped = true
		killGroup(pid)
		waitErr = <-done
	}
	// Kill any leftover members of the group (e.g. a daemonized child).
	killGroup(pid)
	dur := time.Since(start)

	var followUpErr error
	if c.FollowUp != nil {
		if args := c.FollowUp(stdout.buf.Bytes()); len(args) > 0 {
			for attempt := 0; attempt < 2; attempt++ {
				followUpErr = r.followUp(ctx, dir, env, args)
				if followUpErr == nil {
					break
				}
			}
		}
	}

	if stopped {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTimeout
	}
	if stdout.truncated {
		return nil, ErrOutputTooLarge
	}
	res := &Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Duration: dur, FollowUpError: followUpErr}
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == -1 {
			res.ExitCode = 128 + int(signalOf(ee))
		}
	case errors.Is(waitErr, exec.ErrWaitDelay):
		// The child exited; a descendant held the pipes and was killed.
	default:
		return nil, errors.New("clirun: wait failed")
	}
	return res, nil
}

// followUp runs the binary once more in dir: bounded by FollowUpTimeout even
// when ctx is already cancelled, in its own process group, killed with it.
// Child output is discarded and never included in the returned error.
func (r *Runner) followUp(ctx context.Context, dir string, env, args []string) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FollowUpTimeout)
	defer cancel()
	cmd := exec.Command(r.path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(nil)
	sink := &capWriter{max: followUpOutputBytes}
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.WaitDelay = waitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() != nil {
		return ErrFollowUp
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-fctx.Done():
		killGroup(pid)
		<-done
		return ErrFollowUp
	}
	killGroup(pid)
	if sink.truncated {
		return ErrFollowUp
	}
	var ee *exec.ExitError
	switch {
	case waitErr == nil, errors.Is(waitErr, exec.ErrWaitDelay):
		return nil
	case errors.As(waitErr, &ee):
		return ErrFollowUp
	default:
		return ErrFollowUp
	}
}

func signalOf(ee *exec.ExitError) syscall.Signal {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal()
	}
	return 0
}

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// capWriter keeps the first max bytes and discards the rest.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}
