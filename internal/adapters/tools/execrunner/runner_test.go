package execrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var fakeGitPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakegit")
	if err != nil {
		panic(err)
	}
	fakeGitPath = filepath.Join(dir, "git")
	build := exec.Command("go", "build", "-o", fakeGitPath, "./testdata/fakegit")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(fakeGitPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.asBytes != 1<<30 {
		t.Fatalf("default RLIMIT_AS = %d", r.asBytes)
	}
	// The fake git is a Go program whose runtime can exceed 1 GiB of
	// virtual address space under load; real git is not affected.
	r.asBytes = 8 << 30
	return r
}

func TestRunner_RejectsNonGitBinary(t *testing.T) {
	for _, p := range []string{"/bin/sh", "git", filepath.Join(t.TempDir(), "git")} {
		if _, err := New(p, t.TempDir()); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestRunner_SubcommandAllowlist(t *testing.T) {
	r := newRunner(t)
	for _, args := range [][]string{
		{"daemon"}, {"submodule", "update"}, {"remote", "add", "x", "y"}, {"gc"}, {"worktree", "add"},
		{"filter-branch"}, {"-c", "x=y", "daemon"}, {"-C", "/tmp", "--no-pager", "upload-pack", "."},
		{}, {"--exec-path=/tmp", "log"}, {"fetch", "--upload-pack=touch /tmp/x"}, {"clone", "-u", "sh", "url"},
		{"push", "--force", "origin", "a"}, {"push", "origin", "+a:a"}, {"push", "--mirror"}, {"push", "--delete", "origin", "a"},
		{"clone", "-c", "core.fsmonitor=x", "url"}, {"clone", "--template=/tmp", "url"},
	} {
		_, err := r.Run(context.Background(), Command{Args: args})
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Errorf("%v: want denied, got %v", args, err)
		}
	}
	res, err := r.Run(context.Background(), Command{Args: []string{"-C", t.TempDir(), "--no-pager", "-c", "core.pager=cat", "log", "-n", "1"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("log: %v %+v", err, res)
	}
}

func TestRunner_ArgsVerbatim(t *testing.T) {
	r := newRunner(t)
	args := []string{"log", "--format=%H %s", "a b; rm -rf /", "$(id)", "--", "x y"}
	res, err := r.Run(context.Background(), Command{Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(string(res.Stdout), "\x00"); strings.Join(got, "|") != strings.Join(args, "|") {
		t.Fatalf("got %q", got)
	}
}

func TestRunner_EnvMinimal(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@h/db")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-xxx")
	r := newRunner(t)
	res, err := r.Run(context.Background(), Command{Args: []string{"status"}, Env: []string{"GIT_CONFIG_COUNT=1"}})
	if err != nil {
		t.Fatal(err)
	}
	env := string(res.Stdout)
	for _, bad := range []string{"DATABASE_URL", "ANTHROPIC_API_KEY"} {
		if strings.Contains(env, bad) {
			t.Fatalf("%s leaked", bad)
		}
	}
	for _, want := range []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME=" + r.home, "LANG=C.UTF-8"} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing %s in %q", want, env)
		}
	}
	if _, err := r.Run(context.Background(), Command{Args: []string{"status"}, Env: []string{"HOME=/root"}}); err == nil {
		t.Fatal("HOME override accepted")
	}
	if _, err := r.Run(context.Background(), Command{Args: []string{"status"}, Env: []string{"LD_PRELOAD=/x.so"}}); err == nil {
		t.Fatal("LD_PRELOAD accepted")
	}
}

func TestRunner_TimeoutKillsGroup(t *testing.T) {
	r := newRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	res, err := r.Run(context.Background(), Command{Args: []string{"show", pidFile}, Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("res %+v after %v", res, time.Since(start))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if p, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); strings.Contains(string(p), ") Z ") {
			break // zombie awaiting reparent reap: dead
		}
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunner_OutputCap(t *testing.T) {
	r := newRunner(t)
	res, err := r.Run(context.Background(), Command{Args: []string{"diff"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != MaxStreamBytes || !res.Truncated || res.ExitCode != 0 {
		t.Fatalf("len %d truncated %v exit %d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
}

func TestRunner_Concurrency(t *testing.T) {
	r := newRunner(t)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.Run(context.Background(), Command{Args: []string{"blame"}})
			if err != nil || res.ExitCode != 0 {
				t.Errorf("err %v exit %d stderr %.300s", err, res.ExitCode, res.Stderr)
			}
		}()
	}
	wg.Wait()
	if el := time.Since(start); el < 600*time.Millisecond {
		t.Fatalf("3 runs of 300ms finished in %v; semaphore not applied", el)
	}
}

func TestRunner_CPULimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	r := newRunner(t)
	r.cpuSeconds = 1
	start := time.Now()
	res, err := r.Run(context.Background(), Command{Args: []string{"grep"}, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.ExitCode == 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("busy loop not killed by RLIMIT_CPU: %+v after %v", res, time.Since(start))
	}
}

func TestRunner_LimitsApplied(t *testing.T) {
	r := newRunner(t)
	r.asBytes = 6 << 30
	res, err := r.Run(context.Background(), Command{Args: []string{"rev-parse"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	out := string(res.Stdout)
	for _, want := range []string{"Max cpu time              20", "Max file size             536870912", "Max address space         6442450944", "nice=10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}
