package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/labstack/echo/v4"
)

// TC-27 (REQ-13): unavailable git disables code tools with a safe warning,
// without breaking HTTP API-tool registration or requiring a master key.
func TestApproved_MissingGitRegistryRemainsAvailable(t *testing.T) {
	s := settings.Defaults()
	s.GitPath = filepath.Join(t.TempDir(), "missing", "git")
	s.TmpDir = t.TempDir()
	s.ReposDir = t.TempDir()
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	red := agent.NewRedactor(nil)
	r := secrets.NewResolver(nil, nil, red, nil, time.Minute, time.Now)
	reg, err := buildTools(s, r, red)
	if err != nil || reg == nil {
		t.Fatal("missing optional git broke startup")
	}
	if reg.Get("git_sync") != nil || reg.Get("code_search") != nil || reg.Get("github_pr_view") == nil || reg.Get("clickup_get_task") == nil {
		t.Error("missing git disabled wrong tools")
	}
	if !strings.Contains(logs.String(), "code and workspace tools disabled") {
		t.Error("missing git produced no actionable warning")
	}
}

// TC-28 (REQ-13): each of three worker exits/errors cancels and awaits every
// other worker. Channels are barriers rather than scheduling sleeps.
func TestApproved_EachWorkerExitCancelsAndAwaitsThree(t *testing.T) {
	for trigger := 0; trigger < 3; trigger++ {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("worker_%d/error_%v", trigger, fail), func(t *testing.T) {
				started := make(chan int, 3)
				release := make(chan struct{})
				finished := make(chan int, 3)
				workers := make([]serviceWorker, 3)
				for i := range workers {
					workers[i] = workerFunc(func(ctx context.Context) error {
						started <- i
						defer func() { finished <- i }()
						if i == trigger {
							<-release
							if fail {
								return errors.New("fake-private-worker-error")
							}
							return nil
						}
						<-ctx.Done()
						return nil
					})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- runServices(ctx, echo.New(), workers, "127.0.0.1:0", time.Second) }()
				for range 3 {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("three workers did not start")
					}
				}
				close(release)
				select {
				case err := <-done:
					if err == nil || strings.Contains(err.Error(), "fake-private-worker-error") {
						t.Error("unexpected exit not safely reported")
					}
				case <-ctx.Done():
					t.Fatal("worker shutdown did not finish")
				}
				for range 3 {
					select {
					case <-finished:
					default:
						t.Error("runServices returned before all workers completed")
					}
				}
			})
		}
	}
}

// TC-31 (REQ-15): static deployment files agree with runtime defaults;
// no service is installed, started, or modified by this test.
func TestApproved_EnvironmentExampleAndPrivateRuntimeDirs(t *testing.T) {
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok || (key != "DATABASE_URL" && key != "NEXUS_MASTER_KEY_FILE") {
			t.Errorf("undocumented process environment setting: %s", key)
		}
	}
	base := t.TempDir()
	repos, tmp := filepath.Join(base, "repos"), filepath.Join(base, "tmp")
	if err := ensurePrivateDirs(repos, tmp); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{repos, tmp} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Error("runtime data directory not 0700")
		}
	}
	s := settings.Defaults()
	if s.AllowDangerous || s.MaxInputTokens != 60000 || s.MaxOutputTokens != 6000 || s.DebounceWindow != 8*time.Second || s.DebounceMaxWait != 30*time.Second || s.TriageModel != "claude-haiku-5-5" || s.ActorModel != "claude-sonnet-5-5" {
		t.Error("documented runtime defaults changed")
	}
}
