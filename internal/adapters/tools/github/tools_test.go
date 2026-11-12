package githubtools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const prJSON = `{"number":7,"title":"Fix login","state":"open","draft":false,"user":{"login":"alice"},
 "base":{"ref":"develop"},"head":{"ref":"nexus/fix","sha":"abc1234def"},"mergeable":true,"mergeable_state":"clean",
 "additions":10,"deletions":2,"changed_files":3,"comments":1,"html_url":"https://github.com/o/r/pull/7","body":"BODY"}`

func TestPRView_CompactOutput(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"GET /repos/o/r/pulls/7": strings.Replace(prJSON, "BODY", strings.Repeat("x", 5000), 1)}}
	out, err := run(t, find(t, NewReadTools(api), "github_pr_view"), grant(nil), `{"repo":"o/r","number":7}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#7 Fix login", "open", "alice", "develop <- nexus/fix", "abc1234def", "+10 -2", "3 files", "mergeable: clean"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("missing %q:\n%s", want, out.Text)
		}
	}
	if len([]rune(out.Text)) > 2500 || !strings.Contains(out.Text, "…") || strings.Contains(out.Text, `"user"`) {
		t.Fatalf("not compact (%d):\n%s", len(out.Text), out.Text)
	}
	if api.calls[0].Perms["pull_requests"] != "read" || api.calls[0].Perms["contents"] != "read" {
		t.Fatalf("perms %v", api.calls[0].Perms)
	}
}

func TestPRChecks_Summary(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{
		"GET /repos/o/r/pulls/7": prJSON,
		"GET /repos/o/r/commits/abc1234def/check-runs?per_page=100": `{"total_count":4,"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"lint","status":"completed","conclusion":"failure"},
			{"name":"e2e","status":"in_progress","conclusion":null},
			{"name":"docs","status":"completed","conclusion":"skipped"}]}`,
		"GET /repos/o/r/commits/abc1234def/status": `{"state":"success","statuses":[{"context":"ci/legacy","state":"success"}]}`,
	}}
	out, err := run(t, find(t, NewReadTools(api), "github_pr_checks"), grant(nil), `{"repo":"o/r","number":7}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"abc1234", "1 passed", "1 failed", "1 pending", "1 skipped", "failed: lint", "pending: e2e", "combined status: success"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("missing %q:\n%s", want, out.Text)
		}
	}
}

func TestPRFiles_PatchClipped(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"GET /repos/o/r/pulls/7/files?per_page=100": `[
		{"filename":"a.go","status":"modified","additions":3,"deletions":1,"patch":"` + strings.Repeat("+line\\n", 400) + `"},
		{"filename":"img.png","status":"added","additions":0,"deletions":0}]`}}
	out, err := run(t, find(t, NewReadTools(api), "github_pr_files"), grant(nil), `{"repo":"o/r","number":7}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "a.go (modified +3 -1)") || !strings.Contains(out.Text, "img.png (added +0 -0)") || !strings.Contains(out.Text, "[patch truncated]") {
		t.Fatalf("%s", out.Text)
	}
	if len(out.Text) > 6144 {
		t.Fatalf("too long %d", len(out.Text))
	}
}

func TestIssueView_BodyClipped(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"GET /repos/o/r/issues/9": `{"number":9,"title":"Crash","state":"open","user":{"login":"bob"},"labels":[{"name":"bug"},{"name":"p1"}],"comments":4,"body":"` + strings.Repeat("y", 5000) + `"}`}}
	out, err := run(t, find(t, NewReadTools(api), "github_issue_view"), grant(nil), `{"repo":"o/r","number":9}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "#9 Crash") || !strings.Contains(out.Text, "labels: bug, p1") || !strings.Contains(out.Text, "bob") || len([]rune(out.Text)) > 2500 {
		t.Fatalf("%s", out.Text)
	}
}

