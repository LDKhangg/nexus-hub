package codetools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const testToken = "ghs_testtoken1234567890abcdef"

type recordingRunner struct {
	inner    *execrunner.Runner
	delay    time.Duration
	mu       sync.Mutex
	cmds     []execrunner.Command
	inflight int32
	maxIn    int32
}

func (r *recordingRunner) Run(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	n := atomic.AddInt32(&r.inflight, 1)
	for {
		m := atomic.LoadInt32(&r.maxIn)
		if n <= m || atomic.CompareAndSwapInt32(&r.maxIn, m, n) {
			break
		}
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	defer atomic.AddInt32(&r.inflight, -1)
	return r.inner.Run(ctx, c)
}

func (r *recordingRunner) all() []execrunner.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execrunner.Command(nil), r.cmds...)
}

type fakeTokens struct {
	mu    sync.Mutex
	perms []map[string]string
}

func (f *fakeTokens) Token(_ context.Context, ws string, acc tenant.GitHubAccount, repo string, perms map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perms = append(f.perms, perms)
	return testToken, nil
}

type testSan struct{}

func (testSan) CleanOutbound(s string, _ bool) string {
	return strings.ReplaceAll(s, testToken, "[REDACTED]")
}

type wsEnv struct {
	reposDir, remoteDir, src, clone string
	rec                             *recordingRunner
	tokens                          *fakeTokens
	ts                              []tools.Tool
}

// newWS creates a source repo with a commit on defaultBranch, a bare remote
// at <remoteDir>/acme/app.git and workspace tools pointing at it.
func newWS(t *testing.T, defaultBranch string, maxClone int64) *wsEnv {
	t.Helper()
	e := &wsEnv{reposDir: t.TempDir(), remoteDir: t.TempDir(), src: t.TempDir()}
	sh(t, e.src, "init", "-q", "-b", defaultBranch)
	writeFile(t, e.src, "README.md", "hello\n")
	writeFile(t, e.src, ".gitignore", "ignored.log\n")
	sh(t, e.src, "add", "-A")
	sh(t, e.src, "commit", "-qm", "initial")
	bare := filepath.Join(e.remoteDir, "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, e.src, "clone", "-q", "--bare", e.src, bare)
	sh(t, e.src, "remote", "add", "origin", bare)
	e.clone = filepath.Join(e.reposDir, "acme", "app")
	e.rec = &recordingRunner{inner: newTestRunner(t)}
	e.tokens = &fakeTokens{}
	ws := newWorkspaceTools(e.rec, e.tokens, e.reposDir, maxClone, testSan{}, "file://"+e.remoteDir+"/")
	e.ts = ws
	return e
}

func (e *wsEnv) tool(t *testing.T, name string) tools.Tool { return findTool(t, e.ts, name) }

func wsGrant() *tenant.RepoGrant {
	g := grant(true, "")
	g.MergeBaseBranches = []string{"develop"}
	return g
}

func (e *wsEnv) sync(t *testing.T) string {
	t.Helper()
	return mustOK(t, e.tool(t, "git_sync"), wsGrant(), `{"repo":"acme/app"}`)
}

func TestSync_ClonesThenFetches(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	if out := e.sync(t); !strings.Contains(out, "cloned") {
		t.Fatalf("first sync: %s", out)
	}
	if out := e.sync(t); !strings.Contains(out, "fetched") {
		t.Fatalf("second sync: %s", out)
	}
	cfg, err := os.ReadFile(filepath.Join(e.clone, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), testToken) || strings.Contains(string(cfg), "extraheader") {
		t.Fatalf("token persisted in config:\n%s", cfg)
	}
	for _, c := range e.rec.all() {
		if strings.Contains(strings.Join(c.Args, " "), testToken) {
			t.Fatalf("token in argv: %v", c.Args)
		}
	}
	if len(e.tokens.perms) == 0 || e.tokens.perms[0]["contents"] != "read" {
		t.Fatalf("perms %v", e.tokens.perms)
	}
}

func TestSync_TokenOnlyInEnv(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	want := "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))
	found := false
	for _, c := range e.rec.all() {
		for _, kv := range c.Env {
			if kv == want {
				found = true
			}
		}
		if c.Network != containsAny(c.Args, "clone", "fetch") {
			t.Fatalf("network flag wrong for %v", c.Args)
		}
	}
	if !found {
		t.Fatal("token header not passed via env")
	}
}

func containsAny(args []string, subs ...string) bool {
	for _, a := range args {
		for _, s := range subs {
			if a == s {
				return true
			}
		}
	}
	return false
}

func TestSync_CloneTooLarge(t *testing.T) {
	e := newWS(t, "main", 10)
	_, err := run(t, e.tool(t, "git_sync"), wsGrant(), `{"repo":"acme/app"}`)
	wantCode(t, err, "clone_too_large")
	if _, err := os.Stat(e.clone); !os.IsNotExist(err) {
		t.Fatal("oversized clone not removed")
	}
}

func TestSync_RefusesNonManaged(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	for _, name := range []string{"git_sync", "git_pull", "git_checkout", "git_commit", "git_config_set", "git_push", "git_reset_hard", "git_clean"} {
		_, err := run(t, e.tool(t, name), grant(false, e.src), `{"repo":"acme/app"}`)
		wantCode(t, err, "not_managed")
	}
}

func TestPull_FFOnly(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.src, "README.md", "remote change\n")
	sh(t, e.src, "commit", "-qam", "remote")
	sh(t, e.src, "push", "-q", "origin", "main")
	if out := mustOK(t, e.tool(t, "git_pull"), wsGrant(), `{"repo":"acme/app"}`); !strings.Contains(out, "pulled") {
		t.Fatal(out)
	}
	writeFile(t, e.clone, "local.txt", "x")
	sh(t, e.clone, "add", "local.txt")
	sh(t, e.clone, "commit", "-qm", "local")
	localHead := sh(t, e.clone, "rev-parse", "HEAD")
	writeFile(t, e.src, "README.md", "remote change 2\n")
	sh(t, e.src, "commit", "-qam", "remote2")
	sh(t, e.src, "push", "-q", "origin", "main")
	_, err := run(t, e.tool(t, "git_pull"), wsGrant(), `{"repo":"acme/app"}`)
	wantCode(t, err, "pull_not_fast_forward")
	if sh(t, e.clone, "rev-parse", "HEAD") != localHead {
		t.Fatal("HEAD moved")
	}
}

