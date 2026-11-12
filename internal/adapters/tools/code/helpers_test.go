package codetools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const testRepo = "acme/app"

func gitPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	p, _ = filepath.EvalSymlinks(p)
	return p
}

func newTestRunner(t *testing.T) *execrunner.Runner {
	t.Helper()
	r, err := execrunner.New(gitPath(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// sh runs a setup git command directly (test fixture only).
func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitPath(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@example.com", "GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// managedRepo creates <reposDir>/acme/app as a git work tree with files committed.
func managedRepo(t *testing.T, files map[string]string) (reposDir, root string) {
	t.Helper()
	reposDir = t.TempDir()
	root = filepath.Join(reposDir, "acme", "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, root, "init", "-q", "-b", "main")
	for k, v := range files {
		writeFile(t, root, k, v)
	}
	sh(t, root, "add", "-A")
	sh(t, root, "commit", "-qm", "initial")
	return reposDir, root
}

func grant(managed bool, localPath string) *tenant.RepoGrant {
	return &tenant.RepoGrant{Repo: testRepo, Account: "acc", Managed: managed, LocalPath: localPath,
		PushBranches: []string{"nexus/*"}, ProtectedBranches: []string{"main", "master"}}
}

func findTool(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range ts {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s missing", name)
	return nil
}

func run(t *testing.T, tl tools.Tool, g *tenant.RepoGrant, input string) (tools.ToolOutput, error) {
	t.Helper()
	return tl.Run(context.Background(), tools.Call{Input: json.RawMessage(input), Repo: g,
		Account: &tenant.GitHubAccount{Name: "acc", Type: "pat", Login: "bot", CommitName: "Nexus Bot", CommitEmail: "nexus@example.invalid"}, WorkspaceID: "ws"})
}

func mustOK(t *testing.T, tl tools.Tool, g *tenant.RepoGrant, input string) string {
	t.Helper()
	out, err := run(t, tl, g, input)
	if err != nil {
		t.Fatalf("%s %s: %v", tl.Spec().Name, input, err)
	}
	return out.Text
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var te *tools.ToolError
	if !errors.As(err, &te) || te.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
