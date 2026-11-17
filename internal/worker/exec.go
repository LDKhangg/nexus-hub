package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Cmd is one child process. Args never hold a secret or a prompt.
type Cmd struct {
	Dir     string
	Name    string
	Args    []string
	Env     []string // nil: the scrubbed worker environment
	Stdout  io.Writer
	Stderr  io.Writer
	OnStart func(pid int)
}

// Runner starts processes. Tests replace it with a fake that records the
// commands and returns scripted exit codes.
type Runner interface {
	// Run waits for the command. exit is -1 when it did not start or was
	// killed because ctx ended; err is non-nil only when it did not start
	// or ctx ended.
	Run(ctx context.Context, c Cmd) (exit int, err error)
}

// ErrNotStarted: the binary could not be started.
var ErrNotStarted = errors.New("command could not be started")

// OSRunner runs real processes in their own process group (killed as a
// whole on cancel).
type OSRunner struct{}

// Run implements Runner.
func (OSRunner) Run(ctx context.Context, c Cmd) (int, error) {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = ScrubEnv(os.Environ())
	}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return -1, ErrNotStarted
	}
	pid := cmd.Process.Pid
	if c.OnStart != nil {
		c.OnStart(pid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(err), nil
	case <-ctx.Done():
		terminateGroup(pid)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			killGroup(pid)
			<-done
		}
		return -1, ctx.Err()
	}
}

func exitCode(err error) int {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 128
	}
	return 1
}

// ScrubEnv drops every variable that could carry a hub credential or a
// paid API key from env: NEXUS_*, database URLs, *_TOKEN, *_API_KEY,
// *SECRET*, *PASSWORD*. With ANTHROPIC_API_KEY gone `claude` uses the
// owner's subscription login, never the API (W4).
func ScrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || dropEnvKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func dropEnvKey(k string) bool {
	u := strings.ToUpper(k)
	switch {
	case strings.HasPrefix(u, "NEXUS"), u == "DATABASE_URL", u == "TEST_DATABASE_URL",
		strings.HasSuffix(u, "_TOKEN"), strings.HasSuffix(u, "_API_KEY"), strings.HasSuffix(u, "_KEY"),
		strings.Contains(u, "SECRET"), strings.Contains(u, "PASSWORD"), strings.Contains(u, "CREDENTIAL"),
		strings.HasPrefix(u, "CLICKUP"), strings.HasPrefix(u, "TELEGRAM"):
		return true
	}
	return false
}

// GitEnv is the environment for git and gh: the owner's credentials (gh
// login, GH_TOKEN, credential helpers) stay, the hub's do not.
func GitEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if !ok || strings.HasPrefix(u, "NEXUS") || u == "DATABASE_URL" || u == "TEST_DATABASE_URL" ||
			strings.HasPrefix(u, "CLICKUP") || strings.HasPrefix(u, "TELEGRAM") || u == "ANTHROPIC_API_KEY" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
}

// dropCredentialKey reports variables that hand the owner's git, GitHub or
// SSH credentials to a child: agent sockets, askpass helpers, gh and
// GitHub settings, git config redirections and credential managers. This
// removes environment routes only: OpenSSH still reads the passwd home
// (`~/.ssh`) whatever HOME says, so unprotected key files stay reachable
// by a child that runs as the same user.
func dropCredentialKey(k string) bool {
	u := strings.ToUpper(k)
	switch {
	case u == "SSH_AUTH_SOCK", u == "SSH_AGENT_PID", u == "SSH_ASKPASS", u == "SSH_ASKPASS_REQUIRE",
		u == "GIT_ASKPASS", u == "GIT_SSH", u == "GIT_SSH_COMMAND", u == "GIT_CONFIG_GLOBAL",
		u == "GIT_CONFIG_SYSTEM", u == "GIT_CONFIG_NOSYSTEM", u == "GIT_CONFIG_COUNT", u == "GIT_CONFIG_PARAMETERS",
		u == "GIT_DIR", u == "GIT_WORK_TREE", u == "GIT_TERMINAL_PROMPT",
		strings.HasPrefix(u, "GIT_CONFIG_KEY_"), strings.HasPrefix(u, "GIT_CONFIG_VALUE_"),
		strings.HasPrefix(u, "GH_"), strings.HasPrefix(u, "GITHUB_"), strings.HasPrefix(u, "GCM_"):
		return true
	}
	return false
}

// ExecutorEnv is the environment of the executor (claude): ScrubEnv, minus
// every git/GitHub/SSH credential route. gh reads an empty config
// directory (ghDir), git reads no global or system config and never
// prompts. HOME stays: claude finds its subscription login there.
func ExecutorEnv(env []string, ghDir string) []string {
	out := make([]string, 0, len(env)+8)
	for _, kv := range ScrubEnv(env) {
		k, _, _ := strings.Cut(kv, "=")
		if dropCredentialKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GH_CONFIG_DIR="+ghDir, "GH_PROMPT_DISABLED=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never")
}

// TestEnv is the environment of the repository test command, which runs
// code the executor wrote: ExecutorEnv with HOME (and USERPROFILE on
// Windows) moved to an empty directory, and the XDG config, data and state
// directories dropped so they default under it. The build caches (GOPATH,
// GOMODCACHE, GOCACHE, XDG_CACHE_HOME) keep pointing at the real ones so
// builds stay fast and offline-capable.
func TestEnv(env []string, home, ghDir string) []string {
	realHome := envValue(env, "HOME")
	if realHome == "" {
		realHome, _ = os.UserHomeDir()
	}
	cacheDir, _ := os.UserCacheDir()
	base := ExecutorEnv(env, ghDir)
	out := make([]string, 0, len(base)+8)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME":
			continue
		}
		out = append(out, kv)
	}
	setDefault := func(k, v string) {
		if v != "" && envValue(out, k) == "" {
			out = append(out, k+"="+v)
		}
	}
	gopath := envValue(out, "GOPATH")
	if gopath == "" && realHome != "" {
		gopath = filepath.Join(realHome, "go")
	}
	setDefault("GOPATH", gopath)
	if gopath != "" {
		setDefault("GOMODCACHE", filepath.Join(strings.Split(gopath, string(os.PathListSeparator))[0], "pkg", "mod"))
	}
	if cacheDir != "" {
		setDefault("GOCACHE", filepath.Join(cacheDir, "go-build"))
		setDefault("XDG_CACHE_HOME", cacheDir)
	}
	out = append(out, "HOME="+home)
	if runtime.GOOS == "windows" {
		out = append(out, "USERPROFILE="+home)
	}
	return out
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			v = val
		}
	}
	return v
}

// capBuffer keeps the first max bytes written to it.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapBuffer(max int) *capBuffer { return &capBuffer{max: max} }

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *capBuffer) String() string { return c.buf.String() }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