func TestCheckout_CreateBranch(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	mustOK(t, e.tool(t, "git_checkout"), wsGrant(), `{"repo":"acme/app","branch":"nexus/fix-1","create":true,"start_point":"origin/main"}`)
	if b := sh(t, e.clone, "branch", "--show-current"); b != "nexus/fix-1" {
		t.Fatalf("on %s", b)
	}
	mustOK(t, e.tool(t, "git_checkout"), wsGrant(), `{"repo":"acme/app","branch":"main"}`)
	if b := sh(t, e.clone, "branch", "--show-current"); b != "main" {
		t.Fatalf("on %s", b)
	}
}

func TestCheckout_InvalidBranch(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	for _, b := range []string{"-f", "a..b", "x.lock", "HEAD", "a b", "--orphan"} {
		in, _ := json.Marshal(map[string]any{"repo": "acme/app", "branch": b, "create": true})
		_, err := run(t, e.tool(t, "git_checkout"), wsGrant(), string(in))
		wantCode(t, err, "invalid_input")
	}
}

func TestCommit_IdentityFromAccount(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.clone, "new.txt", "content\n")
	mustOK(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["new.txt"],"message":"add new file"}`)
	got := sh(t, e.clone, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s")
	if got != "Nexus Bot <nexus@example.invalid>|Nexus Bot <nexus@example.invalid>|add new file" {
		t.Fatalf("got %q", got)
	}
}

func TestCommit_DenyListedPathRefused(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.clone, ".env", "SECRET=1\n")
	writeFile(t, e.clone, "ok.txt", "ok\n")
	_, err := run(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["ok.txt",".env"],"message":"m"}`)
	wantCode(t, err, "denied_path")
	if staged := sh(t, e.clone, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("staged: %s", staged)
	}
	for _, in := range []string{`{"repo":"acme/app","paths":[],"message":"m"}`, `{"repo":"acme/app","paths":["ok.txt"],"message":""}`, `{"repo":"acme/app","paths":["../x"],"message":"m"}`, `{"repo":"acme/app","paths":["."],"message":"m"}`} {
		_, err := run(t, e.tool(t, "git_commit"), wsGrant(), in)
		wantCode(t, err, "invalid_input")
	}
}

