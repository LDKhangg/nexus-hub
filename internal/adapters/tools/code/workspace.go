package codetools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// TokenSource mints a GitHub token for an account and repo; implemented by
// github.Credentials.
type TokenSource interface {
	Token(ctx context.Context, workspaceID string, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error)
}

const (
	githubRemoteBase  = "https://github.com/"
	extraHeaderKey    = "http.https://github.com/.extraheader"
	maxCommitPaths    = 100
	maxCommitMsgRunes = 2000
	maxConfigValue    = 200
)

var (
	permsRead  = map[string]string{"contents": "read"}
	permsWrite = map[string]string{"contents": "write"}
)

// workspace is shared state for the managed-clone tools.
type workspace struct {
	runner        Runner
	tokens        TokenSource
	reposDir      string
	maxCloneBytes int64
	san           tools.Sanitizer
	remoteBase    string // https://github.com/ in production; file:// in tests

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (w *workspace) lock(repo string) func() {
	w.mu.Lock()
	l, ok := w.locks[repo]
	if !ok {
		l = &sync.Mutex{}
		w.locks[repo] = l
	}
	w.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// args builds hardened args; the test remote additionally allows file://.
func (w *workspace) args(root string, sub ...string) []string {
	a := hardenedArgs(root)
	if w.remoteBase != githubRemoteBase {
		a = append(a, "-c", "protocol.file.allow=always")
	}
	return append(a, sub...)
}

func (w *workspace) remoteURL(repo string) string { return w.remoteBase + repo + ".git" }

// tokenEnv passes the token only through the child environment.
func (w *workspace) tokenEnv(ctx context.Context, c tools.Call, perms map[string]string) ([]string, error) {
	if w.tokens == nil || c.Account == nil {
		return nil, toolErr("missing_credentials")
	}
	tok, err := w.tokens.Token(ctx, c.WorkspaceID, *c.Account, c.Repo.Repo, perms)
	if err != nil {
		if te, ok := err.(*tools.ToolError); ok {
			return nil, te
		}
		return nil, toolErr("credentials_unavailable")
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + extraHeaderKey, "GIT_CONFIG_VALUE_0=" + header}, nil
}

func (w *workspace) run(ctx context.Context, root string, env []string, network bool, sub ...string) (execrunner.Result, error) {
	return runGit(ctx, w.runner, execrunner.Command{Args: w.args(root, sub...), Env: env, Network: network})
}

// wsTool is one workspace tool.
type wsTool struct {
	w       *workspace
	spec    tools.ToolSpec
	allowed []string
	network bool
	exec    func(ctx context.Context, w *workspace, c tools.Call, root string, in input) (tools.ToolOutput, error)
	needs   bool // requires an existing clone
}

func (t *wsTool) Spec() tools.ToolSpec                     { return t.spec }
func (t *wsTool) Repo(raw json.RawMessage) (string, error) { return repoOf(raw) }
func (t *wsTool) UsesNetwork() bool                        { return t.network }

func (t *wsTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	if c.Repo == nil || !c.Repo.Managed {
		return tools.ToolOutput{}, toolErr("not_managed")
	}
	in, err := decode(c.Input, t.allowed...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if c.Repo.Repo != in.Repo {
		return tools.ToolOutput{}, toolErr("repo_unavailable")
	}
	root, err := managedPath(t.w.reposDir, in.Repo)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	defer t.w.lock(in.Repo)()
	if t.needs {
		if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || !st.IsDir() {
			return tools.ToolOutput{}, toolErr("repo_unavailable")
		}
	}
	return t.exec(ctx, t.w, c, root, in)
}

// pushTool adds tier escalation to git_push.
type pushTool struct{ *wsTool }

// EffectiveTier makes a push dangerous when it targets a protected branch,
// a merge base branch, or the remote default branch.
func (p pushTool) EffectiveTier(raw json.RawMessage, g *tenant.RepoGrant) tenant.Tier {
	var in struct {
		Branch string `json:"branch"`
	}
	_ = json.Unmarshal(raw, &in)
	if g == nil {
		return tenant.TierDangerous
	}
	for _, pat := range g.ProtectedBranches {
		if tenant.MatchBranchPattern(pat, in.Branch) {
			return tenant.TierDangerous
		}
	}
	for _, b := range g.MergeBaseBranches {
		if b == in.Branch {
			return tenant.TierDangerous
		}
	}
	if def, ok := p.w.remoteDefault(g.Repo); ok && def == in.Branch {
		return tenant.TierDangerous
	}
	return tenant.TierWrite
}

// remoteDefault reads refs/remotes/origin/HEAD of the managed clone.
func (w *workspace) remoteDefault(repo string) (string, bool) {
	root, err := managedPath(w.reposDir, repo)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(root, ".git", "refs", "remotes", "origin", "HEAD"))
	if err != nil {
		return "", false
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/")
	return ref, ok && ref != ""
}

// NewWorkspaceTools returns the managed-clone tools.
func NewWorkspaceTools(r Runner, tokens TokenSource, reposDir string, maxCloneBytes int64, s tools.Sanitizer) []tools.Tool {
	return newWorkspaceTools(r, tokens, reposDir, maxCloneBytes, s, githubRemoteBase)
}

func newWorkspaceTools(r Runner, tokens TokenSource, reposDir string, maxCloneBytes int64, s tools.Sanitizer, remoteBase string) []tools.Tool {
	w := &workspace{runner: r, tokens: tokens, reposDir: reposDir, maxCloneBytes: maxCloneBytes, san: s, remoteBase: remoteBase, locks: map[string]*sync.Mutex{}}
	mk := func(name, desc string, tier tenant.Tier, sch json.RawMessage, allowed []string, network, needs bool, exec func(context.Context, *workspace, tools.Call, string, input) (tools.ToolOutput, error)) *wsTool {
		return &wsTool{w: w, spec: tools.ToolSpec{Name: name, Description: desc, InputSchema: sch, Tier: tier}, allowed: allowed, network: network, needs: needs, exec: exec}
	}
	branchProp := `,"branch":{"type":"string"}`
	push := mk("git_push", "Push a local branch to origin (same name). Only branches matching the repo's push patterns; never force.",
		tenant.TierWrite, schema(branchProp, "branch"), []string{"branch"}, true, true, gitPush)
	return []tools.Tool{
		mk("git_sync", "Clone the managed repo if absent, otherwise fetch origin.", tenant.TierWrite, schema(""), nil, true, false, gitSync),
		mk("git_pull", "Fast-forward the current branch from origin.", tenant.TierWrite, schema(""), nil, true, true, gitPull),
		mk("git_checkout", "Switch to a branch, optionally creating it from start_point.", tenant.TierWrite,
			schema(branchProp+`,"create":{"type":"boolean"},"start_point":{"type":"string"}`, "branch"), []string{"branch", "create", "start_point"}, false, true, gitCheckout),
		mk("git_commit", "Stage the given paths and commit them with the account identity.", tenant.TierWrite,
			schema(`,"paths":{"type":"array","items":{"type":"string"},"maxItems":100},"message":{"type":"string"}`, "paths", "message"), []string{"paths", "message"}, false, true, gitCommit),
		mk("git_config_set", "Set an allowlisted local config key (user.name, user.email, pull.ff, branch.<b>.remote, branch.<b>.merge).", tenant.TierWrite,
			schema(`,"key":{"type":"string"},"value":{"type":"string"}`, "key", "value"), []string{"key", "value"}, false, true, gitConfigSet),
		pushTool{push},
		mk("git_reset_hard", "Reset the managed clone hard to a ref (discards local changes).", tenant.TierDangerous,
			schema(`,"ref":{"type":"string"}`, "ref"), []string{"ref"}, false, true, gitResetHard),
		mk("git_clean", "Remove untracked files and directories (ignored files are kept).", tenant.TierDangerous, schema(""), nil, false, true, gitClean),
	}
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func (w *workspace) checkSize(root string) error {
	if w.maxCloneBytes > 0 && dirSize(root) > w.maxCloneBytes {
		_ = os.RemoveAll(root)
		return toolErr("clone_too_large")
	}
	return nil
}

func gitSync(ctx context.Context, w *workspace, c tools.Call, root string, in input) (tools.ToolOutput, error) {
	env, err := w.tokenEnv(ctx, c, permsRead)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if st, err := os.Stat(filepath.Join(root, ".git")); err == nil && st.IsDir() {
		res, err := w.run(ctx, root, env, true, "fetch", "--quiet", "--prune", "--no-recurse-submodules", "origin")
		if err != nil {
			return tools.ToolOutput{}, err
		}
		if res.ExitCode != 0 {
			return tools.ToolOutput{}, networkErr(res)
		}
		if err := w.checkSize(root); err != nil {
			return tools.ToolOutput{}, err
		}
		return tools.ToolOutput{Text: "fetched origin for " + in.Repo + headInfo(ctx, w, root), Summary: "fetched " + in.Repo}, nil
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return tools.ToolOutput{}, toolErr("repo_unavailable")
	}
	_ = os.RemoveAll(root) // a leftover directory without .git is managed debris
	res, err := w.run(ctx, parent, env, true, "clone", "--quiet", "--filter=blob:none", "--no-recurse-submodules", "--", w.remoteURL(in.Repo), filepath.Base(root))
	if err != nil {
		_ = os.RemoveAll(root)
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		_ = os.RemoveAll(root)
		return tools.ToolOutput{}, networkErr(res)
	}
	if err := w.checkSize(root); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: "cloned " + in.Repo + headInfo(ctx, w, root), Summary: "cloned " + in.Repo}, nil
}

func headInfo(ctx context.Context, w *workspace, root string) string {
	res, err := w.run(ctx, root, nil, false, "log", "-1", "--format=%h %s", "--no-color")
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return " (HEAD " + clipRunes(strings.TrimSpace(string(res.Stdout)), 120) + ")"
}

var authFailRe = regexp.MustCompile(`(?i)(authentication failed|could not read username|403|401|permission denied|not found)`)

func networkErr(res execrunner.Result) error {
	if authFailRe.Match(res.Stderr) {
		return toolErr("remote_auth_failed")
	}
	return toolErr("remote_failed")
}

func gitPull(ctx context.Context, w *workspace, c tools.Call, root string, in input) (tools.ToolOutput, error) {
	env, err := w.tokenEnv(ctx, c, permsRead)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	res, err := w.run(ctx, root, env, true, "pull", "--ff-only", "--quiet", "--no-recurse-submodules")
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		stderr := strings.ToLower(string(res.Stderr))
		if strings.Contains(stderr, "fast-forward") || strings.Contains(stderr, "diverg") {
			return tools.ToolOutput{}, toolErr("pull_not_fast_forward")
		}
		if strings.Contains(stderr, "no tracking information") {
			return tools.ToolOutput{}, toolErr("no_upstream")
		}
		return tools.ToolOutput{}, networkErr(res)
	}
	if err := w.checkSize(root); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: "pulled (fast-forward)" + headInfo(ctx, w, root), Summary: "pulled " + in.Repo}, nil
}

func (w *workspace) checkBranch(ctx context.Context, root, branch string) error {
	if !validBranch(branch) {
		return toolErr("invalid_input")
	}
	res, err := w.run(ctx, root, nil, false, "check-ref-format", "--branch", branch)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return toolErr("invalid_input")
	}
	return nil
}

