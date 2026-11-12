package githubtools

import (
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const greenRuns = `{"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`

func mergeAPI(pr, runs, status string) *fakeAPI {
	return &fakeAPI{resp: map[string]any{
		"GET /repos/o/r/pulls/7":                                    pr,
		"GET /repos/o/r/commits/abc1234def/check-runs?per_page=100": runs,
		"GET /repos/o/r/commits/abc1234def/status":                  status,
		"PUT /repos/o/r/pulls/7/merge":                              `{"merged":true,"sha":"mergesha99"}`,
	}}
}

const mergeIn = `{"repo":"o/r","number":7,"head_sha":"abc1234def","merge_method":"squash"}`

func TestMerge_HappyPath(t *testing.T) {
	api := mergeAPI(prJSON, greenRuns, `{"state":"pending","statuses":[]}`)
	tl := find(t, NewDangerousTools(api, san{}), "github_pr_merge")
	if tl.Spec().Tier != tenant.TierDangerous {
		t.Fatal("tier")
	}
	out, err := run(t, tl, grant(nil), mergeIn)
	if err != nil {
		t.Fatal(err)
	}
	if api.count("PUT /repos/o/r/pulls/7/merge") != 1 || !strings.Contains(out.Text, "mergesha99") {
		t.Fatalf("%v %s", api.calls, out.Text)
	}
	put := api.calls[len(api.calls)-1]
	body := put.Body.(map[string]string)
	if body["sha"] != "abc1234def" || body["merge_method"] != "squash" || put.Perms["contents"] != "write" {
		t.Fatalf("%+v", put)
	}
}

func TestMerge_Preconditions(t *testing.T) {
	cases := []struct {
		name, pr, runs, status, in string
		g                          func(*tenant.RepoGrant)
		want                       string
	}{
		{"draft", strings.Replace(prJSON, `"draft":false`, `"draft":true`, 1), greenRuns, `{"state":"success","statuses":[]}`, mergeIn, nil, "pr_draft"},
		{"closed", strings.Replace(prJSON, `"state":"open"`, `"state":"closed"`, 1), greenRuns, `{}`, mergeIn, nil, "pr_not_open"},
		{"sha_mismatch", prJSON, greenRuns, `{}`, strings.Replace(mergeIn, "abc1234def", "fff0000aaa", 1), nil, "head_sha_mismatch"},
		// TC-12 (REQ-5): the owner requires an exact SHA, not a prefix.
		{"sha_prefix_only", prJSON, greenRuns, `{"state":"success","statuses":[]}`, strings.Replace(mergeIn, "abc1234def", "abc1234", 1), nil, "head_sha_mismatch"},
		{"base_not_listed", strings.Replace(prJSON, `"ref":"develop"`, `"ref":"release"`, 1), greenRuns, `{}`, mergeIn, nil, "base_not_allowed"},
		{"main_empty_list", strings.Replace(prJSON, `"ref":"develop"`, `"ref":"main"`, 1), greenRuns, `{}`, mergeIn, func(g *tenant.RepoGrant) { g.MergeBaseBranches = nil }, "base_not_allowed"},
		{"method", prJSON, greenRuns, `{}`, strings.Replace(mergeIn, "squash", "rebase", 1), nil, "merge_method_not_allowed"},
		{"failing_check", prJSON, `{"check_runs":[{"name":"lint","status":"completed","conclusion":"failure"}]}`, `{"state":"success","statuses":[]}`, mergeIn, nil, "checks_failing"},
		{"pending_check", prJSON, `{"check_runs":[{"name":"e2e","status":"queued","conclusion":null}]}`, `{"state":"success","statuses":[]}`, mergeIn, nil, "checks_pending"},
		{"status_failure", prJSON, greenRuns, `{"state":"failure","statuses":[{"context":"ci","state":"failure"}]}`, mergeIn, nil, "checks_failing"},
	}
	for _, tc := range cases {
		api := mergeAPI(tc.pr, tc.runs, tc.status)
		_, err := run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(tc.g), tc.in)
		if code(err) != tc.want {
			t.Errorf("%s: got %v", tc.name, err)
		}
		if api.count("PUT") != 0 {
			t.Errorf("%s: merged anyway", tc.name)
		}
	}
	// Short or malformed SHAs are rejected before any call.
	api := mergeAPI(prJSON, greenRuns, `{}`)
	if _, err := run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(nil), strings.Replace(mergeIn, "abc1234def", "abc", 1)); code(err) != "invalid_input" || len(api.calls) != 0 {
		t.Fatalf("short sha: %v", err)
	}
}