func TestPRComment_PrefixedAndNeutralized(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"POST /repos/o/r/issues/7/comments": `{"id":555,"html_url":"https://github.com/o/r/pull/7#issuecomment-555"}`}}
	tl := find(t, NewWriteTools(api, san{}), "github_pr_comment")
	if tl.Spec().Tier != tenant.TierWrite {
		t.Fatal("tier")
	}
	out, err := run(t, tl, grant(nil), `{"repo":"o/r","number":7,"body":"ping @alice please"}`)
	if err != nil {
		t.Fatal(err)
	}
	body := api.calls[0].Body.(map[string]string)["body"]
	if !strings.HasPrefix(body, "🤖 ") || strings.Contains(body, " @alice") || !strings.Contains(body, "`@`alice") {
		t.Fatalf("body %q", body)
	}
	if p := api.calls[0].Perms; p["issues"] != "write" || p["pull_requests"] != "write" || len(p) != 2 {
		t.Fatalf("perms %v", p)
	}
	if !strings.Contains(out.Summary, "555") {
		t.Fatalf("summary %q", out.Summary)
	}
}

func TestGitHubTools_InputValidation(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{}}
	all := append(NewReadTools(api), NewWriteTools(api, san{})...)
	bad := []string{`{}`, `{"repo":"o/r"}`, `{"repo":"o/r","number":0}`, `{"repo":"o/r","number":-1}`, `{"repo":"o/r","number":1.5}`,
		`{"repo":"bad repo","number":1}`, `{"repo":"o/r","number":1,"extra":true}`, `[1]`}
	for _, tl := range all {
		var _ tools.RepoScoped = tl.(tools.RepoScoped)
		var schema map[string]any
		if err := json.Unmarshal(tl.Spec().InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", tl.Spec().Name, err)
		}
		for _, in := range bad {
			if _, err := run(t, tl, grant(nil), in); code(err) != "invalid_input" {
				t.Errorf("%s %s: %v", tl.Spec().Name, in, err)
			}
		}
	}
	comment := find(t, all, "github_pr_comment")
	for _, in := range []string{`{"repo":"o/r","number":1,"body":""}`, `{"repo":"o/r","number":1,"body":"` + strings.Repeat("z", 4001) + `"}`} {
		if _, err := run(t, comment, grant(nil), in); code(err) != "invalid_input" {
			t.Errorf("comment %d: %v", len(in), err)
		}
	}
	if len(api.calls) != 0 {
		t.Fatalf("API called on invalid input: %v", api.calls)
	}
	// Repo of the call must match the grant the policy resolved.
	if _, err := run(t, find(t, all, "github_pr_view"), grant(func(g *tenant.RepoGrant) { g.Repo = "o/other" }), `{"repo":"o/r","number":1}`); code(err) != "repo_mismatch" {
		t.Fatalf("mismatch: %v", err)
	}
}

func TestGitHubTools_UsesGrantAccount(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"GET /repos/o/r/issues/9": `{"number":9,"title":"t","state":"open","user":{"login":"b"}}`}}
	tl := find(t, NewReadTools(api), "github_issue_view")
	if _, err := run(t, tl, grant(nil), `{"repo":"o/r","number":9}`); err != nil {
		t.Fatal(err)
	}
	if c := api.calls[0]; c.Account != "personal" || c.Repo != "o/r" || c.WS != "ws" {
		t.Fatalf("%+v", c)
	}
	if _, err := tl.Run(t.Context(), tools.Call{Input: json.RawMessage(`{"repo":"o/r","number":9}`), Repo: grant(nil), WorkspaceID: "ws"}); code(err) != "no_account" {
		t.Fatalf("no account: %v", err)
	}
	api.resp["GET /repos/o/r/issues/9"] = &github.GitHubError{Code: "rate_limited", Retryable: true}
	_, err := run(t, tl, grant(nil), `{"repo":"o/r","number":9}`)
	if te, ok := err.(*tools.ToolError); !ok || te.Code != "rate_limited" || !te.Retryable {
		t.Fatalf("mapped %v", err)
	}
}
