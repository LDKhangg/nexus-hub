package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
