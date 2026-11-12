// Package githubtools exposes GitHub pull requests and issues to the agent
// as compact-text tools. Every call runs as the account of the repo grant
// the policy resolved, with a down-scoped App token where applicable.
package githubtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// API is the GitHub client surface the tools use (*github.Client).
type API interface {
	Do(ctx context.Context, workspaceID string, account tenant.GitHubAccount, repo string, perms map[string]string, method, path string, body, out any) error
}

// App permission sets.
var (
	permsRead    = map[string]string{"pull_requests": "read", "checks": "read", "statuses": "read", "issues": "read", "contents": "read"}
	permsComment = map[string]string{"issues": "write", "pull_requests": "write"}
	permsMerge   = map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"}
	permsClose   = map[string]string{"issues": "write", "pull_requests": "write"}
)

const (
	maxBodyRunes    = 1500
	maxCommentRunes = 4000
)

type input struct {
	Repo        string  `json:"repo"`
	Number      float64 `json:"number"`
	Body        string  `json:"body"`
	HeadSHA     string  `json:"head_sha"`
	MergeMethod string  `json:"merge_method"`
	Comment     string  `json:"comment"`
	Reason      string  `json:"reason"`
}

func toolErr(code string) error { return &tools.ToolError{Code: code} }

// decode validates the common {repo, number} shape and rejects unknown keys.
func decode(raw json.RawMessage, allowed ...string) (input, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return input{}, toolErr("invalid_input")
	}
	ok := map[string]bool{"repo": true, "number": true}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range probe {
		if !ok[k] {
			return input{}, toolErr("invalid_input")
		}
	}
	var in input
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&in); err != nil {
		return input{}, toolErr("invalid_input")
	}
	if !tenant.ValidRepo(in.Repo) || in.Number < 1 || in.Number > 1e9 || in.Number != math.Trunc(in.Number) {
		return input{}, toolErr("invalid_input")
	}
	return in, nil
}

func (in input) num() int64 { return int64(in.Number) }

func repoOf(raw json.RawMessage) (string, error) {
	var v struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || !tenant.ValidRepo(v.Repo) {
		return "", toolErr("invalid_input")
	}
	return v.Repo, nil
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(append([]string{"repo", "number"}, required...))
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"repo":{"type":"string","description":"owner/name"},"number":{"type":"integer","minimum":1}` + props + `},"required":` + string(req) + `}`)
}

// base carries what every GitHub tool shares.
type base struct {
	api  API
	spec tools.ToolSpec
}

func (b *base) Spec() tools.ToolSpec                     { return b.spec }
func (b *base) Repo(raw json.RawMessage) (string, error) { return repoOf(raw) }
func (b *base) UsesNetwork() bool                        { return true }

// prepare decodes input and checks it against the resolved grant.
func (b *base) prepare(c tools.Call, allowed ...string) (input, error) {
	in, err := decode(c.Input, allowed...)
	if err != nil {
		return in, err
	}
	if c.Repo == nil || !strings.EqualFold(c.Repo.Repo, in.Repo) {
		return in, toolErr("repo_mismatch")
	}
	if c.Account == nil {
		return in, toolErr("no_account")
	}
	return in, nil
}

func (b *base) do(ctx context.Context, c tools.Call, in input, perms map[string]string, method, path string, body, out any) error {
	err := b.api.Do(ctx, c.WorkspaceID, *c.Account, in.Repo, perms, method, path, body, out)
	var ge *github.GitHubError
	if errors.As(err, &ge) {
		return &tools.ToolError{Code: ge.Code, Retryable: ge.Retryable}
	}
	return err
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// pull is the subset of a pull request the tools use.
type pull struct {
	Number         int64                  `json:"number"`
	Title          string                 `json:"title"`
	State          string                 `json:"state"`
	Draft          bool                   `json:"draft"`
	Merged         bool                   `json:"merged"`
	User           struct{ Login string } `json:"user"`
	Base           struct{ Ref string }   `json:"base"`
	Head           struct{ Ref, SHA string }
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changed_files"`
	Comments       int    `json:"comments"`
	HTMLURL        string `json:"html_url"`
	Body           string `json:"body"`
}

func (b *base) getPull(ctx context.Context, c tools.Call, in input, perms map[string]string) (pull, error) {
	var p pull
	err := b.do(ctx, c, in, perms, "GET", fmt.Sprintf("/repos/%s/pulls/%d", in.Repo, in.num()), nil, &p)
	return p, err
}
