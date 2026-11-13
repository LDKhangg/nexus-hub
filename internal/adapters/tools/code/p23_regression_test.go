package codetools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// P23-F15 (REQ-F15): an existing small clone must also be cancelled while
// fetch/pull is growing it, not just on the initial clone or after exit.
func TestP23_FetchAndPullGrowthCancelledWhileRunning(t *testing.T) {
	for _, name := range []string{"git_sync", "git_pull"} {
		t.Run(name, func(t *testing.T) {
			e := newWS(t, "main", 1<<20)
			e.sync(t)
			networkCalls := 0
			cancelled := false
			runner := approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
				if !c.Network {
					return e.rec.inner.Run(ctx, c)
				}
				networkCalls++
				command := "fetch"
				if name == "git_pull" {
					command = "pull"
				}
				if !containsAny(c.Args, command) {
					t.Fatal("fixture did not reach the expected network operation")
				}
				writeFile(t, e.clone, ".git/objects/growing.pack", strings.Repeat("x", 2<<20))
				<-ctx.Done()
				cancelled = true
				return execrunner.Result{}, ctx.Err()
			})
			list := newWorkspaceTools(runner, e.tokens, e.reposDir, 1<<20, testSan{}, "file://"+e.remoteDir+"/")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := findTool(t, list, name).Run(ctx, tools.Call{Repo: wsGrant(), Account: &tenant.GitHubAccount{Name: "acc"}, Input: json.RawMessage(`{"repo":"acme/app"}`)})
			wantCode(t, err, "clone_too_large")
			if networkCalls != 1 || !cancelled || ctx.Err() != nil {
				t.Fatal("growth was not cancelled before the caller deadline")
			}
			if _, err := os.Stat(filepath.Join(e.clone, ".git")); !os.IsNotExist(err) {
				t.Fatal("oversized managed clone was retained")
			}
		})
	}
}

type p23PushAudit struct {
	started bool
	status  string
}

func (s *p23PushAudit) Begin(context.Context, tools.RunStart) (int64, *tools.PriorRun, error) {
	if s.started {
		return 0, &tools.PriorRun{Status: s.status}, nil
	}
	s.started = true
	s.status = tools.StatusStarted
	return 1, nil, nil
}

func (s *p23PushAudit) Finish(_ context.Context, f tools.RunFinish) error {
	s.status = f.Status
	return nil
}

func (*p23PushAudit) Deny(context.Context, tools.RunStart, string) error { return nil }

// P23-F9 (REQ-F9): simulate losing the push acknowledgement *after* real
// local-bare refs moved. The git adapter and executor must retain ambiguity
// and a fresh executor must not send another push.
func TestP23_AcceptedPushWithRemoteFailureNeverRepeats(t *testing.T) {
	e := newWS(t, "main", 1<<20)
	e.sync(t)
	sh(t, e.clone, "switch", "-c", "nexus/uncertain")
	head := sh(t, e.clone, "rev-parse", "HEAD")
	pushes := 0
	audit := &p23PushAudit{}
	runner := approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
		if !containsAny(c.Args, "push") {
			return e.rec.inner.Run(ctx, c)
		}
		if !audit.started || audit.status != tools.StatusStarted {
			t.Fatal("push happened before its started audit")
		}
		pushes++
		res, err := e.rec.inner.Run(ctx, c)
		if err != nil || res.ExitCode != 0 {
			t.Fatal("local-bare push fixture failed before the effect")
		}
		return execrunner.Result{ExitCode: 1, Stderr: []byte("remote: connection lost after updating refs")}, nil
	})
	list := newWorkspaceTools(runner, e.tokens, e.reposDir, 1<<20, testSan{}, "file://"+e.remoteDir+"/")
	reg, err := tools.NewRegistry(findTool(t, list, "git_push"))
	if err != nil {
		t.Fatal(err)
	}
	g := *wsGrant()
	g.Allow = []string{"git_push"}
	p := tenant.ToolPolicyConfig{MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"acc": {Name: "acc"}}, Repos: map[string]tenant.RepoGrant{"acme/app": g}}
	for attempt := 1; attempt <= 2; attempt++ {
		exec := tools.NewExecutor(reg, p, tools.DefaultLimits(), false, audit, testSan{}, 42, "ws")
		result := exec.Call(context.Background(), attempt, "git_push", json.RawMessage(`{"repo":"acme/app","branch":"nexus/uncertain"}`))
		if !result.IsError || result.Status != tools.StatusUnknownOutcome || audit.status != tools.StatusUnknownOutcome {
			t.Fatal("accepted push failure lost its unknown outcome")
		}
	}
	if pushes != 1 {
		t.Fatal("fresh executor repeated the uncertain push")
	}
	bare := filepath.Join(e.remoteDir, "acme/app.git")
	if got := sh(t, bare, "rev-parse", "refs/heads/nexus/uncertain"); got != head {
		t.Fatal("fixture did not produce its real remote ref update")
	}
}
