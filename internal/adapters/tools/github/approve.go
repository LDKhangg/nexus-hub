package githubtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// ToolPRApprove is the name of the pull request approval tool.
const ToolPRApprove = "github_pr_approve"

// Outcomes of github_pr_approve, in its output ("outcome=<x>").
const (
	// OutcomeApproved: a new APPROVE review was posted.
	OutcomeApproved = "approved"
	// OutcomeAlreadyApproved: the account's latest review is already an
	// approval; nothing was posted.
	OutcomeAlreadyApproved = "already_approved"
)

// Failure codes of github_pr_approve, besides the shared client codes.
const (
	CodePRNotFound         = "pr_not_found"
	CodePRClosed           = "pr_closed"
	CodeOwnPR              = "own_pr"
	CodeInvalidState       = "invalid_state"
	CodeGitHubUnauthorized = "github_unauthorized"
	CodeAccountNotFound    = "account_not_found"
	CodeAccountNotPAT      = "account_not_pat"
)

const (
	maxReviewPages = 10
	reviewsPerPage = 100
)

// permsApprove is the App permission set of an approval. Approvals run only
// as a pat account, which ignores it; it is set for completeness.
var permsApprove = map[string]string{"pull_requests": "write"}

// approveTool is github_pr_approve: it approves one pull request as a hub
// GitHub account of type pat, on any repository that account can reach.
// It is AccountScoped (no repo grant) and ActionOnly (only the behavior
// engine's run-level action executor may run it).
type approveTool struct {
	api  API
	spec tools.ToolSpec
}

var (
	_ tools.AccountScoped = (*approveTool)(nil)
	_ tools.ActionOnly    = (*approveTool)(nil)
)

// NewApproveTool returns github_pr_approve. Input:
// {"account":"<hub account name>","repo":"owner/name","number":<n>}.
// Its output names the pull request, the outcome and the review id:
// "pr=owner/name#n outcome=approved|already_approved review_id=<id>".
func NewApproveTool(api API) tools.Tool {
	return &approveTool{api: api, spec: tools.ToolSpec{
		Name: ToolPRApprove,
		Description: "Approve an open pull request as the named hub GitHub account (type pat), with no review text. " +
			"Does nothing when that account's latest review is already an approval.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
			`"account":{"type":"string","minLength":1,"maxLength":63},` +
			`"repo":{"type":"string","description":"owner/name"},"number":{"type":"integer","minimum":1}},` +
			`"required":["account","repo","number"]}`),
		Tier: tenant.TierWrite,
	}}
}

func (t *approveTool) Spec() tools.ToolSpec { return t.spec }
func (t *approveTool) UsesNetwork() bool    { return true }
func (t *approveTool) ActionOnly()          {}

type approveInput struct {
	Account string  `json:"account"`
	Repo    string  `json:"repo"`
	Number  float64 `json:"number"`
}

func decodeApprove(raw json.RawMessage) (approveInput, error) {
	var in approveInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, toolErr("invalid_input")
	}
	if !tenant.ValidCredentialRef(in.Account) || !tenant.ValidRepo(in.Repo) ||
		in.Number < 1 || in.Number > 1e9 || in.Number != math.Trunc(in.Number) {
		return in, toolErr("invalid_input")
	}
	return in, nil
}

// Target implements tools.AccountScoped.
func (t *approveTool) Target(raw json.RawMessage) (string, string, error) {
	in, err := decodeApprove(raw)
	if err != nil {
		return "", "", err
	}
	return in.Account, in.Repo, nil
}

// review is the subset of a pull request review the tool reads.
type review struct {
	ID    int64                  `json:"id"`
	State string                 `json:"state"`
	User  struct{ Login string } `json:"user"`
}

