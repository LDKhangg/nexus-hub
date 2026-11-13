package storage

import (
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

// P3-DEBT-01 (REQ-21 / Phase 2 REQ-6; R16): a successful comment followed by
// a definite close rejection is still a partial side effect. A fresh executor
// must not repeat that comment or forget its consumed dangerous/write budget.
func TestS23PartialCloseFailureNeverRepeatsAcceptedComment(t *testing.T) {
	ctx, _, store, job := newToolRunDB(t)
	var comments, closes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			io.WriteString(w, `{"number":7,"state":"open","merged":false,"head":{"sha":"abcdef1234"},"base":{"ref":"develop"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			comments.Add(1)
			io.WriteString(w, `{"id":123}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/pulls/7":
			closes.Add(1)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"definite fake rejection"}`)
		default:
			t.Error("unexpected fake GitHub API request")
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	red := agent.NewRedactor(nil)
	api := githubapi.NewClientAt(srv.URL, srv.Client(), approvedCredentialSource{})
	reg, err := tools.NewRegistry(githubtools.NewDangerousTools(api, red)...)
	if err != nil {
		t.Fatal(err)
	}
	policy := tenant.ToolPolicyConfig{Enabled: []string{"github_pr_close"}, MaxWritesPerJob: 3, Accounts: map[string]tenant.GitHubAccount{"p": {Name: "p"}}, Repos: map[string]tenant.RepoGrant{"o/r": {Repo: "o/r", Account: "p", Allow: []string{"github_pr_close"}}}}
	input := json.RawMessage(`{"repo":"o/r","number":7,"comment":"Closing explanation"}`)
	fresh := func() *tools.Executor {
		return tools.NewExecutor(reg, policy, tools.DefaultLimits(), true, store, red, job, workspaceA)
	}
	a := fresh().Call(ctx, 1, "github_pr_close", input)
	if comments.Load() != 1 || closes.Load() != 1 {
		t.Fatal("fixture did not reach partial-success boundary")
	}
	b := fresh().Call(ctx, 2, "github_pr_close", input)
	if comments.Load() != 1 {
		t.Errorf("R16: accepted comment repeated after close failure; effects=%d", comments.Load())
	}
	if a.Status != tools.StatusUnknownOutcome || b.Status != tools.StatusUnknownOutcome {
		t.Errorf("R16: partial effect lost its non-replayable audit; first=%s retry=%s", a.Status, b.Status)
	}
	writes, dangerous, err := store.JobCounts(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if writes < 1 || dangerous < 1 {
		t.Errorf("R16: partial side effect omitted from durable budgets: writes=%d dangerous=%d", writes, dangerous)
	}
}
