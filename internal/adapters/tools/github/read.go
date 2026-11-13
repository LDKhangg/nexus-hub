package githubtools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const (
	maxFiles        = 100
	maxPatchPerFile = 1200
	maxPatchTotal   = 4500
	maxListedChecks = 20
)

type readTool struct {
	base
	run func(context.Context, *readTool, tools.Call, input) (tools.ToolOutput, error)
}

func (t *readTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	in, err := t.prepare(c)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	return t.run(ctx, t, c, in)
}

// NewReadTools returns github_pr_view, github_pr_checks, github_pr_files
// and github_issue_view.
func NewReadTools(api API, directories ...people.Resolver) []tools.Tool {
	var directory people.Resolver
	if len(directories) > 0 {
		directory = directories[0]
	}
	mk := func(name, desc string, fn func(context.Context, *readTool, tools.Call, input) (tools.ToolOutput, error)) tools.Tool {
		return &readTool{base: base{api: api, people: directory, spec: tools.ToolSpec{Name: name, Description: desc, InputSchema: schema(""), Tier: tenant.TierRead}}, run: fn}
	}
	return []tools.Tool{
		mk("github_pr_view", "Show a pull request: state, branches, head SHA, size, mergeability and a clipped description.", prView),
		mk("github_pr_checks", "Summarize CI check runs and the combined commit status of a pull request's head.", prChecks),
		mk("github_pr_files", "List files changed by a pull request with clipped patches.", prFiles),
		mk("github_issue_view", "Show an issue: state, author, labels and a clipped body.", issueView),
	}
}

func prView(ctx context.Context, t *readTool, c tools.Call, in input) (tools.ToolOutput, error) {
	p, err := t.getPull(ctx, c, in, permsRead)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	state := p.State
	if p.Merged {
		state = "merged"
	}
	if p.Draft {
		state += " (draft)"
	}
	mergeable := p.MergeableState
	if mergeable == "" {
		mergeable = "unknown"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "#%d %s\nstate: %s | author: %s\nbranches: %s <- %s\nhead: %s\nsize: +%d -%d, %d files, %d comments\nmergeable: %s\n%s\n",
		p.Number, p.Title, state, p.User.Login, p.Base.Ref, p.Head.Ref, p.Head.SHA, p.Additions, p.Deletions, p.ChangedFiles, p.Comments, mergeable, p.HTMLURL)
	if body := strings.TrimSpace(p.Body); body != "" {
		sb.WriteString("description:\n" + clipRunes(body, maxBodyRunes))
	}
	return tools.ToolOutput{Text: sb.String(), Summary: fmt.Sprintf("PR #%d %s", p.Number, state)}, nil
}

type checkRun struct {
	Name, Status string
	Conclusion   *string
}

type checksSummary struct {
	passed, failed, pending, skipped []string
	combined                         string
	// runsRead/runsTotal and statusesRead/statusesTotal count what was read
	// against what GitHub reported. incomplete is true when the page cap was
	// reached before every check run or status was read.
	runsRead, runsTotal         int
	statusesRead, statusesTotal int
	incomplete                  bool
}

const (
	checksPageSize   = 100 // check-runs per_page
	statusesPageSize = 30  // combined status default per_page
	maxCheckPages    = 10
)

func (b *base) checks(ctx context.Context, c tools.Call, in input, perms map[string]string, sha string) (checksSummary, error) {
	var s checksSummary
	var runs []checkRun
	runsTotal, runsMore := 0, false
	for page := 1; page <= maxCheckPages; page++ {
		var resp struct {
			TotalCount int        `json:"total_count"`
			CheckRuns  []checkRun `json:"check_runs"`
		}
		path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=%d", in.Repo, sha, checksPageSize)
		if page > 1 {
			path += fmt.Sprintf("&page=%d", page)
		}
		if err := b.do(ctx, c, in, perms, "GET", path, nil, &resp); err != nil {
			return checksSummary{}, err
		}
		runs = append(runs, resp.CheckRuns...)
		runsTotal = max(runsTotal, resp.TotalCount)
		runsMore = len(resp.CheckRuns) >= checksPageSize && (resp.TotalCount == 0 || len(runs) < resp.TotalCount)
		if !runsMore {
			break
		}
	}
	type commitStatus struct{ Context, State string }
	var statuses []commitStatus
	state, statusesTotal, statusesMore := "", 0, false
	for page := 1; page <= maxCheckPages; page++ {
		var resp struct {
			State      string         `json:"state"`
			TotalCount int            `json:"total_count"`
			Statuses   []commitStatus `json:"statuses"`
		}
		// The first page keeps the default page size; later pages ask for the
		// next page of the same size.
		path := fmt.Sprintf("/repos/%s/commits/%s/status", in.Repo, sha)
		if page > 1 {
			path += fmt.Sprintf("?page=%d", page)
		}
		if err := b.do(ctx, c, in, perms, "GET", path, nil, &resp); err != nil {
			return checksSummary{}, err
		}
		if page == 1 {
			state = resp.State
		}
		statuses = append(statuses, resp.Statuses...)
		statusesTotal = max(statusesTotal, resp.TotalCount)
		statusesMore = len(resp.Statuses) >= statusesPageSize && (resp.TotalCount == 0 || len(statuses) < resp.TotalCount)
		if !statusesMore {
			break
		}
	}
	s.runsRead, s.runsTotal = len(runs), max(runsTotal, len(runs))
	s.statusesRead, s.statusesTotal = len(statuses), max(statusesTotal, len(statuses))
	s.incomplete = runsMore || statusesMore || s.runsRead < s.runsTotal || s.statusesRead < s.statusesTotal
	for _, r := range runs {
		switch {
		case r.Status != "completed" || r.Conclusion == nil:
			s.pending = append(s.pending, r.Name)
		case *r.Conclusion == "success":
			s.passed = append(s.passed, r.Name)
		case *r.Conclusion == "skipped" || *r.Conclusion == "neutral":
			s.skipped = append(s.skipped, r.Name)
		default:
			s.failed = append(s.failed, r.Name)
		}
	}
	// Each legacy status counts on its own as well, so a failing status on a
	// later page blocks a merge even if the combined state disagrees.
	for _, st := range statuses {
		switch st.State {
		case "failure", "error":
			s.failed = append(s.failed, "status:"+st.Context)
		case "pending":
			s.pending = append(s.pending, "status:"+st.Context)
		}
	}
	// The combined status is "pending" when there are no legacy statuses.
	s.combined = state
	if len(statuses) == 0 && statusesTotal == 0 {
		s.combined = "none"
	}
	return s, nil
}

