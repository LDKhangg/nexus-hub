package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
)

func phase12dFakeGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'fake-git-ran\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func phase12dRegistry(t *testing.T, sandbox bool) (*execrunner.Runner, string) {
	t.Helper()
	s := settings.Defaults()
	s.GitPath = phase12dFakeGit(t)
	s.TmpDir = t.TempDir()
	s.ReposDir = t.TempDir()
	s.BwrapPath = filepath.Join(t.TempDir(), "bwrap")
	s.GitSandbox = sandbox
	redactor := agent.NewRedactor(nil)
	resolver := secrets.NewResolver(nil, nil, redactor, nil, time.Minute, time.Now)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	reg, runner, err := buildToolRegistry(s, resolver, redactor, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	for _, name := range []string{"code_search", "git_sync", "git_pull", "git_push", "git_reset_hard", "git_clean"} {
		if reg.Get(name) == nil {
			t.Errorf("tool %s was not registered", name)
		}
	}
	return runner, logs.String()
}

// TC-D-9 (REQ-14): sandbox startup failure keeps code tools visible but every
// attempted git operation fails closed with one safe code.
func TestPhase12dSandboxStartupFailureFailsEveryGitCallClosed(t *testing.T) {
	runner, logs := phase12dRegistry(t, true)
	if !strings.Contains(logs, "sandbox_unavailable") {
		t.Fatalf("missing fail-closed startup log: %s", logs)
	}
	for _, args := range [][]string{{"status"}, {"clone", "https://github.com/acme/app.git"}, {"push", "origin", "main"}, {"reset", "--hard"}} {
		_, err := runner.Run(context.Background(), execrunner.Command{Args: args, Dir: t.TempDir(), Network: true})
		var denied *execrunner.DeniedError
		if !errors.As(err, &denied) || denied.Code != "sandbox_unavailable" {
			t.Errorf("git %q: %v", args, err)
		}
	}
}

// TC-D-10 (REQ-14): disabling the sandbox is explicit, noisy, and selects the
// legacy runner without consulting the configured bwrap path.
func TestPhase12dSandboxEscapeHatchWarnsAndUsesLegacyRunner(t *testing.T) {
	runner, logs := phase12dRegistry(t, false)
	if !strings.Contains(logs, "WARNING: git_sandbox=false") {
		t.Fatalf("missing escape-hatch warning: %s", logs)
	}
	res, err := runner.Run(context.Background(), execrunner.Command{Args: []string{"status"}})
	if err != nil || !strings.Contains(string(res.Stdout), "fake-git-ran") {
		t.Fatalf("legacy runner result=%q err=%v", res.Stdout, err)
	}
}

// TC-D-13 (REQ-15): Gate C installs a pinned scanner through a verified
// checksum and emits only count-level findings from a redacted report.
func TestPhase12dCISecretScanIsPinnedVerifiedAndNonDisclosing(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	yml := string(raw)
	for _, want := range []string{"- name: secret scan", "sha256sum --check --status", "gitleaks\" git", "--redact", "--report-path \"$tmp/report.json\"", "jq 'length'", "details withheld"} {
		if !strings.Contains(yml, want) {
			t.Errorf("secret scan misses %q", want)
		}
	}
	if !regexp.MustCompile(`version=[0-9]+\.[0-9]+\.[0-9]+`).MatchString(yml) {
		t.Error("gitleaks version is not pinned")
	}
	if strings.Index(yml, "sha256sum --check --status") > strings.Index(yml, "\"$tmp/gitleaks\" git") {
		t.Error("scanner executes before checksum verification")
	}
	for _, leak := range []string{"cat \"$tmp/report.json\"", "jq . \"$tmp/report.json\""} {
		if strings.Contains(yml, leak) {
			t.Errorf("CI prints secret report through %q", leak)
		}
	}
}
