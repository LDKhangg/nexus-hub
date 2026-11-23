// Package execrunner runs exactly one binary, git, with no shell, a minimal
// environment, resource limits, a process-group kill on timeout and a
// global concurrency cap.
package execrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// MaxStreamBytes caps stdout and stderr each.
	MaxStreamBytes = 1 << 20
	// MaxConcurrent is the global cap on running git processes.
	MaxConcurrent = 2

	defaultReadTimeout    = 30 * time.Second
	defaultNetworkTimeout = 120 * time.Second
	cpuSecondsRead        = 20
	cpuSecondsNetwork     = 90
	addressSpaceBytes     = 1 << 30
	fileSizeBytes         = 512 << 20
	niceness              = 10
	basePath              = "/usr/local/bin:/usr/bin:/bin"
)

// Command is one git invocation. The binary is fixed; Args starts after it.
type Command struct {
	Args    []string
	Dir     string
	Stdin   []byte
	Env     []string
	Timeout time.Duration
	Network bool
}

// Result is the outcome of a command that started. A non-zero exit is not
// an error.
type Result struct {
	Stdout, Stderr      []byte
	ExitCode            int
	TimedOut, Truncated bool
}

// DeniedError is returned before exec when a command violates the policy.
type DeniedError struct{ Code string }

func (e *DeniedError) Error() string { return e.Code }

// Runner executes git under the VPS limits.
type Runner struct {
	gitPath     string
	home        string
	sem         chan struct{}
	cpuSeconds  uint64 // overrides the read CPU limit in tests
	asBytes     uint64 // RLIMIT_AS; tests raise it for the Go-based fake git
	bwrapPath   string
	proxy       *connectProxy
	unavailable string
}

// New verifies gitPath (absolute, executable, base name "git") and prepares
// <tmpDir>/home as the child HOME.
func New(gitPath, tmpDir string) (*Runner, error) {
	if !filepath.IsAbs(gitPath) || filepath.Base(gitPath) != "git" {
		return nil, errors.New("git path must be absolute and named git")
	}
	st, err := os.Stat(gitPath)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("git path is not an executable file")
	}
	if !filepath.IsAbs(tmpDir) {
		return nil, errors.New("tmp dir must be absolute")
	}
	home := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("prepare git home: %w", err)
	}
	return &Runner{gitPath: gitPath, home: home, sem: make(chan struct{}, MaxConcurrent), asBytes: addressSpaceBytes}, nil
}

// SandboxConfig selects the verified bubblewrap executable and bounds its
// startup probe. A zero ProbeTimeout uses the production default.
type SandboxConfig struct {
	BwrapPath    string
	ProbeTimeout time.Duration
}

// NewSandboxed prepares a runner that executes git only inside bubblewrap.
// It probes the requested namespaces before starting the private CONNECT
// proxy; any failure leaves no usable runner, so callers can fail closed.
func NewSandboxed(gitPath, tmpDir string, cfg SandboxConfig) (*Runner, error) {
	r, err := New(gitPath, tmpDir)
	if err != nil {
		return nil, err
	}
	if err := verifyExecutable(cfg.BwrapPath, "bwrap"); err != nil {
		return nil, errors.New("bwrap is not an executable file")
	}
	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	probe := exec.CommandContext(ctx, cfg.BwrapPath, "--unshare-all", "--die-with-parent", "--new-session", "--ro-bind", "/", "/", "--", "/bin/true")
	probe.Stdout, probe.Stderr = nil, nil
	if err := probe.Run(); err != nil {
		return nil, errors.New("bwrap startup probe failed")
	}
	p, err := startConnectProxy()
	if err != nil {
		return nil, errors.New("git egress proxy startup failed")
	}
	r.bwrapPath, r.proxy = cfg.BwrapPath, p
	return r, nil
}

// NewUnavailable returns a fail-closed runner. It lets tools remain
// registered while every attempted git call returns the supplied safe code.
func NewUnavailable(code string) *Runner {
	return &Runner{sem: make(chan struct{}, MaxConcurrent), unavailable: code, asBytes: addressSpaceBytes}
}

func verifyExecutable(path, base string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) != base {
		return errors.New("executable path is invalid")
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return errors.New("executable path is not usable")
	}
	return nil
}

// Close stops the runner's loopback proxy. It is safe to call more than once.
func (r *Runner) Close() error {
	if r == nil || r.proxy == nil {
		return nil
	}
	return r.proxy.Close()
}

// GitPath returns the verified git binary path.
func (r *Runner) GitPath() string { return r.gitPath }

var allowedSubcommands = map[string]bool{
	// read
	"rev-parse": true, "cat-file": true, "ls-tree": true, "grep": true, "show": true, "log": true,
	"diff": true, "blame": true, "status": true, "branch": true, "check-ref-format": true,
	// write
	"clone": true, "fetch": true, "pull": true, "checkout": true, "switch": true, "add": true,
	"commit": true, "config": true, "push": true,
	// dangerous
	"reset": true, "clean": true,
}

var networkSubcommands = map[string]bool{"clone": true, "fetch": true, "pull": true, "push": true}

