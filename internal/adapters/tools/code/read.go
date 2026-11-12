package codetools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const (
	maxListEntries  = 200
	maxSearchHits   = 50
	maxHitRunes     = 200
	maxReadLines    = 200
	maxObjectBytes  = 1 << 20
	maxLogEntries   = 20
	maxBlameLines   = 100
	maxPatchBytes   = 6 * 1024
	maxStatusLines  = 200
	maxOutlineItems = 200
)

// readTool is a read-tier code tool driven by a run function.
type readTool struct {
	spec        tools.ToolSpec
	allowed     []string
	managedOnly bool
	runner      *execrunner.Runner
	reposDir    string
	run         func(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error)
}

func (t *readTool) Spec() tools.ToolSpec                     { return t.spec }
func (t *readTool) Repo(raw json.RawMessage) (string, error) { return repoOf(raw) }

func (t *readTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	in, err := decode(c.Input, t.allowed...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if c.Repo == nil || c.Repo.Repo != in.Repo {
		return tools.ToolOutput{}, toolErr("repo_unavailable")
	}
	if t.managedOnly && !c.Repo.Managed {
		return tools.ToolOutput{}, deniedErr("not_managed")
	}
	root, err := cloneRoot(c.Repo, t.reposDir)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if in.Ref == "" {
		in.Ref = "HEAD"
	}
	if !validRef(in.Ref) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	return t.run(ctx, t, root, in)
}

func (t *readTool) git(ctx context.Context, root string, sub ...string) (execrunner.Result, error) {
	return git(ctx, t.runner, root, execrunner.Command{}, sub...)
}

// NewReadTools returns the read-tier code tools.
func NewReadTools(r *execrunner.Runner, reposDir string) []tools.Tool {
	mk := func(name, desc string, sch json.RawMessage, allowed []string, managedOnly bool, run func(context.Context, *readTool, string, input) (tools.ToolOutput, error)) tools.Tool {
		return &readTool{spec: tools.ToolSpec{Name: name, Description: desc, InputSchema: sch, Tier: tenant.TierRead},
			allowed: allowed, managedOnly: managedOnly, runner: r, reposDir: reposDir, run: run}
	}
	refProp := `,"ref":{"type":"string","description":"commit, branch or tag; default HEAD"}`
	pathProp := `,"path":{"type":"string","description":"repo-relative path"}`
	lines := `,"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}`
	return []tools.Tool{
		mk("code_list_files", "List committed files (max 200) under an optional directory.",
			schema(refProp+pathProp), []string{"ref", "path"}, false, listFiles),
		mk("code_search", "Search committed files; fixed string unless regex=true. Max 50 hits.",
			schema(refProp+pathProp+`,"query":{"type":"string"},"regex":{"type":"boolean"}`, "query"), []string{"ref", "path", "query", "regex"}, false, search),
		mk("code_read_file", "Read a committed text file with line numbers (max 200 lines per call).",
			schema(refProp+pathProp+lines, "path"), []string{"ref", "path", "start_line", "end_line"}, false, readFile),
		mk("code_outline", "List top-level declarations of a committed file with line numbers.",
			schema(refProp+pathProp, "path"), []string{"ref", "path"}, false, outline),
		mk("git_log", "Recent commits (max 20), optionally for one path.",
			schema(refProp+pathProp+`,"max":{"type":"integer","minimum":1,"maximum":20}`), []string{"ref", "path", "max"}, false, gitLog),
		mk("git_diff", "Diff between two commits: stat plus a capped patch.",
			schema(`,"base":{"type":"string"},"head":{"type":"string","description":"default HEAD"}`, "base"), []string{"base", "head"}, false, gitDiff),
		mk("git_blame", "Blame a line range (max 100 lines) of a committed file.",
			schema(refProp+pathProp+lines, "path"), []string{"ref", "path", "start_line", "end_line"}, false, gitBlame),
		mk("git_status", "Working tree status of the managed clone.",
			schema(""), nil, true, gitStatus),
	}
}

func listFiles(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	p, err := cleanRepoPath(in.Path, true)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	args := []string{"ls-tree", "-r", "-z", "--name-only", in.Ref, "--"}
	if p != "" {
		args = append(args, p)
	}
	res, err := t.git(ctx, root, args...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	var out []string
	more := 0
	for _, f := range strings.Split(string(res.Stdout), "\x00") {
		if f == "" || deniedPath(f) {
			continue
		}
		if len(out) >= maxListEntries {
			more++
			continue
		}
		out = append(out, f)
	}
	text := strings.Join(out, "\n")
	if more > 0 {
		text += fmt.Sprintf("\n(%d more files not shown)", more)
	}
	if len(out) == 0 {
		text = "(no files)"
	}
	return tools.ToolOutput{Text: text, Summary: fmt.Sprintf("%d files", len(out)+more)}, nil
}

var grepLineRe = regexp.MustCompile(`^(.*?):([0-9]+):(.*)$`)

func search(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	q := strings.TrimSpace(in.Query)
	if q == "" || len(q) > 200 || strings.ContainsAny(q, "\x00\n") {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	p, err := cleanRepoPath(in.Path, true)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	mode := "-F"
	if in.Regex {
		mode = "-E"
	}
	args := []string{"grep", "-n", "-I", "--no-color", "--no-textconv", mode, "-e", q, in.Ref, "--"}
	if p != "" {
		args = append(args, p)
	}
	res, err := t.git(ctx, root, args...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode == 1 && len(res.Stdout) == 0 {
		return tools.ToolOutput{Text: "no matches", Summary: "0 hits"}, nil
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	prefix := in.Ref + ":"
	var hits []string
	total := 0
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimPrefix(line, prefix)
		m := grepLineRe.FindStringSubmatch(line)
		if m == nil || deniedPath(m[1]) {
			continue
		}
		total++
		if len(hits) < maxSearchHits {
			hits = append(hits, m[1]+":"+m[2]+":"+clipRunes(strings.TrimSpace(m[3]), maxHitRunes))
		}
	}
	if len(hits) == 0 {
		return tools.ToolOutput{Text: "no matches", Summary: "0 hits"}, nil
	}
	text := strings.Join(hits, "\n")
	if total > len(hits) || res.Truncated {
		text += fmt.Sprintf("\n(results limited to %d hits)", maxSearchHits)
	}
	return tools.ToolOutput{Text: text, Summary: fmt.Sprintf("%d hits", total)}, nil
}

// readBlob returns a committed text blob, enforcing deny-list, type, size
// and binary checks.
func (t *readTool) readBlob(ctx context.Context, root, ref, rawPath string) (string, []byte, error) {
	p, err := cleanRepoPath(rawPath, false)
	if err != nil {
		return "", nil, err
	}
	if deniedPath(p) {
		return "", nil, deniedErr("denied_path")
	}
	obj := ref + ":" + p
	res, err := t.git(ctx, root, "cat-file", "-t", obj)
	if err != nil {
		return "", nil, err
	}
	if res.ExitCode != 0 {
		return "", nil, toolErr("not_found")
	}
	if strings.TrimSpace(string(res.Stdout)) != "blob" {
		return "", nil, toolErr("not_a_file")
	}
	res, err = t.git(ctx, root, "cat-file", "-s", obj)
	if err != nil {
		return "", nil, err
	}
	size, perr := strconv.ParseInt(strings.TrimSpace(string(res.Stdout)), 10, 64)
	if res.ExitCode != 0 || perr != nil {
		return "", nil, gitErr(res)
	}
	if size > maxObjectBytes {
		return "", nil, toolErr("file_too_large")
	}
	res, err = t.git(ctx, root, "cat-file", "blob", obj)
	if err != nil {
		return "", nil, err
	}
	if res.ExitCode != 0 {
		return "", nil, gitErr(res)
	}
	head := res.Stdout
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", nil, toolErr("binary_file")
	}
	return p, res.Stdout, nil
}

func lineWindow(start, end, maxLines int) (int, int) {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start + maxLines - 1
	}
	if end-start+1 > maxLines {
		end = start + maxLines - 1
	}
	return start, end
}

func readFile(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	p, content, err := t.readBlob(ctx, root, in.Ref, in.Path)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	start, end := lineWindow(in.StartLine, in.EndLine, maxReadLines)
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (lines %d-%d of %d lines)\n", p, start, end, len(lines))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	return tools.ToolOutput{Text: b.String(), Summary: fmt.Sprintf("%s lines %d-%d", p, start, end)}, nil
}

func gitLog(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	n := in.Max
	if n <= 0 || n > maxLogEntries {
		n = maxLogEntries
	}
	p, err := cleanRepoPath(in.Path, true)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	args := []string{"log", "--no-color", "--no-ext-diff", "--no-textconv", "--date=short", "--format=%h %ad %an: %s", "-n", strconv.Itoa(n), "--end-of-options", in.Ref, "--"}
	if p != "" {
		if deniedPath(p) {
			return tools.ToolOutput{}, deniedErr("denied_path")
		}
		args = append(args, p)
	}
	res, err := t.git(ctx, root, args...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	text := strings.TrimSpace(string(res.Stdout))
	if text == "" {
		text = "(no commits)"
	}
	return tools.ToolOutput{Text: text, Summary: fmt.Sprintf("%d commits", strings.Count(text, "\n")+1)}, nil
}

func gitDiff(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	head := in.Head
	if head == "" {
		head = "HEAD"
	}
	if !validRef(in.Base) || !validRef(head) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	common := []string{"--no-color", "--no-ext-diff", "--no-textconv", "--end-of-options", in.Base, head, "--", "."}
	common = append(common, deniedPathspecs...)
	stat, err := t.git(ctx, root, append([]string{"diff", "--stat=120"}, common...)...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if stat.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(stat)
	}
	patch, err := t.git(ctx, root, append([]string{"diff"}, common...)...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if patch.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(patch)
	}
	text := strings.TrimSpace(string(stat.Stdout))
	if text == "" {
		return tools.ToolOutput{Text: "(no differences)", Summary: "no diff"}, nil
	}
	body := string(patch.Stdout)
	room := maxPatchBytes - len(text) - 2
	if room < 0 {
		room = 0
	}
	if len(body) > room || patch.Truncated {
		cut := room
		for cut > 0 && cut < len(body) && (body[cut]&0xC0) == 0x80 {
			cut--
		}
		if cut > len(body) {
			cut = len(body)
		}
		body = body[:cut] + "\n[truncated]"
	}
	return tools.ToolOutput{Text: text + "\n\n" + body, Summary: lastLine(text)}, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func gitBlame(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	p, err := cleanRepoPath(in.Path, false)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if deniedPath(p) {
		return tools.ToolOutput{}, deniedErr("denied_path")
	}
	start, end := lineWindow(in.StartLine, in.EndLine, maxBlameLines)
	res, err := t.git(ctx, root, "blame", "--no-textconv", "--date=short", "-L", fmt.Sprintf("%d,%d", start, end), in.Ref, "--", p) // blame mis-parses --end-of-options; validRef forbids a leading "-"
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		if bytes.Contains(res.Stderr, []byte("has only")) {
			return tools.ToolOutput{}, toolErr("invalid_range")
		}
		return tools.ToolOutput{}, gitErr(res)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	for i, l := range lines {
		lines[i] = clipRunes(l, 240)
	}
	return tools.ToolOutput{Text: strings.Join(lines, "\n"), Summary: fmt.Sprintf("%s lines %d-%d", p, start, start+len(lines)-1)}, nil
}

func gitStatus(ctx context.Context, t *readTool, root string, _ input) (tools.ToolOutput, error) {
	// status hashes worktree files, so clean filters and similar programs
	// from the repository config would run; refuse such a config first.
	if err := t.checkWorktreeConfig(ctx, root); err != nil {
		return tools.ToolOutput{}, err
	}
	res, err := t.git(ctx, root, "status", "--porcelain=v2", "--branch", "--untracked-files=normal")
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) > maxStatusLines {
		lines = append(lines[:maxStatusLines], fmt.Sprintf("(%d more lines)", len(lines)-maxStatusLines))
	}
	return tools.ToolOutput{Text: strings.Join(lines, "\n"), Summary: fmt.Sprintf("%d status lines", len(lines))}, nil
}

// checkWorktreeConfig fails with unsafe_repo_config when the repository's
// local config defines anything that makes git run a program while reading
// the worktree: filters, textconv or external diff/merge commands, an
// fsmonitor, or includes and worktree config that could add them.
func (t *readTool) checkWorktreeConfig(ctx context.Context, root string) error {
	res, err := t.git(ctx, root, "config", "--local", "--list", "-z")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || res.Truncated {
		return deniedErr("unsafe_repo_config")
	}
	for _, entry := range strings.Split(string(res.Stdout), "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		if unsafeWorktreeKey(strings.ToLower(key)) {
			return deniedErr("unsafe_repo_config")
		}
	}
	return nil
}

func unsafeWorktreeKey(k string) bool {
	switch {
	case k == "":
		return false
	case strings.HasPrefix(k, "filter."), strings.HasPrefix(k, "include."), strings.HasPrefix(k, "includeif."):
		return true
	case strings.HasSuffix(k, ".textconv"), strings.HasSuffix(k, ".command"), strings.HasSuffix(k, ".driver"):
		return true
	case k == "core.fsmonitor", k == "core.worktree", k == "extensions.worktreeconfig", k == "diff.external":
		return true
	}
	return false
}
