package githubtools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type apiCall struct {
	WS, Account, Repo, Method, Path string
	Perms                           map[string]string
	Body                            any
}

// fakeAPI answers by "METHOD path"; values are JSON text or *github.GitHubError.
type fakeAPI struct {
	mu    sync.Mutex
	resp  map[string]any
	calls []apiCall
}

func (f *fakeAPI) Do(_ context.Context, ws string, account tenant.GitHubAccount, repo string, perms map[string]string, method, path string, body, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, apiCall{ws, account.Name, repo, method, path, perms, body})
	r, ok := f.resp[method+" "+path]
	f.mu.Unlock()
	if !ok {
		return &github.GitHubError{Code: "not_found"}
	}
	if e, isErr := r.(*github.GitHubError); isErr {
		return e
	}
	if out != nil {
		return json.Unmarshal([]byte(r.(string)), out)
	}
	return nil
}

func (f *fakeAPI) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.Method+" "+c.Path, prefix) {
			n++
		}
	}
	return n
}

type san struct{}

func (san) CleanOutbound(s string, mentions bool) string {
	if mentions {
		return strings.ReplaceAll(s, "@", "`@`")
	}
	return s
}

var account = tenant.GitHubAccount{Name: "personal", Type: "pat"}

func grant(extra func(*tenant.RepoGrant)) *tenant.RepoGrant {
	g := &tenant.RepoGrant{Repo: "o/r", Account: "personal", MergeMethods: []string{"squash"}, MergeBaseBranches: []string{"develop"}, RequireChecksGreen: true}
	if extra != nil {
		extra(g)
	}
	return g
}

func find(t *testing.T, list []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range list {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s missing", name)
	return nil
}

func run(t *testing.T, tl tools.Tool, g *tenant.RepoGrant, input string) (tools.ToolOutput, error) {
	t.Helper()
	acc := account
	return tl.Run(context.Background(), tools.Call{Input: json.RawMessage(input), Repo: g, Account: &acc, WorkspaceID: "ws"})
}

func code(err error) string {
	if te, ok := err.(*tools.ToolError); ok {
		return te.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}
