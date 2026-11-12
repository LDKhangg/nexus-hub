package githubtools

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

type dangerTool struct {
	base
	san     tools.Sanitizer
	allowed []string
	run     func(context.Context, *dangerTool, tools.Call, input) (tools.ToolOutput, error)
}

func (t *dangerTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	in, err := t.prepare(c, t.allowed...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if in.Comment != "" && !validText(in.Comment, maxCommentRunes) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	return t.run(ctx, t, c, in)
}

// NewDangerousTools returns github_pr_merge, github_pr_close and
// github_issue_close. They only run when dangerous tools are enabled.
func NewDangerousTools(api API, s tools.Sanitizer) []tools.Tool {
	mk := func(name, desc, props string, req []string, allowed []string, fn func(context.Context, *dangerTool, tools.Call, input) (tools.ToolOutput, error)) tools.Tool {
		return &dangerTool{base: base{api: api, spec: tools.ToolSpec{Name: name, Description: desc, InputSchema: schema(props, req...), Tier: tenant.TierDangerous}}, san: s, allowed: allowed, run: fn}
	}
	comment := `,"comment":{"type":"string","maxLength":4000}`
	return []tools.Tool{
		mk("github_pr_merge", "Merge an open pull request whose head is exactly head_sha, into an allowed base branch, with an allowed method, once its checks are green.",
			`,"head_sha":{"type":"string","pattern":"^[0-9a-f]{7,40}$"},"merge_method":{"type":"string","enum":["merge","squash","rebase"]}`,
			[]string{"head_sha", "merge_method"}, []string{"head_sha", "merge_method"}, merge),
		mk("github_pr_close", "Close an open pull request without merging, optionally posting a comment first.", comment, nil, []string{"comment"}, closePR),
		mk("github_issue_close", "Close an issue as completed or not_planned, optionally posting a comment first.",
			comment+`,"reason":{"type":"string","enum":["completed","not_planned"]}`, nil, []string{"comment", "reason"}, closeIssue),
	}
}

func merge(ctx context.Context, t *dangerTool, c tools.Call, in input) (tools.ToolOutput, error) {
	if !shaRe.MatchString(in.HeadSHA) || !slices.Contains([]string{"merge", "squash", "rebase"}, in.MergeMethod) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	p, err := t.getPull(ctx, c, in, permsMerge)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	switch {
	case p.State != "open" || p.Merged:
		return tools.ToolOutput{}, toolErr("pr_not_open")
	case p.Draft:
		return tools.ToolOutput{}, toolErr("pr_draft")
	case p.Head.SHA == "" || !strings.EqualFold(p.Head.SHA, in.HeadSHA):
		return tools.ToolOutput{}, toolErr("head_sha_mismatch")
	case !slices.Contains(c.Repo.MergeBaseBranches, p.Base.Ref): // never an implicit main/master
		return tools.ToolOutput{}, deniedErr("base_not_allowed")
	case !slices.Contains(c.Repo.MergeMethods, in.MergeMethod):
		return tools.ToolOutput{}, deniedErr("merge_method_not_allowed")
	}
	if c.Repo.RequireChecksGreen {
		s, err := t.checks(ctx, c, in, permsMerge, p.Head.SHA)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		switch {
		case len(s.failed) > 0 || s.combined == "failure" || s.combined == "error":
			return tools.ToolOutput{}, toolErr("checks_failing")
		case s.incomplete: // unread pages could hold a failing check
			return tools.ToolOutput{}, toolErr("checks_unverified")
		case len(s.pending) > 0 || s.combined == "pending":
			return tools.ToolOutput{}, toolErr("checks_pending")
		}
	}
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	// sha makes GitHub refuse the merge if the head moved after our checks.
	body := map[string]string{"sha": p.Head.SHA, "merge_method": in.MergeMethod}
	if err := t.do(ctx, c, in, permsMerge, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", in.Repo, in.num()), body, &out); err != nil {
		return tools.ToolOutput{}, err
	}
	if !out.Merged {
		return tools.ToolOutput{}, toolErr("not_merged")
	}
	return tools.ToolOutput{Text: fmt.Sprintf("merged #%d into %s (%s) as %s", p.Number, p.Base.Ref, in.MergeMethod, out.SHA), Summary: fmt.Sprintf("merged #%d %s", p.Number, short(out.SHA))}, nil
}

func (t *dangerTool) comment(ctx context.Context, c tools.Call, in input) error {
	if in.Comment == "" {
		return nil
	}
	return t.do(ctx, c, in, permsClose, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", in.Repo, in.num()), map[string]string{"body": botBody(t.san, in.Comment)}, nil)
}

func closePR(ctx context.Context, t *dangerTool, c tools.Call, in input) (tools.ToolOutput, error) {
	p, err := t.getPull(ctx, c, in, permsClose)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if p.State != "open" || p.Merged {
		return tools.ToolOutput{}, toolErr("pr_not_open")
	}
	if err := t.comment(ctx, c, in); err != nil {
		return tools.ToolOutput{}, err
	}
	if err := t.do(ctx, c, in, permsClose, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", in.Repo, in.num()), map[string]string{"state": "closed"}, nil); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: fmt.Sprintf("closed PR #%d", in.num()), Summary: fmt.Sprintf("closed PR #%d", in.num())}, nil
}

func closeIssue(ctx context.Context, t *dangerTool, c tools.Call, in input) (tools.ToolOutput, error) {
	reason := in.Reason
	if reason == "" {
		reason = "completed"
	}
	if reason != "completed" && reason != "not_planned" {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	if err := t.comment(ctx, c, in); err != nil {
		return tools.ToolOutput{}, err
	}
	if err := t.do(ctx, c, in, permsClose, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", in.Repo, in.num()), map[string]string{"state": "closed", "state_reason": reason}, nil); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: fmt.Sprintf("closed issue #%d (%s)", in.num(), reason), Summary: fmt.Sprintf("closed issue #%d %s", in.num(), reason)}, nil
}