func (t *approveTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	in, err := decodeApprove(c.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	switch {
	case c.Account == nil || c.Account.Name != in.Account:
		return tools.ToolOutput{}, deniedErr(CodeAccountNotFound)
	case c.Account.Type != "pat":
		// An App account would approve as a bot, not as the owner.
		return tools.ToolOutput{}, deniedErr(CodeAccountNotPAT)
	case strings.TrimSpace(c.Account.Login) == "":
		return tools.ToolOutput{}, deniedErr(CodeAccountNotFound)
	}
	// The pat credential source verifies that the token belongs to this
	// login before every use, so it is the token's own identity.
	login := c.Account.Login
	n := int64(in.Number)
	ref := fmt.Sprintf("%s#%d", in.Repo, n)

	var p pull
	if err := t.do(ctx, c, in.Repo, "GET", fmt.Sprintf("/repos/%s/pulls/%d", in.Repo, n), nil, &p, false); err != nil {
		return tools.ToolOutput{}, err
	}
	if p.State != "open" || p.Merged {
		return tools.ToolOutput{}, toolErr(CodePRClosed)
	}
	if strings.EqualFold(p.User.Login, login) {
		// GitHub refuses an author's approval of their own pull request.
		return tools.ToolOutput{}, toolErr(CodeOwnPR)
	}

	if id, ok, err := t.latestApproval(ctx, c, in.Repo, n, login); err != nil {
		return tools.ToolOutput{}, err
	} else if ok {
		text := fmt.Sprintf("pr=%s outcome=%s review_id=%d", ref, OutcomeAlreadyApproved, id)
		return tools.ToolOutput{Text: text, Summary: text}, nil
	}

	var out review
	if err := t.do(ctx, c, in.Repo, "POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", in.Repo, n), map[string]string{"event": "APPROVE"}, &out, true); err != nil {
		return tools.ToolOutput{}, err
	}
	text := fmt.Sprintf("pr=%s outcome=%s review_id=%d", ref, OutcomeApproved, out.ID)
	return tools.ToolOutput{Text: text, Summary: text}, nil
}

// latestApproval pages through the pull request's reviews (at most
// maxReviewPages pages) and reports whether login's latest effective review
// is an approval, with its id. COMMENTED and PENDING reviews do not change
// a reviewer's state on GitHub, so they are skipped. When the pages run
// out before the list ends the answer is "not approved": a second approval
// is harmless, a missed one is not.
func (t *approveTool) latestApproval(ctx context.Context, c tools.Call, repo string, n int64, login string) (int64, bool, error) {
	var latest *review
	for page := 1; page <= maxReviewPages; page++ {
		q := url.Values{}
		q.Set("per_page", fmt.Sprint(reviewsPerPage))
		q.Set("page", fmt.Sprint(page))
		var rs []review
		if err := t.do(ctx, c, repo, "GET", fmt.Sprintf("/repos/%s/pulls/%d/reviews?%s", repo, n, q.Encode()), nil, &rs, false); err != nil {
			return 0, false, err
		}
		for i := range rs {
			r := rs[i]
			if !strings.EqualFold(r.User.Login, login) {
				continue
			}
			switch r.State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				latest = &r
			}
		}
		if len(rs) < reviewsPerPage {
			break
		}
	}
	if latest != nil && latest.State == "APPROVED" {
		return latest.ID, true, nil
	}
	return 0, false, nil
}

// do calls GitHub as c.Account and maps client errors to tool errors. write
// marks the approval itself: a failure after it may have reached GitHub is
// ambiguous. Reads before it are definite failures.
func (t *approveTool) do(ctx context.Context, c tools.Call, repo, method, path string, body, out any, write bool) error {
	err := t.api.Do(ctx, c.WorkspaceID, *c.Account, repo, permsApprove, method, path, body, out)
	if err == nil {
		return nil
	}
	var ge *github.GitHubError
	if !errors.As(err, &ge) {
		return err
	}
	switch ge.Code {
	case "not_found":
		return toolErr(CodePRNotFound)
	case "unauthorized", "forbidden":
		return toolErr(CodeGitHubUnauthorized)
	case "conflict":
		if write {
			// The author check ran before posting, so this is another state
			// GitHub refuses (a draft, a pending review, a race with a close).
			return toolErr(CodeInvalidState)
		}
	}
	return &tools.ToolError{Code: ge.Code, Retryable: ge.Retryable, Denied: netguardDenied[ge.Code], Ambiguous: write && ambiguousCodes[ge.Code]}
}
