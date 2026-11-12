package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type approvedCredentialSource struct{}

func (approvedCredentialSource) Token(context.Context, string, tenant.GitHubAccount, string, map[string]string) (string, error) {
	return "fake-http-effect-token", nil
}

// TC-13 (REQ-6), TC-14 (REQ-F9): the fake server accepts the actual POST,
// then loses its response/returns 5xx/bad JSON. Real client + tool + executor
// + durable store must not repeat the remote effect after a fresh attempt.
func TestApproved_RemoteAcceptedUncertainCommentNeverRepeats(t *testing.T) {
	for _, mode := range []string{"success", "lost_response", "server_error", "invalid_json", "definite_rejection"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool, store, job := newToolRunDB(t)
			var effects atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/repos/o/r/issues/7/comments" {
					t.Error("unexpected API request")
					w.WriteHeader(404)
					return
				}
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_runs WHERE agent_job_id=$1 AND status='started'`, job).Scan(&n); err != nil || n != 1 {
					t.Error("POST reached server before durable started row")
				}
				if mode == "definite_rejection" {
					w.WriteHeader(403)
					io.WriteString(w, `{"message":"forbidden"}`)
					return
				}
				effects.Add(1)
				switch mode {
				case "lost_response":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error("hijack failed")
						return
					}
					conn.Close()
				case "server_error":
					w.WriteHeader(500)
					io.WriteString(w, `{"message":"fake private upstream body"}`)
				case "invalid_json":
					io.WriteString(w, `{"id":`)
				default:
					io.WriteString(w, `{"id":123,"html_url":"https://example.invalid/comment/123"}`)
				}
			}))
			defer srv.Close()
			red := agent.NewRedactor(nil)
			api := githubapi.NewClientAt(srv.URL, srv.Client(), approvedCredentialSource{})
			reg, err := tools.NewRegistry(githubtools.NewWriteTools(api, red)...)
			if err != nil {
				t.Fatal(err)
			}
			p := tenant.ToolPolicyConfig{MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"p": {Name: "p"}}, Repos: map[string]tenant.RepoGrant{"o/r": {Repo: "o/r", Account: "p", Allow: []string{"github_pr_comment"}}}}
			fresh := func() *tools.Executor {
				return tools.NewExecutor(reg, p, tools.DefaultLimits(), false, store, red, job, workspaceA)
			}
			a := fresh().Call(ctx, 1, "github_pr_comment", json.RawMessage(`{"repo":"o/r","number":7,"body":"Báo cáo an toàn 🧠"}`))
			b := fresh().Call(ctx, 2, "github_pr_comment", json.RawMessage(`{ "body":"Báo cáo an toàn 🧠", "number":7, "repo":"o/r" }`))
			want := tools.StatusUnknownOutcome
			if mode == "success" {
				want = tools.StatusOK
			}
			if mode == "definite_rejection" {
				want = tools.StatusError
			}
			if a.Status != want || b.Status != want {
				t.Errorf("status first=%s retry=%s want=%s", a.Status, b.Status, want)
			}
			wantEffects := int32(1)
			if mode == "definite_rejection" {
				wantEffects = 0
			}
			if effects.Load() != wantEffects {
				t.Errorf("remote effects=%d want=%d", effects.Load(), wantEffects)
			}
		})
	}
}