func gitCheckout(ctx context.Context, w *workspace, _ tools.Call, root string, in input) (tools.ToolOutput, error) {
	if err := w.checkBranch(ctx, root, in.Branch); err != nil {
		return tools.ToolOutput{}, err
	}
	args := []string{"switch", "--quiet", "--no-guess"}
	if in.Create {
		args = append(args, "-c", in.Branch)
		if in.StartPoint != "" {
			if !validRef(in.StartPoint) {
				return tools.ToolOutput{}, toolErr("invalid_input")
			}
			args = append(args, in.StartPoint)
		}
	} else {
		if in.StartPoint != "" {
			return tools.ToolOutput{}, toolErr("invalid_input")
		}
		args = append(args, in.Branch)
	}
	res, err := w.run(ctx, root, nil, false, args...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		switch {
		case strings.Contains(stderr, "already exists"):
			return tools.ToolOutput{}, toolErr("branch_exists")
		case strings.Contains(stderr, "overwritten") || strings.Contains(stderr, "local changes"):
			return tools.ToolOutput{}, toolErr("dirty_worktree")
		}
		return tools.ToolOutput{}, gitErr(res)
	}
	verb := "switched to"
	if in.Create {
		verb = "created and switched to"
	}
	return tools.ToolOutput{Text: verb + " " + in.Branch + headInfo(ctx, w, root), Summary: verb + " " + in.Branch}, nil
}

