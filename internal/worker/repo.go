package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	gitTimeout      = 10 * time.Minute
	worktreesSubdir = ".nexus-worktrees"
	branchPrefix    = "nexus/"
)

// Repos does the git and gh work of a job with the owner's local
// credentials. Every command is argv-only (no shell).
type Repos struct {
	Git       string
	GH        string
	CloneWith string // gh | git
	ReposDir  string
	Runner    Runner
	Env       []string // GitEnv(os.Environ()) when nil
}

var (
	errWorkspace = errors.New("workspace preparation failed")
	shaRE        = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	refNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	prURLRE      = regexp.MustCompile(`https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/pull/[0-9]+`)
)

func (r *Repos) env() []string {
	if r.Env != nil {
		return r.Env
	}
	return GitEnv(os.Environ())
}

// run runs a git/gh command and returns its exit code and bounded stdout.
// Inside a job worktree it first checks that the worktree's .git link
// still points to its own git directory, and every command runs with
// repository settings that execute code turned off (gitSafetyEnv): the
// executor can write files in the worktree, never code the worker runs.
func (r *Repos) run(ctx context.Context, dir, bin string, args ...string) (int, string, error) {
	if err := r.checkGitLink(dir); err != nil {
		return -1, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out := newCapBuffer(1 << 20)
	exit, err := r.Runner.Run(ctx, Cmd{Dir: dir, Name: bin, Args: args, Env: gitSafetyEnv(r.env()), Stdout: out})
	return exit, out.String(), err
}

// gitSafetyEnv overrides, with command-line precedence (GIT_CONFIG_COUNT),
// the git settings that make git run a program on add, diff, commit or
// push: core.fsmonitor and hooks. Inherited config overrides, GIT_DIR and
// GIT_WORK_TREE are dropped so the worktree's own git directory is used.
func gitSafetyEnv(env []string) []string {
	out := make([]string, 0, len(env)+5)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if u == "GIT_CONFIG_COUNT" || u == "GIT_CONFIG_PARAMETERS" || u == "GIT_DIR" || u == "GIT_WORK_TREE" ||
			strings.HasPrefix(u, "GIT_CONFIG_KEY_") || strings.HasPrefix(u, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1="+os.DevNull)
}

var errGitLink = errors.New("the worktree git link was changed")

// checkGitLink refuses a command inside a job worktree whose .git is not
// the regular file git wrote, pointing into <clone>/.git/worktrees/. An
// executor that rewrote it could point git at a directory it controls
// (with its own config and hooks).
func (r *Repos) checkGitLink(dir string) error {
	if r.ReposDir == "" || dir == "" {
		return nil
	}
	sep := string(filepath.Separator)
	root := filepath.Join(r.ReposDir, worktreesSubdir) + sep
	clean := filepath.Clean(dir)
	if !strings.HasPrefix(clean, root) {
		return nil // not inside a job worktree
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(clean, root), sep)
	if first == "" {
		return nil
	}
	wt := root + first
	link := filepath.Join(wt, ".git")
	fi, err := os.Lstat(link)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return errGitLink
	}
	data, err := os.ReadFile(link)
	if err != nil {
		return errGitLink
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	p = strings.TrimSpace(p)
	if !ok || p == "" || strings.ContainsAny(p, "\n\r\x00") {
		return errGitLink
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt, p)
	}
	p = filepath.Clean(p)
	if !strings.HasPrefix(p, filepath.Clean(r.ReposDir)+sep) || strings.HasPrefix(p+sep, root) ||
		!strings.Contains(p, sep+".git"+sep+"worktrees"+sep) {
		return errGitLink
	}
	return nil
}

func (r *Repos) git(ctx context.Context, dir string, args ...string) (string, error) {
	exit, out, err := r.run(ctx, dir, r.Git, args...)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return out, fmt.Errorf("git %s failed", args[0])
	}
	return out, nil
}

// ClonePath is where repo ("owner/name") is cloned.
func (r *Repos) ClonePath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return filepath.Join(r.ReposDir, owner, name)
}