func listNames(names []string) string {
	sort.Strings(names)
	more := ""
	if len(names) > maxListedChecks {
		more = fmt.Sprintf(" (+%d more)", len(names)-maxListedChecks)
		names = names[:maxListedChecks]
	}
	return strings.Join(names, ", ") + more
}

func prChecks(ctx context.Context, t *readTool, c tools.Call, in input) (tools.ToolOutput, error) {
	p, err := t.getPull(ctx, c, in, permsRead)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	s, err := t.checks(ctx, c, in, permsRead, p.Head.SHA)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	head := fmt.Sprintf("%d passed, %d failed, %d pending, %d skipped", len(s.passed), len(s.failed), len(s.pending), len(s.skipped))
	var sb strings.Builder
	fmt.Fprintf(&sb, "PR #%d head %s: %s\n", p.Number, p.Head.SHA, head)
	if len(s.failed) > 0 {
		sb.WriteString("failed: " + listNames(s.failed) + "\n")
	}
	if len(s.pending) > 0 {
		sb.WriteString("pending: " + listNames(s.pending) + "\n")
	}
	sb.WriteString("combined status: " + s.combined)
	if s.incomplete {
		fmt.Fprintf(&sb, "\nincomplete: read %d of %d check runs and %d of %d statuses; the list above is incomplete", s.runsRead, s.runsTotal, s.statusesRead, s.statusesTotal)
		head += " (incomplete)"
	}
	return tools.ToolOutput{Text: sb.String(), Summary: head}, nil
}

func prFiles(ctx context.Context, t *readTool, c tools.Call, in input) (tools.ToolOutput, error) {
	var files []struct {
		Filename, Status, Patch string
		PreviousFilename        string `json:"previous_filename"`
		Additions, Deletions    int
	}
	if err := t.do(ctx, c, in, permsRead, "GET", fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=%d", in.Repo, in.num(), maxFiles), nil, &files); err != nil {
		return tools.ToolOutput{}, err
	}
	var sb strings.Builder
	budget := maxPatchTotal
	for _, f := range files {
		fmt.Fprintf(&sb, "%s (%s +%d -%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
		if f.Patch == "" {
			continue
		}
		if tools.DeniedPath(f.Filename) || (f.PreviousFilename != "" && tools.DeniedPath(f.PreviousFilename)) {
			sb.WriteString("[patch withheld: secret path]\n")
			continue
		}
		limit := min(maxPatchPerFile, budget)
		if limit <= 0 {
			sb.WriteString("[patch omitted]\n")
			continue
		}
		patch := f.Patch
		if len(patch) > limit {
			patch = strings.ToValidUTF8(patch[:limit], "") + "\n[patch truncated]"
		}
		budget -= len(patch)
		sb.WriteString(patch + "\n")
	}
	if len(files) == maxFiles {
		sb.WriteString("(file list may be incomplete: first 100 shown)\n")
	}
	return tools.ToolOutput{Text: sb.String(), Summary: fmt.Sprintf("%d files", len(files))}, nil
}

func issueView(ctx context.Context, t *readTool, c tools.Call, in input) (tools.ToolOutput, error) {
	var is struct {
		Number      int64
		Title       string
		State       string
		StateReason string `json:"state_reason"`
		User        struct{ Login string }
		Labels      []struct{ Name string }
		Comments    int
		HTMLURL     string `json:"html_url"`
		Body        string
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := t.do(ctx, c, in, permsRead, "GET", fmt.Sprintf("/repos/%s/issues/%d", in.Repo, in.num()), nil, &is); err != nil {
		return tools.ToolOutput{}, err
	}
	labels := make([]string, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, l.Name)
	}
	kind := "issue"
	if is.PullRequest != nil {
		kind = "pull request"
	}
	state := is.State
	if is.StateReason != "" {
		state += " (" + is.StateReason + ")"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "#%d %s\n%s | state: %s | author: %s | %d comments\nlabels: %s\n%s\n", is.Number, is.Title, kind, state, is.User.Login, is.Comments, strings.Join(labels, ", "), is.HTMLURL)
	if body := strings.TrimSpace(is.Body); body != "" {
		sb.WriteString("body:\n" + clipRunes(body, maxBodyRunes))
	}
	return tools.ToolOutput{Text: sb.String(), Summary: fmt.Sprintf("issue #%d %s", is.Number, is.State)}, nil
}