func gitCommit(ctx context.Context, w *workspace, c tools.Call, root string, in input) (tools.ToolOutput, error) {
	if len(in.Paths) == 0 || len(in.Paths) > maxCommitPaths {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" || utf8.RuneCountInString(msg) > maxCommitMsgRunes {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	paths := make([]string, 0, len(in.Paths))
	for _, raw := range in.Paths {
		p, err := cleanRepoPath(raw, false)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		if deniedPath(p) {
			return tools.ToolOutput{}, toolErr("denied_path")
		}
		paths = append(paths, p)
	}
	if c.Account == nil || c.Account.CommitName == "" || c.Account.CommitEmail == "" {
		return tools.ToolOutput{}, toolErr("account_identity_missing")
	}
	res, err := w.run(ctx, root, nil, false, append([]string{"add", "--"}, paths...)...)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		if bytes.Contains(res.Stderr, []byte("did not match")) {
			return tools.ToolOutput{}, toolErr("not_found")
		}
		return tools.ToolOutput{}, gitErr(res)
	}
	clean := w.san.CleanOutbound(msg, true)
	res, err = w.run(ctx, root, nil, false,
		"-c", "user.name="+c.Account.CommitName, "-c", "user.email="+c.Account.CommitEmail,
		"commit", "--quiet", "-m", clean)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		out := string(res.Stdout) + string(res.Stderr)
		if strings.Contains(out, "nothing to commit") || strings.Contains(out, "no changes added") || strings.Contains(out, "nothing added") {
			return tools.ToolOutput{}, toolErr("nothing_to_commit")
		}
		return tools.ToolOutput{}, gitErr(res)
	}
	return tools.ToolOutput{Text: fmt.Sprintf("committed %d path(s)", len(paths)) + headInfo(ctx, w, root), Summary: fmt.Sprintf("committed %d path(s)", len(paths))}, nil
}

