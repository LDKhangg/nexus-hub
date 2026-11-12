package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Simulate the crash boundary after the real side effect but before its final
// audit update. Begin and budget queries remain the real durable DB operations.
type approvedLostFinish struct{ *ToolRunStore }

func (*approvedLostFinish) Finish(context.Context, tools.RunFinish) error {
	return errors.New("fake crash before audit finish")
}

// TC-13/14 (REQ-6): dropping prior-started detection repeats a real local git
// operation or a remote write. An uncertain result must remain visibly unknown.
func TestApproved_CrashAfterCommitPushCommentMergeNeverRepeats(t *testing.T) {
	for _, name := range []string{"git_commit", "git_push", "github_pr_comment", "github_pr_merge"} {
		t.Run(name, func(t *testing.T) {
			ctx, pool, store, job := newToolRunDB(t)
			red := agent.NewRedactor(nil)
			account := tenant.GitHubAccount{Name: "p", CommitName: "Nexus", CommitEmail: "n@example.invalid"}
			g := tenant.RepoGrant{Repo: "acme/app", Account: "p", Managed: true, Allow: []string{name}, PushBranches: []string{"nexus/*"}, MergeBaseBranches: []string{"develop"}, MergeMethods: []string{"squash"}, RequireChecksGreen: true}
			var all []tools.Tool
			input := `{"repo":"acme/app","number":7,"body":"Báo cáo 🧠"}`
			var effects atomic.Int32
			var clone, bare, gitPath string
			var countBefore string
			if strings.HasPrefix(name, "git_") {
				var err error
				gitPath, err = exec.LookPath("git")
				if err != nil {
					t.Fatal("git missing")
				}
				gitPath, _ = filepath.Abs(gitPath)
				remote := bareRemote(t, gitPath)
				bare = filepath.Join(remote, "acme/app.git")
				repos := t.TempDir()
				clone = filepath.Join(repos, "acme/app")
				runner, err := execrunner.New(gitPath, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				all = codetools.NewWorkspaceToolsWithLocalRemote(runner, approvedCredentialSource{}, repos, 1<<28, red, "file://"+remote+"/")
				var syncTool tools.Tool
				for _, tl := range all {
					if tl.Spec().Name == "git_sync" {
						syncTool = tl
					}
				}
				if _, err := syncTool.Run(ctx, tools.Call{Repo: &g, Account: &account, WorkspaceID: workspaceA, Input: json.RawMessage(`{"repo":"acme/app"}`)}); err != nil {
					t.Fatal("local clone failed")
				}
				gitRun(t, gitPath, clone, "switch", "-c", "nexus/crash")
				if name == "git_commit" {
					if err := os.WriteFile(filepath.Join(clone, "safe.txt"), []byte("business change"), 0600); err != nil {
						t.Fatal(err)
					}
					input = `{"repo":"acme/app","paths":["safe.txt"],"message":"Crash boundary"}`
				} else {
					input = `{"repo":"acme/app","branch":"nexus/crash"}`
				}
				cmd := exec.Command(gitPath, "-C", clone, "rev-list", "--count", "HEAD")
				b, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				countBefore = strings.TrimSpace(string(b))
			} else {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == "POST" || r.Method == "PUT":
						var n int
						if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_runs WHERE status='started'`).Scan(&n); err != nil || n != 1 {
							t.Error("remote effect before started audit")
						}
						effects.Add(1)
						if r.Method == "PUT" {
							io.WriteString(w, `{"merged":true,"sha":"merged123"}`)
						} else {
							io.WriteString(w, `{"id":123}`)
						}
					case strings.HasSuffix(r.URL.Path, "/pulls/7"):
						io.WriteString(w, `{"number":7,"state":"open","draft":false,"head":{"sha":"abcdef1234"},"base":{"ref":"develop"}}`)
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						io.WriteString(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
					case strings.HasSuffix(r.URL.Path, "/status"):
						io.WriteString(w, `{"state":"success","statuses":[]}`)
					default:
						w.WriteHeader(404)
					}
				}))
				defer srv.Close()
				api := githubapi.NewClientAt(srv.URL, srv.Client(), approvedCredentialSource{})
				all = append(githubtools.NewWriteTools(api, red), githubtools.NewDangerousTools(api, red)...)
				if name == "github_pr_merge" {
					input = `{"repo":"acme/app","number":7,"head_sha":"abcdef1234","merge_method":"squash"}`
				}
			}
			reg, err := tools.NewRegistry(all...)
			if err != nil {
				t.Fatal(err)
			}
			policy := tenant.ToolPolicyConfig{Enabled: []string{name}, MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"p": account}, Repos: map[string]tenant.RepoGrant{"acme/app": g}}
			first := tools.NewExecutor(reg, policy, tools.DefaultLimits(), true, &approvedLostFinish{store}, red, job, workspaceA).Call(ctx, 1, name, json.RawMessage(input))
			if first.Status != tools.StatusOK {
				t.Fatalf("fixture failed before crash: %s", first.Content)
			}
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM tool_runs`).Scan(&status); err != nil || status != "started" {
				t.Fatal("crash boundary did not leave started audit")
			}
			second := tools.NewExecutor(reg, policy, tools.DefaultLimits(), true, store, red, job, workspaceA).Call(ctx, 2, name, json.RawMessage(input))
			if second.Status != tools.StatusUnknownOutcome || !second.IsError || !strings.Contains(second.Content, "unknown_outcome") {
				t.Errorf("crash retry status=%s content=%s; expected explicit unknown_outcome", second.Status, second.Content)
			}
			if strings.HasPrefix(name, "github_") {
				if effects.Load() != 1 {
					t.Error("remote effect repeated after crash")
				}
			} else {
				if name == "git_commit" {
					cmd := exec.Command(gitPath, "-C", clone, "rev-list", "--count", "HEAD")
					b, err := cmd.Output()
					if err != nil || countBefore != "1" || strings.TrimSpace(string(b)) != "2" {
						t.Error("commit did not create exactly one new commit")
					}
				} else {
					cmd := exec.Command(gitPath, "--git-dir", bare, "rev-parse", "refs/heads/nexus/crash")
					if _, err := cmd.Output(); err != nil {
						t.Error("push did not reach local bare remote")
					}
				}
			}
		})
	}
}