func TestCommit_MessageSanitized(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.clone, "a.txt", "a\n")
	mustOK(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["a.txt"],"message":"leak `+testToken+` here"}`)
	if msg := sh(t, e.clone, "log", "-1", "--format=%B"); strings.Contains(msg, testToken) || !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("msg %q", msg)
	}
}

func TestCommit_HooksNotRun(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	marker := filepath.Join(t.TempDir(), "HOOK_RAN")
	hook := filepath.Join(e.clone, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.clone, "a.txt", "a\n")
	mustOK(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["a.txt"],"message":"m"}`)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook executed")
	}
}

func TestConfigSet_AllowlistOnly(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	mustOK(t, e.tool(t, "git_config_set"), wsGrant(), `{"repo":"acme/app","key":"user.name","value":"Bot"}`)
	if v := sh(t, e.clone, "config", "--local", "user.name"); v != "Bot" {
		t.Fatalf("got %q", v)
	}
	mustOK(t, e.tool(t, "git_config_set"), wsGrant(), `{"repo":"acme/app","key":"branch.nexus/x.merge","value":"refs/heads/nexus/x"}`)
	for _, key := range []string{"core.fsmonitor", "credential.helper", "url.x.insteadOf", "alias.x", "remote.origin.url", "core.hooksPath", "branch.a..b.merge"} {
		in, _ := json.Marshal(map[string]string{"repo": "acme/app", "key": key, "value": "/tmp/x"})
		_, err := run(t, e.tool(t, "git_config_set"), wsGrant(), string(in))
		wantCode(t, err, "config_key_denied")
	}
	_, err := run(t, e.tool(t, "git_config_set"), wsGrant(), `{"repo":"acme/app","key":"pull.ff","value":"sometimes"}`)
	wantCode(t, err, "invalid_input")
}

func TestPush_PatternEnforced(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	mustOK(t, e.tool(t, "git_checkout"), wsGrant(), `{"repo":"acme/app","branch":"nexus/feat","create":true}`)
	if out := mustOK(t, e.tool(t, "git_push"), wsGrant(), `{"repo":"acme/app","branch":"nexus/feat"}`); !strings.Contains(out, "pushed") {
		t.Fatal(out)
	}
	if got := sh(t, e.src, "ls-remote", "origin", "refs/heads/nexus/feat"); got == "" {
		t.Fatal("branch not on remote")
	}
	_, err := run(t, e.tool(t, "git_push"), wsGrant(), `{"repo":"acme/app","branch":"feature/x"}`)
	wantCode(t, err, "branch_not_allowed")
	if p := e.tokens.perms[len(e.tokens.perms)-1]; p["contents"] != "write" {
		t.Fatalf("push perms %v", p)
	}
}

func TestPush_EscalatesProtected(t *testing.T) {
	e := newWS(t, "trunk", 1<<30)
	e.sync(t)
	esc := e.tool(t, "git_push").(tools.TierEscalator)
	g := wsGrant()
	for branch, want := range map[string]tenant.Tier{"main": tenant.TierDangerous, "master": tenant.TierDangerous, "develop": tenant.TierDangerous, "trunk": tenant.TierDangerous, "nexus/x": tenant.TierWrite} {
		if got := esc.EffectiveTier(json.RawMessage(`{"repo":"acme/app","branch":"`+branch+`"}`), g); got != want {
			t.Errorf("%s: got %s want %s", branch, got, want)
		}
	}
}

