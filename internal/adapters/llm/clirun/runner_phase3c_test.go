package clirun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeExecutable(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerPhase3C_IsolatesInvocationAndDeletesWorkdir(t *testing.T) {
	// TC-43, TC-44 (REQ-C2): argv is literal, stdin is separate, the
	// environment is allow-listed, files are private, and cwd is removed.
	p := fakeExecutable(t, `
printf 'argv=%s\n' "$*"
printf 'stdin='; cat; printf '\n'
printf 'cwd=%s\n' "$PWD"
printf 'env='; env | sort | tr '\n' ','; printf '\n'
printf 'mode='; stat -c %a config.json
`)
	root := t.TempDir()
	r, err := New(Config{Path: p, TmpRoot: root, LookupEnv: func(k string) (string, bool) {
		v := map[string]string{"HOME": "/fake-home", "PATH": "/bin", "LANG": "C", "DATABASE_URL": "secret"}[k]
		return v, v != ""
	}})
	if err != nil { t.Fatal(err) }
	res, err := r.Run(context.Background(), Call{Args: []string{"literal;touch", "$(id)"}, Stdin: []byte("private prompt"), Files: map[string][]byte{"config.json": []byte("{}")}, EmptyDirEnv: []string{"XDG_CONFIG_HOME"}})
	if err != nil { t.Fatal(err) }
	out := string(res.Stdout)
	for _, want := range []string{"argv=literal;touch $(id)", "stdin=private prompt", "HOME=/fake-home", "XDG_CONFIG_HOME=", "mode=600"} {
		if !strings.Contains(out, want) { t.Errorf("output missing %q: %s", want, out) }
	}
	if strings.Contains(out, "DATABASE_URL") || strings.Contains(out, "secret") { t.Fatalf("inherited secret environment: %s", out) }
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 { t.Fatalf("workdir not removed: %v %v", entries, err) }
}

func TestRunnerPhase3C_BoundsOutputTimeoutAndConcurrency(t *testing.T) {
	// TC-45, TC-46 (REQ-C2): timeout/cancel and output caps release slots.
	t.Run("output cap", func(t *testing.T) {
		p := fakeExecutable(t, `head -c 4194305 /dev/zero`)
		r, err := New(Config{Path: p, Timeout: time.Second})
		if err != nil { t.Fatal(err) }
		if _, err := r.Run(context.Background(), Call{}); !errors.Is(err, ErrOutputTooLarge) { t.Fatalf("got %v", err) }
	})
	t.Run("timeout and cancelled waiter", func(t *testing.T) {
		p := fakeExecutable(t, `sleep 30`)
		r, err := New(Config{Path: p, Timeout: 50 * time.Millisecond, Concurrency: 1})
		if err != nil { t.Fatal(err) }
		if _, err := r.Run(context.Background(), Call{}); !errors.Is(err, ErrTimeout) { t.Fatalf("got %v", err) }
		ctx, cancel := context.WithCancel(context.Background()); cancel()
		if _, err := r.Run(ctx, Call{}); !errors.Is(err, context.Canceled) { t.Fatalf("slot not released: %v", err) }
	})
}

func TestRunnerPhase3C_RejectsPathsAndTraversal(t *testing.T) {
	// TC-42, TC-44 (REQ-C1, REQ-C2).
	for _, p := range []string{"relative", "/tmp/../tmp/nope"} {
		if err := CheckExecutable(p); err == nil { t.Fatalf("accepted %q", p) }
	}
	p := fakeExecutable(t, `exit 0`)
	r, err := New(Config{Path: p})
	if err != nil { t.Fatal(err) }
	for _, files := range []map[string][]byte{{"../escape": nil}, {"a/b": nil}, {"..": nil}} {
		if _, err := r.Run(context.Background(), Call{Files: files}); !errors.Is(err, ErrWorkDir) { t.Fatalf("files %v: %v", files, err) }
	}
}