// WorktreePath is the worktree of a job.
func (r *Repos) WorktreePath(jobID string) string {
	return filepath.Join(r.ReposDir, worktreesSubdir, jobID)
}

// EnsureClone clones repo when missing, else fetches origin (W9).
func (r *Repos) EnsureClone(ctx context.Context, repo string) (string, error) {
	if !ValidRepo(repo) {
		return "", errWorkspace
	}
	dir := r.ClonePath(repo)
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		if _, err := r.git(ctx, dir, "fetch", "--prune", "--quiet", "origin"); err != nil {
			return "", errWorkspace
		}
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", errWorkspace
	}
	if _, err := os.Stat(dir); err == nil {
		return "", errWorkspace // something else is there; never overwrite it
	}
	var exit int
	var err error
	if r.CloneWith == "git" {
		exit, _, err = r.run(ctx, filepath.Dir(dir), r.Git, "clone", "--quiet", "https://github.com/"+repo+".git", dir)
	} else {
		exit, _, err = r.run(ctx, filepath.Dir(dir), r.GH, "repo", "clone", repo, dir, "--", "--quiet")
	}
	if err != nil || exit != 0 {
		_ = os.RemoveAll(dir)
		return "", errWorkspace
	}
	return dir, nil
}

// DefaultBranch returns origin's default branch name.
func (r *Repos) DefaultBranch(ctx context.Context, clone string) (string, error) {
	out, err := r.git(ctx, clone, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		if _, err := r.git(ctx, clone, "remote", "set-head", "origin", "--auto"); err != nil {
			return "", errWorkspace
		}
		if out, err = r.git(ctx, clone, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err != nil {
			return "", errWorkspace
		}
	}
	b, ok := strings.CutPrefix(strings.TrimSpace(out), "origin/")
	if !ok || !refNameRE.MatchString(b) || strings.Contains(b, "..") {
		return "", errWorkspace
	}
	return b, nil
}

// BranchName is nexus/<task-id>-<short> (REQ-5).
func BranchName(taskID, jobID string) string {
	t := strings.ToLower(taskID)
	var sb strings.Builder
	for _, c := range t {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			sb.WriteRune(c)
		case c == '-' || c == '_' || c == '.':
			sb.WriteByte('-')
		}
	}
	t = strings.Trim(sb.String(), "-")
	if len(t) > 40 {
		t = strings.Trim(t[:40], "-")
	}
	if t == "" {
		t = "job"
	}
	short := strings.ReplaceAll(jobID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return branchPrefix + t + "-" + strings.ToLower(short)
}

// PrepareWorktree creates (or reuses, on a resumed attempt) the job's
// worktree on branch from origin/<defaultBranch>, and git-excludes the
// private .nexus directory. It returns the base commit.
func (r *Repos) PrepareWorktree(ctx context.Context, clone, jobID, branch, defaultBranch string) (string, string, error) {
	wt := r.WorktreePath(jobID)
	base, err := r.git(ctx, clone, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+defaultBranch+"^{commit}")
	base = strings.TrimSpace(base)
	if err != nil || !shaRE.MatchString(base) {
		return "", "", errWorkspace
	}
	if fi, err := os.Stat(filepath.Join(wt, ".git")); err == nil && !fi.IsDir() {
		cur, err := r.git(ctx, wt, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil || strings.TrimSpace(cur) != branch {
			return "", "", errWorkspace
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
			return "", "", errWorkspace
		}
		_, _ = r.git(ctx, clone, "worktree", "prune")
		if _, err := r.git(ctx, clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			_, err = r.git(ctx, clone, "worktree", "add", "--quiet", wt, branch)
			if err != nil {
				return "", "", errWorkspace
			}
		} else if _, err := r.git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-b", branch, wt, base); err != nil {
			return "", "", errWorkspace
		}
	}
	if err := r.excludePrivate(ctx, wt); err != nil {
		return "", "", errWorkspace
	}
	if err := os.MkdirAll(filepath.Join(wt, privateDirName), 0o700); err != nil {
		return "", "", errWorkspace
	}
	return wt, base, nil
}

func (r *Repos) excludePrivate(ctx context.Context, wt string) error {
	common, err := r.git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	path := filepath.Join(strings.TrimSpace(common), "info", "exclude")
	line := "/" + privateDirName + "/"
	if data, err := os.ReadFile(path); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == line {
				return nil
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + line + "\n")
	return err
}

// DiffStat is the change of a worktree against its base commit.
type DiffStat struct {
	Files, Added, Removed int64
}

// Changed is added + removed lines.
func (d DiffStat) Changed() int64 { return d.Added + d.Removed }

// StageAndDiff stages every change except .nexus and returns the diff
// against base (committed by the executor or not).
func (r *Repos) StageAndDiff(ctx context.Context, wt, base string) (DiffStat, error) {
	if _, err := r.git(ctx, wt, "add", "--all", "--", ".", ":(exclude)"+privateDirName,
		":(exclude,glob)**/.git", ":(exclude,glob)**/.git/**"); err != nil {
		return DiffStat{}, err
	}
	out, err := r.git(ctx, wt, "diff", "--cached", "--numstat", "--no-renames", base, "--")
	if err != nil {
		return DiffStat{}, err
	}
	var d DiffStat
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		d.Files++
		a, _ := strconv.ParseInt(f[0], 10, 64) // "-" for binary files counts 0
		b, _ := strconv.ParseInt(f[1], 10, 64)
		d.Added += a
		d.Removed += b
	}
	return d, nil
}

