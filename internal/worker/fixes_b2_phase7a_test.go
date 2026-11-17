package worker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Fix Brief B2 regression tests: claude version gate, first-heartbeat 401,
// write-confinement settings. Fakes only.

// T-B2-1 (REQ-5, R-B-10): claudeVersionSupported compares against ClaudeMinVersion and refuses empty or unparseable text.
func TestClaudeVersionSupported(t *testing.T) {
	cases := map[string]bool{
		"2.1.314":   true,
		"2.1.315":   true,
		"2.2.0":     true,
		"3.0.0":     true,
		"10.0.0":    true,
		"2.1.313":   false,
		"2.1.9":     false,
		"2.0.999":   false,
		"1.0.0":     false,
		"":          false,
		"   ":       false,
		"garbage":   false,
		"latest":    false,
		"-2.1.314":  false,
		"v2.1.314x": false,
	}
	for in, want := range cases {
		if got := claudeVersionSupported(in); got != want {
			t.Errorf("claudeVersionSupported(%q) = %v, want %v", in, got, want)
		}
	}
}

// T-B2-2 (REQ-5, R-B-10): an old, empty or garbage claude version fails the job with executor_unsupported before any clone, worktree, executor run, push or PR.
func TestJob_UnsupportedClaudeVersionFailsBeforeClone(t *testing.T) {
	for _, v := range []string{"2.1.313", "1.0.0", "", "garbage", "0.0.0"} {
		v := v
		t.Run("version="+v, func(t *testing.T) {
			e := newJobEnv(t, "")
			e.ex.ver = &v
			res := e.runJob(t)
			if res.Status != behavior.WorkerStatusFailed || res.ReasonCode != ReasonExecutorUnsupported {
				t.Fatalf("result %+v", res)
			}
			if len(e.ex.requests()) != 0 || e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) || e.mainMoved(t) {
				t.Fatal("an unsupported-version job ran the executor, pushed or opened a PR")
			}
			if _, err := os.Stat(e.worktree()); err == nil {
				t.Fatal("a worktree was created before the version gate")
			}
			for _, p := range e.hub.progressList() {
				if p.Phase == PhaseCloning {
					t.Fatalf("progress reached %s before the version gate", p.Phase)
				}
			}
			if !strings.Contains(res.Summary, ClaudeMinVersion) {
				t.Errorf("summary %q should name the minimum version", res.Summary)
			}
		})
	}
}

// T-B2-3 (REQ-5, R-B-10): a supported version runs the job normally (the gate does not block).
func TestJob_SupportedClaudeVersionRuns(t *testing.T) {
	for _, v := range []string{ClaudeMinVersion, "2.1.400", "2.2.0"} {
		v := v
		e := newJobEnv(t, "")
		e.ex.ver = &v
		addFile(e)
		if res := e.runJob(t); res.Status != behavior.WorkerStatusSucceeded {
			t.Fatalf("version %s: result %+v", v, res)
		}
	}
}

// T-B2-4 (R-B-5): a 401 on the first heartbeat skips the job: one heartbeat only (no retry), no clone, no executor, no result.
func TestJob_FirstHeartbeat401SkipsWithoutRetry(t *testing.T) {
	e := newJobEnv(t, "")
	e.hub.firstErr = ErrUnauthorized
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e.w.handleLease(ctx, jobLease(defaultJobID, jobPayload))
	if n := e.hub.beats(); n != 1 {
		t.Fatalf("heartbeats = %d, want exactly 1 (no retry on 401)", n)
	}
	if len(e.ex.requests()) != 0 || e.gh.Load() != 0 || e.hasRef("refs/heads/"+BranchName("FC-1", defaultJobID)) {
		t.Fatal("a job with a rejected token ran or pushed")
	}
	if _, err := os.Stat(e.worktree()); err == nil {
		t.Fatal("a worktree was created for a skipped job")
	}
	if len(e.hub.progressList()) != 0 {
		t.Fatalf("progress sent for a skipped job: %+v", e.hub.progressList())
	}
}

// T-B2-5 (REQ-5, R-B-9): --settings blocks both reads and writes outside the worktree, and the deny list covers Edit on the six credential/system paths.
func TestClaudeSettingsBlockWritesAndDenyEdit(t *testing.T) {
	var s struct {
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(claudeSettingsJSON), &s); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"blockReadsOutsideWorkingDirectories", "blockWritesOutsideWorkingDirectories"} {
		if v, ok := s.Permissions[k].(bool); !ok || !v {
			t.Errorf("settings %s = %v, want true", k, s.Permissions[k])
		}
	}
	run := claudeRunner(`{}`, 0)
	e := &ClaudeExecutor{Bin: "claude", AllowedTools: DefaultClaudeAllowedTools, Runner: run}
	if _, err := e.Run(context.Background(), ExecRequest{Dir: "/wt", MaxTurns: 5, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	denied := argAfter(run.calls()[0].Args, "--disallowedTools")
	for _, must := range []string{"Edit(~/.ssh/**)", "Edit(~/.config/**)", "Edit(~/.local/**)", "Edit(~/.gnupg/**)", "Edit(~/.aws/**)", "Edit(//etc/**)"} {
		found := false
		for _, d := range strings.Split(denied, ",") {
			if d == must {
				found = true
			}
		}
		if !found {
			t.Errorf("deny-list lacks %q", must)
		}
	}
	for _, a := range strings.Split(argAfter(run.calls()[0].Args, "--allowedTools"), ",") {
		if strings.HasPrefix(a, "Edit(") && strings.Contains(a, "..") {
			t.Errorf("allow-list widens Edit: %q", a)
		}
	}
}