// checkArgs validates the global options and the subcommand, and applies
// defense-in-depth argument bans that tools must never need.
func checkArgs(args []string) error {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-c" || a == "-C":
			if i+1 >= len(args) {
				return &DeniedError{Code: "subcommand_denied"}
			}
			i += 2
			continue
		case a == "--no-pager":
			i++
			continue
		}
		break
	}
	if i >= len(args) || !allowedSubcommands[args[i]] {
		return &DeniedError{Code: "subcommand_denied"}
	}
	sub, rest := args[i], args[i+1:]
	for _, a := range rest {
		if a == "--" {
			break
		}
		for _, p := range []string{"--upload-pack", "--receive-pack", "--exec", "--template", "--config", "--separate-git-dir"} {
			if a == p || strings.HasPrefix(a, p+"=") {
				return &DeniedError{Code: "argument_denied"}
			}
		}
		if networkSubcommands[sub] && (a == "-u" || a == "-c") {
			return &DeniedError{Code: "argument_denied"}
		}
		if sub == "push" {
			if a == "-f" || strings.HasPrefix(a, "--force") || a == "--mirror" || a == "--delete" || a == "-d" ||
				a == "--all" || a == "--tags" || a == "--prune" || strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":") {
				return &DeniedError{Code: "argument_denied"}
			}
		}
	}
	return nil
}

var allowedEnvPrefixes = []string{"GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL="}

func checkEnv(env []string) error {
	for _, kv := range env {
		ok := false
		for _, p := range allowedEnvPrefixes {
			if strings.HasPrefix(kv, p) {
				ok = true
				break
			}
		}
		if !ok {
			return &DeniedError{Code: "env_denied"}
		}
	}
	return nil
}

// Run executes git. Errors are policy denials, cancellation while waiting
// for a slot, or a failure to start the process.
func (r *Runner) Run(ctx context.Context, c Command) (Result, error) {
	if r.unavailable != "" {
		return Result{}, &DeniedError{Code: r.unavailable}
	}
	if err := checkArgs(c.Args); err != nil {
		return Result{}, err
	}
	if err := checkEnv(c.Env); err != nil {
		return Result{}, err
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultReadTimeout
		if c.Network {
			timeout = defaultNetworkTimeout
		}
	}
	cpu := uint64(cpuSecondsRead)
	if r.cpuSeconds > 0 {
		cpu = r.cpuSeconds
	}
	if c.Network {
		cpu = cpuSecondsNetwork
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := c.Dir
	if dir == "" {
		dir = r.home
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Result{}, &DeniedError{Code: "working_directory_denied"}
	}
	commandPath, commandArgs := r.gitPath, c.Args
	if r.bwrapPath != "" {
		args, err := r.sandboxArgs(dir, c.Network && isNetworkCommand(c.Args))
		if err != nil {
			return Result{}, err
		}
		commandPath = r.bwrapPath
		commandArgs = append(args, append([]string{"--", r.gitPath}, c.Args...)...)
	}
	cmd := exec.Command(commandPath, commandArgs...)
	cmd.Dir = dir
	fixedEnv := []string{
		"PATH=" + basePath,
		"HOME=" + r.home,
		"LANG=C.UTF-8",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"NO_COLOR=1",
	}
	if r.bwrapPath != "" {
		fixedEnv = append(fixedEnv, "GIT_ALLOW_PROTOCOL=https")
		if r.proxy != nil && c.Network && isNetworkCommand(c.Args) {
			proxyURL := "http://" + r.proxy.Addr()
			fixedEnv = append(fixedEnv, "https_proxy="+proxyURL, "HTTPS_PROXY="+proxyURL)
		}
	}
	cmd.Env = append(fixedEnv, c.Env...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	stdout := &capWriter{max: MaxStreamBytes}
	stderr := &capWriter{max: MaxStreamBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// No Pdeathsig: it fires when the forking OS thread exits, which Go may do.
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start git: %w", err)
	}
	pid := cmd.Process.Pid
	if err := applyLimits(pid, cpu, r.asBytes, fileSizeBytes, niceness); err != nil {
		killGroup(pid)
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("apply limits: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-rctx.Done():
		timedOut = true
		killGroup(pid)
		waitErr = <-done
	}
	// Kill any leftover members of the group (e.g. a daemonized child).
	killGroup(pid)

	res := Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), TimedOut: timedOut, Truncated: stdout.truncated || stderr.truncated}
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == -1 {
			res.ExitCode = 128 + int(signalOf(ee))
		}
	default:
		return res, fmt.Errorf("wait git: %w", waitErr)
	}
	return res, nil
}

func isNetworkCommand(args []string) bool {
	for i := 0; i < len(args); {
		switch args[i] {
		case "-c", "-C":
			i += 2
		case "--no-pager":
			i++
		default:
			return networkSubcommands[args[i]]
		}
	}
	return false
}

func (r *Runner) sandboxArgs(dir string, network bool) ([]string, error) {
	if forbiddenWritablePath(dir) || forbiddenWritablePath(r.home) {
		return nil, &DeniedError{Code: "working_directory_denied"}
	}
	args := []string{"--unshare-all"}
	if network {
		args = append(args, "--share-net")
	}
	args = append(args, "--die-with-parent", "--new-session", "--tmpfs", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc/ssl", "/etc/pki", "/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/gai.conf"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	if !underAny(r.gitPath, "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32") {
		args = append(args, "--ro-bind", r.gitPath, r.gitPath)
	}
	for _, path := range uniquePaths(dir, r.home) {
		args = append(args, "--bind", path, path)
	}
	return append(args, "--chdir", dir), nil
}

func forbiddenWritablePath(path string) bool {
	return path == "/home" || strings.HasPrefix(path, "/home/")
}

func underAny(path string, roots ...string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func uniquePaths(paths ...string) []string {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

func signalOf(ee *exec.ExitError) syscall.Signal {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal()
	}
	return 0
}

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