// CurrentBranch is the checked-out branch of wt.
func (r *Repos) CurrentBranch(ctx context.Context, wt string) (string, error) {
	out, err := r.git(ctx, wt, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// Commit commits the staged changes with the message in msgFile; nothing
// staged is not an error.
func (r *Repos) Commit(ctx context.Context, wt, msgFile string) error {
	exit, _, err := r.run(ctx, wt, r.Git, "diff", "--cached", "--quiet")
	if err != nil {
		return err
	}
	if exit == 0 {
		return nil // nothing staged (the executor committed, or no change)
	}
	_, err = r.git(ctx, wt, "commit", "--quiet", "-F", msgFile)
	return err
}

// Push pushes branch to origin. It refuses the default branch and any
// branch outside nexus/, and never forces.
func (r *Repos) Push(ctx context.Context, wt, branch, defaultBranch string) error {
	if !strings.HasPrefix(branch, branchPrefix) || branch == defaultBranch || !refNameRE.MatchString(branch) {
		return errors.New("refusing to push this branch")
	}
	_, err := r.git(ctx, wt, "push", "--quiet", "-u", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// CreateDraftPR opens a draft PR with the owner's gh login and returns
// its URL.
func (r *Repos) CreateDraftPR(ctx context.Context, wt, repo, branch, defaultBranch, title, bodyFile string) (string, error) {
	exit, out, err := r.run(ctx, wt, r.GH, "pr", "create", "--draft", "--repo", repo, "--head", branch,
		"--base", defaultBranch, "--title", title, "--body-file", bodyFile)
	if err != nil || exit != 0 {
		return "", errors.New("gh pr create failed")
	}
	u := prURLRE.FindString(out)
	if u == "" {
		return "", errors.New("gh pr create gave no URL")
	}
	return u, nil
}

// RemoveWorktree removes a finished job's worktree (local branch kept;
// it is on origin when pushed).
func (r *Repos) RemoveWorktree(ctx context.Context, clone, wt string) error {
	if clone != "" {
		if _, err := os.Stat(clone); err == nil {
			if _, err := r.git(ctx, clone, "worktree", "remove", "--force", wt); err == nil {
				_, _ = r.git(ctx, clone, "worktree", "prune")
				return nil
			}
		}
	}
	if !strings.HasPrefix(filepath.Clean(wt), filepath.Join(r.ReposDir, worktreesSubdir)+string(filepath.Separator)) {
		return errors.New("refusing to remove a path outside the worktrees directory")
	}
	if err := os.RemoveAll(wt); err != nil {
		return err
	}
	if clone != "" {
		_, _ = r.git(ctx, clone, "worktree", "prune")
	}
	return nil
}