func TestPush_NeverForce(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	mustOK(t, e.tool(t, "git_checkout"), wsGrant(), `{"repo":"acme/app","branch":"nexus/x","create":true}`)
	writeFile(t, e.clone, "a.txt", "1\n")
	sh(t, e.clone, "add", "a.txt")
	sh(t, e.clone, "commit", "-qm", "one")
	mustOK(t, e.tool(t, "git_push"), wsGrant(), `{"repo":"acme/app","branch":"nexus/x"}`)
	sh(t, e.clone, "reset", "-q", "--hard", "HEAD~1")
	writeFile(t, e.clone, "b.txt", "2\n")
	sh(t, e.clone, "add", "b.txt")
	sh(t, e.clone, "commit", "-qm", "two")
	_, err := run(t, e.tool(t, "git_push"), wsGrant(), `{"repo":"acme/app","branch":"nexus/x"}`)
	wantCode(t, err, "push_rejected")
	for _, c := range e.rec.all() {
		if !containsAny(c.Args, "push") {
			continue
		}
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--force") || a == "-f" || strings.HasPrefix(a, "+") || a == "--delete" || a == "--mirror" {
				t.Fatalf("forbidden push arg %q", a)
			}
		}
	}
}

func TestResetHard_ManagedOnlyAndValidRef(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.clone, "README.md", "dirty\n")
	mustOK(t, e.tool(t, "git_reset_hard"), wsGrant(), `{"repo":"acme/app","ref":"origin/main"}`)
	if b, _ := os.ReadFile(filepath.Join(e.clone, "README.md")); string(b) != "hello\n" {
		t.Fatalf("not reset: %q", b)
	}
	for _, ref := range []string{"--hard", "-q", "a..b", ""} {
		in, _ := json.Marshal(map[string]string{"repo": "acme/app", "ref": ref})
		_, err := run(t, e.tool(t, "git_reset_hard"), wsGrant(), string(in))
		wantCode(t, err, "invalid_input")
	}
}

func TestClean_NoIgnoredFilesRemoved(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	writeFile(t, e.clone, "ignored.log", "keep\n")
	writeFile(t, e.clone, "junk/untracked.txt", "remove\n")
	mustOK(t, e.tool(t, "git_clean"), wsGrant(), `{"repo":"acme/app"}`)
	if _, err := os.Stat(filepath.Join(e.clone, "ignored.log")); err != nil {
		t.Fatal("ignored file removed")
	}
	if _, err := os.Stat(filepath.Join(e.clone, "junk")); !os.IsNotExist(err) {
		t.Fatal("untracked dir kept")
	}
}

func TestWorkspace_PerRepoSerialized(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	e.rec.delay = 80 * time.Millisecond
	atomic.StoreInt32(&e.rec.maxIn, 0)
	var wg sync.WaitGroup
	for _, name := range []string{"git_clean", "git_clean", "git_clean"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, err := run(t, e.tool(t, name), wsGrant(), `{"repo":"acme/app"}`); err != nil {
				t.Error(err)
			}
		}(name)
	}
	wg.Wait()
	if m := atomic.LoadInt32(&e.rec.maxIn); m != 1 {
		t.Fatalf("max concurrent git on one repo = %d", m)
	}
}

func TestWorkspace_SpecsAndTiers(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	tiers := map[string]tenant.Tier{}
	for _, tl := range e.ts {
		tiers[tl.Spec().Name] = tl.Spec().Tier
		if _, ok := tl.(tools.RepoScoped); !ok {
			t.Errorf("%s not repo scoped", tl.Spec().Name)
		}
	}
	want := map[string]tenant.Tier{"git_sync": "write", "git_pull": "write", "git_checkout": "write", "git_commit": "write", "git_config_set": "write", "git_push": "write", "git_reset_hard": "dangerous", "git_clean": "dangerous"}
	for k, v := range want {
		if tiers[k] != v {
			t.Errorf("%s tier %s", k, tiers[k])
		}
	}
	for _, name := range []string{"git_sync", "git_pull", "git_push"} {
		if nt, ok := e.tool(t, name).(tools.NetworkTool); !ok || !nt.UsesNetwork() {
			t.Errorf("%s not a network tool", name)
		}
	}
	if nt, ok := e.tool(t, "git_commit").(tools.NetworkTool); ok && nt.UsesNetwork() {
		t.Error("git_commit marked network")
	}
}
