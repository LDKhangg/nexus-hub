package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

const w7Workspace = "019a6690-abcd-7000-8000-000000000001"

type w7CLI struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	store  *storage.WorkerStore
	srv    *httptest.Server
	home   string
	lookup func(string) (string, bool)
}

func newW7CLI(t *testing.T) *w7CLI {
	t.Helper()
	ctx, pool := setupDB(t)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'W')`, w7Workspace); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewWorkerStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ec := echo.New()
	httpdelivery.NewWorkerHandler(store, store).WithLeases(store, nil).Register(ec)
	srv := httptest.NewServer(ec)
	t.Cleanup(srv.Close)
	home := t.TempDir()
	return &w7CLI{ctx: ctx, pool: pool, store: store, srv: srv, home: home,
		lookup: func(k string) (string, bool) {
			if k == "HOME" {
				return home, true
			}
			return "", false
		}}
}

func (c *w7CLI) tokenPath() string {
	return filepath.Join(c.home, ".config", "nexus-hub", "worker-token")
}

func (c *w7CLI) enroll(t *testing.T, stdinText string, args ...string) (string, error) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(stdinText)
	defer func() { stdin = old }()
	var out bytes.Buffer
	err := run(append([]string{"worker", "enroll"}, args...), &out, c.lookup)
	return out.String(), err
}

func (c *w7CLI) code(t *testing.T) string {
	t.Helper()
	code, _, err := c.store.CreateEnrollCode(c.ctx, w7Workspace, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// T-50 (REQ-1): `nh worker enroll` stores the token 0600 in a 0700 directory and prints neither secret.
func TestWorkerCLI7a_EnrollWritesPrivateTokenFile(t *testing.T) {
	c := newW7CLI(t)
	code := c.code(t)
	out, err := c.enroll(t, code+"\n", "--hub", c.srv.URL, "--name", "laptop", "--capabilities", "repo.write, build.go")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	raw, err := os.ReadFile(c.tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, "nhw_") {
		t.Fatalf("token file content: %q", token[:min(len(token), 6)])
	}
	if fi, _ := os.Stat(c.tokenPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(c.tokenPath())); fi.Mode().Perm() != 0o700 {
		t.Fatalf("token dir mode %v", fi.Mode().Perm())
	}
	for _, secret := range []string{token, code, strings.ReplaceAll(code, "-", "")} {
		if strings.Contains(out, secret) {
			t.Fatalf("output leaks a secret: %q", out)
		}
	}
	if !strings.Contains(out, "laptop") {
		t.Fatalf("output does not say what happened: %q", out)
	}
	dev, err := c.store.Authenticate(c.ctx, token)
	if err != nil || dev.Name != "laptop" || strings.Join(dev.Capabilities, ",") != "repo.write,build.go" {
		t.Fatalf("stored token does not authenticate: %+v %v", dev, err)
	}
	var n int
	c.pool.QueryRow(c.ctx, `SELECT count(*) FROM worker_devices d WHERE d::text LIKE '%'||$1||'%'`, token).Scan(&n)
	if n != 0 {
		t.Fatal("hub stored the plaintext token")
	}
}

// T-51 (REQ-1): mistakes give a short clear error, leave no token file, and do not burn the code.
func TestWorkerCLI7a_EnrollMistakes(t *testing.T) {
	c := newW7CLI(t)
	code := c.code(t)
	if _, err := c.enroll(t, "AAAA-BBBB-CCCC\n", "--hub", c.srv.URL, "--name", "x"); err == nil || !strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("wrong code: %v", err)
	}
	for _, in := range []string{"", "   \n", strings.Repeat("A", 65)} {
		if _, err := c.enroll(t, in, "--hub", c.srv.URL, "--name", "x"); err == nil {
			t.Fatalf("code %q accepted", in[:min(len(in), 5)])
		}
	}
	for _, hub := range []string{"http://example.com", "ftp://127.0.0.1", "127.0.0.1:1", "http://user:pw@127.0.0.1:9", "https://", "http://127.0.0.1:1/?x=1"} {
		if _, err := c.enroll(t, code, "--hub", hub, "--name", "x"); err == nil {
			t.Fatalf("hub %q accepted", hub)
		}
	}
	if _, err := c.enroll(t, code, "--name", "x"); err == nil {
		t.Fatal("missing --hub accepted")
	}
	if _, err := c.enroll(t, code, "--hub", c.srv.URL, "extra"); err == nil {
		t.Fatal("stray argument accepted")
	}
	if _, err := os.Stat(c.tokenPath()); err == nil {
		t.Fatal("a failed enroll left a token file")
	}
	// unreachable hub: no code in the error
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()
	if _, err := c.enroll(t, code, "--hub", deadURL, "--name", "x"); err == nil || strings.Contains(err.Error(), code) {
		t.Fatalf("unreachable hub: %v", err)
	}
	// name clash: clear hint, code not burned
	if _, err := c.enroll(t, code, "--hub", c.srv.URL, "--name", "laptop"); err != nil {
		t.Fatal(err)
	}
	code2 := c.code(t)
	_, err := c.enroll(t, code2, "--hub", c.srv.URL, "--name", "laptop")
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("name clash message: %v", err)
	}
	if _, err := c.enroll(t, code2, "--hub", c.srv.URL, "--name", "laptop-2"); err != nil {
		t.Fatalf("code burned by a name clash: %v", err)
	}
	// default name works (derived from the host name, always a valid name)
	code3 := c.code(t)
	if _, err := c.enroll(t, code3, "--hub", c.srv.URL); err != nil {
		t.Fatalf("default device name refused: %v", err)
	}
}

// T-52 (REQ-1): re-enrolling replaces an older, too-open token file with a 0600 one.
func TestWorkerCLI7a_EnrollReplacesLooseTokenFile(t *testing.T) {
	c := newW7CLI(t)
	if err := os.MkdirAll(filepath.Dir(c.tokenPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.tokenPath(), []byte("old-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.enroll(t, c.code(t), "--hub", c.srv.URL, "--name", "laptop"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.tokenPath())
	fi, _ := os.Stat(c.tokenPath())
	if strings.Contains(string(raw), "old-token") || !strings.HasPrefix(string(raw), "nhw_") || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v content=%q", fi.Mode().Perm(), string(raw)[:min(len(raw), 8)])
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(c.tokenPath()), ".worker-token-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// T-56 (REQ-1, 12): nh worker devices | revoke | jobs.
func TestWorkerCLI7a_DevicesRevokeJobs(t *testing.T) {
	c := newW7CLI(t)
	code := c.code(t)
	dev, _, err := c.store.Enroll(c.ctx, code, "laptop", []string{"repo.write"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := c.store.CreateJob(c.ctx, w7Workspace, 0, "step", nil, json.RawMessage(`{"instruction":"INSTRUCTION-TEXT-QQQ"}`))
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := c.store.LeaseJob(c.ctx, dev)
	if err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if _, _, err := c.store.ReportResult(c.ctx, dev, job.ID, lease.LeaseToken, "k", behavior.WorkerResult{Status: "failed", ReasonCode: "tests_failed", Summary: "SUMMARY-TEXT-QQQ"}); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runCommand(c.ctx, c.pool, append([]string{"worker"}, args...), &out, c.lookup)
		return out.String(), err
	}
	out, err := cli("devices", "--workspace-id", w7Workspace)
	if err != nil || !strings.Contains(out, "laptop") || !strings.Contains(out, "repo.write") {
		t.Fatalf("devices: %q %v", out, err)
	}
	out, err = cli("devices", "--workspace-id", w7Workspace, "--json")
	var rows []map[string]any
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["name"] != "laptop" {
		t.Fatalf("devices --json: %q %v", out, err)
	}
	if _, ok := rows[0]["token"]; ok || strings.Contains(out, "token_hash") {
		t.Fatalf("devices --json exposes token data: %s", out)
	}
	out, err = cli("jobs", "--workspace-id", w7Workspace)
	if err != nil || !strings.Contains(out, job.ID) || !strings.Contains(out, "FAILED") || !strings.Contains(out, "tests_failed") || !strings.Contains(out, "laptop") {
		t.Fatalf("jobs: %q %v", out, err)
	}
	for _, banned := range []string{"SUMMARY-TEXT", "INSTRUCTION-TEXT", lease.LeaseToken} {
		if strings.Contains(out, banned) {
			t.Fatalf("jobs output leaks %q", banned)
		}
	}
	out, err = cli("jobs", "--workspace-id", w7Workspace, "--json")
	var parsed struct {
		Jobs   []map[string]any `json:"jobs"`
		Events []map[string]any `json:"events"`
	}
	if err != nil || json.Unmarshal([]byte(out), &parsed) != nil || len(parsed.Jobs) != 1 || len(parsed.Events) < 3 {
		t.Fatalf("jobs --json: %q %v", out, err)
	}
	if strings.Contains(out, "SUMMARY-TEXT") || strings.Contains(out, "INSTRUCTION-TEXT") {
		t.Fatal("jobs --json leaks text")
	}
	// revoke
	if out, err := cli("revoke", "--workspace-id", w7Workspace, "laptop"); err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %q %v", out, err)
	}
	if _, err := cli("revoke", "--workspace-id", w7Workspace, "laptop"); err == nil || !strings.Contains(err.Error(), "no active device") {
		t.Fatalf("second revoke: %v", err)
	}
	out, _ = cli("devices", "--workspace-id", w7Workspace)
	if !strings.Contains(out, "revoked") {
		t.Fatalf("devices after revoke: %q", out)
	}
	// mistakes
	for _, args := range [][]string{
		{}, {"bogus"}, {"devices"}, {"devices", "--workspace-id", "not-a-uuid"}, {"revoke", "--workspace-id", w7Workspace},
		{"jobs", "--workspace-id", w7Workspace, "--limit", "0"}, {"jobs", "--workspace-id", w7Workspace, "--limit", "201"},
		{"jobs", "--workspace-id", w7Workspace, "extra"},
	} {
		if _, err := cli(args...); err == nil {
			t.Fatalf("mistake accepted: %v", args)
		}
	}
}