func TestMerge_RequireChecksGreenFalse(t *testing.T) {
	api := mergeAPI(prJSON, `{"check_runs":[{"name":"lint","status":"completed","conclusion":"failure"}]}`, `{"state":"failure","statuses":[{"context":"c","state":"failure"}]}`)
	if _, err := run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(func(g *tenant.RepoGrant) { g.RequireChecksGreen = false }), mergeIn); err != nil {
		t.Fatal(err)
	}
	if api.count("GET /repos/o/r/commits") != 0 || api.count("PUT") != 1 {
		t.Fatalf("%v", api.calls)
	}
}

func TestMerge_Conflict409(t *testing.T) {
	api := mergeAPI(prJSON, greenRuns, `{}`)
	api.resp["PUT /repos/o/r/pulls/7/merge"] = &github.GitHubError{Code: "conflict"}
	if _, err := run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(nil), mergeIn); code(err) != "conflict" {
		t.Fatalf("%v", err)
	}
}

func TestClosePR_WithComment(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{
		"GET /repos/o/r/pulls/7":            prJSON,
		"POST /repos/o/r/issues/7/comments": `{"id":1}`,
		"PATCH /repos/o/r/pulls/7":          `{"state":"closed"}`,
	}}
	tl := find(t, NewDangerousTools(api, san{}), "github_pr_close")
	if _, err := run(t, tl, grant(nil), `{"repo":"o/r","number":7,"comment":"superseded by @bob's PR"}`); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range api.calls {
		order = append(order, c.Method)
	}
	if strings.Join(order, ",") != "GET,POST,PATCH" {
		t.Fatalf("order %v", order)
	}
	if b := api.calls[1].Body.(map[string]string)["body"]; !strings.HasPrefix(b, BotPrefix) || strings.Contains(b, " @bob") {
		t.Fatalf("comment %q", b)
	}
	if b := api.calls[2].Body.(map[string]string); b["state"] != "closed" {
		t.Fatalf("patch %v", b)
	}
	// Already closed: no writes.
	api2 := &fakeAPI{resp: map[string]any{"GET /repos/o/r/pulls/7": strings.Replace(prJSON, `"state":"open"`, `"state":"closed"`, 1)}}
	if _, err := run(t, find(t, NewDangerousTools(api2, san{}), "github_pr_close"), grant(nil), `{"repo":"o/r","number":7}`); code(err) != "pr_not_open" || len(api2.calls) != 1 {
		t.Fatalf("closed: %v", err)
	}
}

func TestCloseIssue_Reason(t *testing.T) {
	api := &fakeAPI{resp: map[string]any{"PATCH /repos/o/r/issues/9": `{"state":"closed"}`}}
	tl := find(t, NewDangerousTools(api, san{}), "github_issue_close")
	if _, err := run(t, tl, grant(nil), `{"repo":"o/r","number":9,"reason":"not_planned"}`); err != nil {
		t.Fatal(err)
	}
	if b := api.calls[0].Body.(map[string]string); b["state"] != "closed" || b["state_reason"] != "not_planned" {
		t.Fatalf("%v", b)
	}
	if _, err := run(t, tl, grant(nil), `{"repo":"o/r","number":9}`); err != nil {
		t.Fatal(err)
	}
	if b := api.calls[1].Body.(map[string]string); b["state_reason"] != "completed" {
		t.Fatalf("default reason %v", b)
	}
	if _, err := run(t, tl, grant(nil), `{"repo":"o/r","number":9,"reason":"wontfix"}`); code(err) != "invalid_input" {
		t.Fatalf("bad reason %v", err)
	}
}
