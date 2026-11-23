package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TC-Fb-10 (REQ-9).
func TestCIEnforcesGeneratedMigrationAndConfigurationDrift(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{"make generate", "git status --porcelain", "goose -dir db/migrations validate", "make config-doc-check"} {
		if !strings.Contains(workflow, required) {
			t.Errorf("CI lacks required check %q", required)
		}
	}
	versionMatch := regexp.MustCompile(`SQLC_VERSION:\s*(v[0-9]+\.[0-9]+\.[0-9]+)`).FindStringSubmatch(workflow)
	if len(versionMatch) != 2 {
		t.Fatal("CI has no pinned SQLC_VERSION")
	}
	headerBytes, err := os.ReadFile("../../internal/adapters/storage/sqlc/models.go")
	if err != nil {
		t.Fatal(err)
	}
	headerMatch := regexp.MustCompile(`(?m)^//\s+sqlc (v[0-9]+\.[0-9]+\.[0-9]+)$`).FindStringSubmatch(string(headerBytes))
	if len(headerMatch) != 2 || headerMatch[1] != versionMatch[1] {
		t.Fatalf("generated sqlc version %q differs from CI pin %q", headerMatch, versionMatch[1])
	}
}

// TC-Fb-12 (REQ-10).
func TestGitignoreCoversLocalSecretsKeysAndDumps(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{".env", ".env.local", "secrets/service.key", "backup/nexus.dump"}
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gitignore check failed: %v: %s", err, stderr.String())
	}
	ignored := make(map[string]bool)
	for _, path := range strings.Fields(stdout.String()) {
		ignored[path] = true
	}
	for _, path := range paths {
		if !ignored[path] {
			t.Errorf("sensitive path %q is not ignored", path)
		}
	}
}