var branchKeyRe = regexp.MustCompile(`^branch\.(.+)\.(remote|merge)$`)

func gitConfigSet(ctx context.Context, w *workspace, _ tools.Call, root string, in input) (tools.ToolOutput, error) {
	key, value := in.Key, in.Value
	if len(value) > maxConfigValue || strings.ContainsAny(value, "\n\r\x00") {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	switch {
	case key == "user.name" || key == "user.email":
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "<>") {
			return tools.ToolOutput{}, toolErr("invalid_input")
		}
	case key == "pull.ff":
		if value != "true" && value != "false" && value != "only" {
			return tools.ToolOutput{}, toolErr("invalid_input")
		}
	default:
		m := branchKeyRe.FindStringSubmatch(key)
		if m == nil || !validBranch(m[1]) {
			return tools.ToolOutput{}, toolErr("config_key_denied")
		}
		if m[2] == "remote" && value != "origin" {
			return tools.ToolOutput{}, toolErr("invalid_input")
		}
		if m[2] == "merge" {
			b, ok := strings.CutPrefix(value, "refs/heads/")
			if !ok || !validBranch(b) {
				return tools.ToolOutput{}, toolErr("invalid_input")
			}
		}
	}
	res, err := w.run(ctx, root, nil, false, "config", "--local", "--", key, value)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	return tools.ToolOutput{Text: "set " + key, Summary: "set " + key}, nil
}

func gitPush(ctx context.Context, w *workspace, c tools.Call, root string, in input) (tools.ToolOutput, error) {
	if err := w.checkBranch(ctx, root, in.Branch); err != nil {
		return tools.ToolOutput{}, err
	}
	allowed := false
	for _, pat := range c.Repo.PushBranches {
		if tenant.MatchBranchPattern(pat, in.Branch) {
			allowed = true
			break
		}
	}
	if !allowed {
		return tools.ToolOutput{}, toolErr("branch_not_allowed")
	}
	env, err := w.tokenEnv(ctx, c, permsWrite)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	ref := "refs/heads/" + in.Branch
	res, err := w.run(ctx, root, env, true, "push", "--quiet", "--no-verify", "origin", ref+":"+ref)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		switch {
		case strings.Contains(stderr, "rejected") || strings.Contains(stderr, "non-fast-forward") || strings.Contains(stderr, "fetch first"):
			return tools.ToolOutput{}, toolErr("push_rejected")
		case strings.Contains(stderr, "does not match any") || strings.Contains(stderr, "src refspec"):
			return tools.ToolOutput{}, toolErr("not_found")
		}
		return tools.ToolOutput{}, networkErr(res)
	}
	return tools.ToolOutput{Text: "pushed " + in.Branch + " to origin" + headInfo(ctx, w, root), Summary: "pushed " + in.Branch}, nil
}

func gitResetHard(ctx context.Context, w *workspace, _ tools.Call, root string, in input) (tools.ToolOutput, error) {
	if !validRef(in.Ref) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	res, err := w.run(ctx, root, nil, false, "reset", "--quiet", "--hard", in.Ref, "--")
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	return tools.ToolOutput{Text: "reset --hard to " + in.Ref + headInfo(ctx, w, root), Summary: "reset to " + in.Ref}, nil
}

func gitClean(ctx context.Context, w *workspace, _ tools.Call, root string, _ input) (tools.ToolOutput, error) {
	res, err := w.run(ctx, root, nil, false, "clean", "-fd")
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if res.ExitCode != 0 {
		return tools.ToolOutput{}, gitErr(res)
	}
	removed := strings.Count(string(res.Stdout), "Removing ")
	return tools.ToolOutput{Text: fmt.Sprintf("removed %d untracked path(s)", removed), Summary: fmt.Sprintf("clean removed %d", removed)}, nil
}

// NewWorkspaceToolsWithLocalRemote uses remoteBase (a file:// URL ending in
// "/") instead of github.com. It exists for cross-package end-to-end tests
// and panics outside `go test`, so production can never enable file://.
func NewWorkspaceToolsWithLocalRemote(r Runner, tokens TokenSource, reposDir string, maxCloneBytes int64, s tools.Sanitizer, remoteBase string) []tools.Tool {
	if !testing.Testing() || !strings.HasPrefix(remoteBase, "file://") {
		panic("codetools: local remotes are test-only")
	}
	return newWorkspaceTools(r, tokens, reposDir, maxCloneBytes, s, remoteBase)
}
