// Package codetools provides hardened git code-reading and managed-clone
// workspace tools for the agent.
package codetools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// hardenedArgs prefixes a git subcommand with the options that disable
// every repo-config code-execution vector and network transports.
func hardenedArgs(root string, sub ...string) []string {
	args := []string{
		"-C", root, "--no-pager",
		"-c", "core.fsmonitor=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "diff.external=",
		"-c", "core.pager=cat",
		"-c", "safe.directory=" + root,
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "core.sshCommand=",
		"-c", "uploadpack.packObjectsHook=",
		// No signing or editor program may run, and the remote cannot
		// redirect a request elsewhere. Filter drivers cannot be disabled
		// here; workspace.verifyConfig rejects any filter.* key instead.
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "gpg.program=false",
		"-c", "gpg.ssh.program=false",
		"-c", "gpg.x509.program=false",
		"-c", "core.editor=false",
		"-c", "sequence.editor=false",
		"-c", "http.followRedirects=false",
	}
	return append(args, sub...)
}

func toolErr(code string) error { return &tools.ToolError{Code: code} }

// deniedErr is a refusal on policy grounds; the executor audits it as denied.
func deniedErr(code string) error { return &tools.ToolError{Code: code, Denied: true} }

var refRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// validRef applies the syntactic ref rules (HEAD, branch names, SHAs).
func validRef(s string) bool {
	if !refRe.MatchString(s) || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	return !strings.HasSuffix(s, ".lock") && !strings.HasSuffix(s, "/")
}

// validBranch is validRef for branch names (git check-ref-format --branch
// is applied by the workspace tools at run time).
func validBranch(s string) bool { return tenant.ValidBranchName(s) && validRef(s) && s != "HEAD" }

// cleanRepoPath validates a repository-relative path. Empty is allowed only
// when allowRoot is set.
func cleanRepoPath(p string, allowRoot bool) (string, error) {
	if p == "" || p == "." {
		if allowRoot {
			return "", nil
		}
		return "", toolErr("invalid_input")
	}
	if strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.Contains(p, "\\") {
		return "", toolErr("invalid_input")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", toolErr("invalid_input")
		}
	}
	c := path.Clean(p)
	if c == "." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "-") || len(c) > 1024 {
		return "", toolErr("invalid_input")
	}
	return c, nil
}

// deniedPath reports whether a repo path may hold secrets.
func deniedPath(p string) bool { return tools.DeniedPath(p) }

// deniedPathspecs exclude secret-shaped paths from diff and log output.
var deniedPathspecs = []string{
	":(exclude,icase,glob)**/.env", ":(exclude,icase,glob)**/.env.*", ":(exclude,icase,glob)**/*.pem",
	":(exclude,icase,glob)**/*.key", ":(exclude,icase,glob)**/id_rsa*", ":(exclude,icase,glob)**/id_ed25519*",
	":(exclude,icase,glob)**/secrets/**", ":(exclude,icase,glob)**/*credentials*", ":(exclude,icase,glob)**/*.p12",
	":(exclude,icase,glob)**/*.kdbx",
}

// cloneRoot returns the work tree a grant reads from: the managed clone
// when grant.Managed, otherwise local_path.
func cloneRoot(grant *tenant.RepoGrant, reposDir string) (string, error) {
	if grant == nil {
		return "", toolErr("repo_unavailable")
	}
	root := grant.LocalPath
	if grant.Managed {
		var err error
		if root, err = managedPath(reposDir, grant.Repo); err != nil {
			return "", err
		}
	}
	if root == "" {
		return "", toolErr("repo_unavailable")
	}
	if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || !st.IsDir() {
		return "", toolErr("repo_unavailable")
	}
	return root, nil
}

// managedPath is <reposDir>/<owner>/<repo> for a validated repo name.
func managedPath(reposDir, repo string) (string, error) {
	if !tenant.ValidRepo(repo) || reposDir == "" || !filepath.IsAbs(reposDir) {
		return "", toolErr("repo_unavailable")
	}
	owner, name, _ := strings.Cut(repo, "/")
	return filepath.Join(reposDir, owner, name), nil
}

// Runner is the subset of *execrunner.Runner the tools use.
type Runner interface {
	Run(ctx context.Context, c execrunner.Command) (execrunner.Result, error)
}

// git runs a hardened git command and maps runner failures to tool errors.
func git(ctx context.Context, r Runner, root string, cmd execrunner.Command, sub ...string) (execrunner.Result, error) {
	cmd.Args = hardenedArgs(root, sub...)
	return runGit(ctx, r, cmd)
}

// runGit runs a fully built command and maps runner failures to tool errors.
func runGit(ctx context.Context, r Runner, cmd execrunner.Command) (execrunner.Result, error) {
	res, err := r.Run(ctx, cmd)
	if err != nil {
		var de *execrunner.DeniedError
		if errors.As(err, &de) {
			return res, deniedErr(de.Code)
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, toolErr("git_unavailable")
	}
	if res.TimedOut {
		return res, toolErr("timeout")
	}
	return res, nil
}

var notFoundRe = regexp.MustCompile(`(?i)(unknown revision|bad revision|does not exist|not a valid object|invalid object name|bad object|not a tree object|no such path|ambiguous argument|exists on disk, but not in)`)

// gitErr maps a failed git exit to a safe code; stderr is never surfaced.
func gitErr(res execrunner.Result) error {
	if notFoundRe.Match(res.Stderr) {
		return toolErr("not_found")
	}
	return toolErr("git_failed")
}

// input is the union of every code tool's input fields.
type input struct {
	Repo       string   `json:"repo"`
	Ref        string   `json:"ref"`
	Path       string   `json:"path"`
	Query      string   `json:"query"`
	Regex      bool     `json:"regex"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Max        int      `json:"max"`
	Base       string   `json:"base"`
	Head       string   `json:"head"`
	Branch     string   `json:"branch"`
	Create     bool     `json:"create"`
	StartPoint string   `json:"start_point"`
	Paths      []string `json:"paths"`
	Message    string   `json:"message"`
	Key        string   `json:"key"`
	Value      string   `json:"value"`
}

func decode(raw json.RawMessage, allowed ...string) (input, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return input{}, toolErr("invalid_input")
	}
	ok := map[string]bool{"repo": true}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range probe {
		if !ok[k] {
			return input{}, toolErr("invalid_input")
		}
	}
	var in input
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&in); err != nil || !tenant.ValidRepo(in.Repo) {
		return input{}, toolErr("invalid_input")
	}
	return in, nil
}

// repoOf implements tools.RepoScoped for every code tool.
func repoOf(raw json.RawMessage) (string, error) {
	var v struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || !tenant.ValidRepo(v.Repo) {
		return "", errors.New("invalid repo")
	}
	return v.Repo, nil
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(append([]string{"repo"}, required...))
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"repo":{"type":"string","description":"owner/name"}` + props + `},"required":` + string(req) + `}`)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
