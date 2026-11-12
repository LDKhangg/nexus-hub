package codetools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// TC-39 (REQ-F4), TC-55 (REQ-F15): independently audited existing assertions.
func TestApproved_NormalCommitAndSmallWorkspace(t *testing.T) {
	t.Run("commit_identity", TestCommit_IdentityFromAccount)
	t.Run("clone_fetch", TestSync_ClonesThenFetches)
	t.Run("pull", TestPull_FFOnly)
}

// TC-16 (REQ-7), TC-34 (REQ-F1): read tools must not execute repo programs either.
func TestApproved_StatusCannotExecuteCleanFilter(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	marker := filepath.Join(t.TempDir(), "executed")
	program := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf executed > '"+marker+"'\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.clone, ".gitattributes", "README.md filter=attack\n")
	sh(t, e.clone, "config", "--local", "filter.attack.clean", program)
	read := NewReadTools(newTestRunner(t), e.reposDir)
	_, err := run(t, findTool(t, read, "git_status"), wsGrant(), `{"repo":"acme/app"}`)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Error("repository-selected clean filter executed during git_status")
	}
	if err == nil {
		t.Error("git_status accepted unsafe repository configuration")
	}
}

// TC-16, TC-34 (REQ-7, REQ-F1), D1: dropping any read-config gate must
// expose a denial failure, even for keys that do not execute on this platform.
func TestApproved_StatusRejectsEveryProgramConfigChannel(t *testing.T) {
	for _, key := range []string{"filter.attack.clean", "filter.attack.smudge", "filter.attack.process", "diff.attack.textconv", "diff.attack.command", "merge.attack.driver", "core.fsmonitor", "core.worktree", "include.path", "includeIf.gitdir:/.path", "extensions.worktreeConfig", "diff.external"} {
		t.Run(key, func(t *testing.T) {
			e := newWS(t, "main", 1<<30)
			e.sync(t)
			marker := filepath.Join(t.TempDir(), "executed")
			program := filepath.Join(t.TempDir(), "program")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf executed > '"+marker+"'\ncat\n"), 0700); err != nil {
				t.Fatal(err)
			}
			value := program
			if key == "extensions.worktreeConfig" {
				value = "true"
			}
			sh(t, e.clone, "config", "--local", key, value)
			_, err := run(t, findTool(t, NewReadTools(newTestRunner(t), e.reposDir), "git_status"), wsGrant(), `{"repo":"acme/app"}`)
			wantCode(t, err, "unsafe_repo_config")
			var te *tools.ToolError
			if !errors.As(err, &te) || !te.Denied {
				t.Error("unsafe status config is not a policy denial")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("repository program executed")
			}
		})
	}
}

// TC-33 (REQ-F1), TC-34 (REQ-F1): actual mutations, not just argv assertions.
func TestApproved_RepositoryProgramsNeverExecute(t *testing.T) {
	for _, key := range []string{"filter.attack.clean", "filter.attack.smudge", "filter.attack.process", "gpg.program", "core.hooksPath", "core.fsmonitor", "diff.attack.textconv", "core.pager", "credential.helper"} {
		t.Run(key, func(t *testing.T) {
			e := newWS(t, "main", 1<<30)
			e.sync(t)
			marker := filepath.Join(t.TempDir(), "executed")
			program := filepath.Join(t.TempDir(), "program")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf executed > '"+marker+"'\ncat\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, e.clone, ".gitattributes", "*.txt filter=attack diff=attack\n")
			writeFile(t, e.clone, "safe.txt", "business data\n")
			sh(t, e.clone, "config", "--local", key, program)
			before := len(e.rec.all())
			_, err := run(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["safe.txt"],"message":"safe"}`)
			wantCode(t, err, "unsafe_repo_config")
			var te *tools.ToolError
			if !errors.As(err, &te) || !te.Denied {
				t.Error("unsafe config must be a policy denial")
			}
			for _, c := range e.rec.all()[before:] {
				if containsAny(c.Args, "add", "commit") {
					t.Error("mutation reached runner")
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("repository-selected program executed")
			}
		})
	}
}

// TC-36 (REQ-F2): no network command may reach a config-selected target.
func TestApproved_ConfigCannotRedirectRemote(t *testing.T) {
	for _, key := range []string{"remote.origin.pushurl", "url.https://attacker.invalid/.insteadOf", "http.proxy", "remote.origin.url", "http.followRedirects"} {
		t.Run(key, func(t *testing.T) {
			e := newWS(t, "main", 1<<30)
			e.sync(t)
			sh(t, e.clone, "config", "--local", key, "https://attacker.invalid/other.git")
			before := len(e.rec.all())
			for _, name := range []string{"git_sync", "git_pull", "git_push"} {
				_, err := run(t, e.tool(t, name), wsGrant(), `{"repo":"acme/app","branch":"nexus/x"}`)
				// git_sync/pull have closed input schemas.
				if name != "git_push" {
					_, err = run(t, e.tool(t, name), wsGrant(), `{"repo":"acme/app"}`)
				}
				wantCode(t, err, "unsafe_repo_config")
			}
			for _, c := range e.rec.all()[before:] {
				if c.Network {
					t.Fatal("network attempted after hostile config")
				}
			}
		})
	}
}

// TC-37 (REQ-F3), TC-38 (REQ-F3): root symlink allowed; descendants forbidden.
func TestApproved_ManagedPathSymlinks(t *testing.T) {
	t.Run("root_allowed", func(t *testing.T) {
		e := newWS(t, "main", 1<<30)
		link := filepath.Join(t.TempDir(), "repos")
		if err := os.Symlink(e.reposDir, link); err != nil {
			t.Fatal(err)
		}
		e.ts = newWorkspaceTools(e.rec, e.tokens, link, 1<<30, testSan{}, "file://"+e.remoteDir+"/")
		e.sync(t)
		writeFile(t, e.clone, "ok.txt", "ordinary content")
		mustOK(t, e.tool(t, "git_commit"), wsGrant(), `{"repo":"acme/app","paths":["ok.txt"],"message":"root link works"}`)
		if got := sh(t, e.clone, "show", "HEAD:ok.txt"); got != "ordinary content" {
			t.Fatal("allowed root did not commit")
		}
	})
	for _, part := range []string{"owner", "clone", ".git"} {
		t.Run(part, func(t *testing.T) {
			e := newWS(t, "main", 1<<30)
			e.sync(t)
			path := e.clone
			if part == "owner" {
				path = filepath.Dir(e.clone)
			}
			if part == ".git" {
				path = filepath.Join(e.clone, ".git")
			}
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			head := sh(t, e.clone, "rev-parse", "HEAD")
			writeFile(t, outside, "sentinel", "must remain")
			before := len(e.rec.all())
			_, err := run(t, e.tool(t, "git_clean"), wsGrant(), `{"repo":"acme/app"}`)
			wantCode(t, err, "repo_unavailable")
			if len(e.rec.all()) != before {
				t.Error("symlink reached runner")
			}
			if sh(t, e.clone, "rev-parse", "HEAD") != head {
				t.Error("outside HEAD changed")
			}
			b, err := os.ReadFile(filepath.Join(outside, "sentinel"))
			if err != nil || string(b) != "must remain" {
				t.Fatal("outside data changed")
			}
		})
	}
}

// TC-40 (REQ-F4): pathspec tricks and existing staged secrets cannot enter HEAD.
func TestApproved_CommitCannotSmuggleSecretPaths(t *testing.T) {
	for _, mode := range []string{"directory", "wildcard", "magic", "prestaged", "deletion"} {
		t.Run(mode, func(t *testing.T) {
			e := newWS(t, "main", 1<<30)
			if mode == "deletion" {
				writeFile(t, e.src, "secrets/old.txt", "private old business password")
				sh(t, e.src, "add", "secrets/old.txt")
				sh(t, e.src, "commit", "-qm", "fixture secret")
				sh(t, e.src, "push", "-q", "origin", "main")
			}
			e.sync(t)
			head := sh(t, e.clone, "rev-parse", "HEAD")
			writeFile(t, e.clone, "docs/ok.txt", "business data")
			writeFile(t, e.clone, "docs/.env", "PASSWORD=ordinary-private-password")
			path := "docs/ok.txt"
			switch mode {
			case "directory":
				path = "docs"
			case "wildcard":
				path = "docs/*"
			case "magic":
				path = ":(glob)**"
			case "prestaged":
				sh(t, e.clone, "add", "docs/.env")
			case "deletion":
				sh(t, e.clone, "rm", "secrets/old.txt")
			}
			in, _ := json.Marshal(map[string]any{"repo": "acme/app", "paths": []string{path}, "message": "must not leak"})
			_, err := run(t, e.tool(t, "git_commit"), wsGrant(), string(in))
			if err == nil {
				t.Error("secret-bearing commit accepted")
			}
			if got := sh(t, e.clone, "rev-parse", "HEAD"); got != head {
				t.Error("HEAD advanced with a forbidden index/pathspec")
			}
			if b, err := os.ReadFile(filepath.Join(e.clone, "docs/.env")); err != nil || string(b) != "PASSWORD=ordinary-private-password" {
				t.Error("worktree secret was changed")
			}
			if got := sh(t, e.clone, "ls-tree", "-r", "--name-only", "HEAD", "docs/.env"); got != "" {
				t.Error("hidden secret blob committed")
			}
		})
	}
}

// TC-53 (REQ-F14), TC-54 (REQ-F14): default information fails closed.
func TestApproved_DefaultBranchUnknownIsDangerous(t *testing.T) {
	for _, mode := range []string{"known", "packed", "missing", "stale"} {
		t.Run(mode, func(t *testing.T) {
			e := newWS(t, "trunk", 1<<30)
			e.sync(t)
			if mode == "packed" {
				sh(t, e.clone, "pack-refs", "--all")
			}
			if mode == "missing" {
				if err := os.Remove(filepath.Join(e.clone, ".git/refs/remotes/origin/HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "stale" {
				sh(t, e.clone, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/gone")
			}
			esc := e.tool(t, "git_push").(tools.TierEscalator)
			for _, b := range []string{"trunk", "nexus/feature"} {
				want := tenant.TierDangerous
				if b == "nexus/feature" && (mode == "known" || mode == "packed") {
					want = tenant.TierWrite
				}
				if got := esc.EffectiveTier(json.RawMessage(`{"repo":"acme/app","branch":"`+b+`"}`), wsGrant()); got != want {
					t.Errorf("%s: got %s want %s", b, got, want)
				}
				if want == tenant.TierDangerous {
					g := *wsGrant()
					g.Allow = []string{"git_push"}
					g.PushBranches = []string{"trunk", "nexus/*"}
					reg, err := tools.NewRegistry(e.tool(t, "git_push"))
					if err != nil {
						t.Fatal(err)
					}
					audit := &approvedAuditStore{}
					policy := tenant.ToolPolicyConfig{MaxWritesPerJob: 2, Accounts: map[string]tenant.GitHubAccount{"acc": {Name: "acc"}}, Repos: map[string]tenant.RepoGrant{"acme/app": g}}
					exec := tools.NewExecutor(reg, policy, tools.DefaultLimits(), false, audit, testSan{}, 42, "ws")
					before := len(e.rec.all())
					r := exec.Call(context.Background(), 1, "git_push", json.RawMessage(`{"repo":"acme/app","branch":"`+b+`"}`))
					if !r.IsError || r.Status != tools.StatusDenied || audit.code != "dangerous_disabled" {
						t.Error("dangerous push bypassed disabled DB switch")
					}
					for _, cmd := range e.rec.all()[before:] {
						if cmd.Network {
							t.Error("dangerous push reached network")
						}
					}
				}
			}
		})
	}
}

type approvedAuditStore struct{ code string }

func (*approvedAuditStore) Begin(context.Context, tools.RunStart) (int64, *tools.PriorRun, error) {
	return 1, nil, nil
}
func (*approvedAuditStore) Finish(context.Context, tools.RunFinish) error { return nil }
func (s *approvedAuditStore) Deny(_ context.Context, _ tools.RunStart, code string) error {
	s.code = code
	return nil
}

// TC-35 (REQ-F2): production URL arguments are inspected without dialing
// GitHub; the ordinary local-bare push/fetch effects have separate tests.
func TestApproved_ProductionFetchPushPinSameURLAndBranch(t *testing.T) {
	e := newWS(t, "main", 1<<30)
	e.sync(t)
	sh(t, e.clone, "config", "--local", "remote.origin.url", "https://github.com/acme/app.git")
	networkCalls := 0
	r := approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
		if c.Network {
			networkCalls++
			if !containsAny(c.Args, "https://github.com/acme/app.git") || strings.Contains(strings.Join(c.Args, " "), testToken) {
				t.Error("network URL not pinned or token in argv")
			}
			if containsAny(c.Args, "push") && !containsAny(c.Args, "refs/heads/nexus/x:refs/heads/nexus/x") {
				t.Error("push changed branch identity")
			}
			return execrunner.Result{}, nil
		}
		return e.rec.inner.Run(ctx, c)
	})
	ts := NewWorkspaceTools(r, e.tokens, e.reposDir, 1<<30, testSan{})
	mustOK(t, findTool(t, ts, "git_sync"), wsGrant(), `{"repo":"acme/app"}`)
	mustOK(t, findTool(t, ts, "git_push"), wsGrant(), `{"repo":"acme/app","branch":"nexus/x"}`)
	if networkCalls != 2 {
		t.Error("fixture did not exercise both network commands")
	}
	cfg, err := os.ReadFile(filepath.Join(e.clone, ".git/config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), testToken) || strings.Contains(string(cfg), "extraheader") {
		t.Error("production network commands persisted token")
	}
}

type approvedRunnerFunc func(context.Context, execrunner.Command) (execrunner.Result, error)

func (f approvedRunnerFunc) Run(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
	return f(ctx, c)
}

// TC-36 (REQ-F2): removing http.followRedirects=false makes real git hit
// the second TLS server. Both endpoints are local fakes; no public DNS/network.
func TestApproved_RealGitHTTPSNeverFollowsRedirect(t *testing.T) {
	var sourceHits, targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	defer source.Close()
	inner := newTestRunner(t)
	r := approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
		// Trust only the local fake's self-signed certificate for this probe.
		c.Args = append([]string{"-c", "http.sslVerify=false"}, c.Args...)
		return inner.Run(ctx, c)
	})
	ts := newWorkspaceTools(r, &fakeTokens{}, t.TempDir(), 1<<20, testSan{}, source.URL+"/")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := findTool(t, ts, "git_sync").Run(ctx, tools.Call{Repo: wsGrant(), Account: &tenant.GitHubAccount{Name: "acc"}, Input: json.RawMessage(`{"repo":"acme/app"}`)})
	if err == nil {
		t.Error("redirecting clone succeeded")
	}
	if sourceHits.Load() != 1 || targetHits.Load() != 0 {
		t.Errorf("TLS requests source=%d target=%d", sourceHits.Load(), targetHits.Load())
	}
}

// TC-56 (REQ-F15): a live producer must be cancelled, not merely checked on exit.
func TestApproved_CloneWatchdogAndDiskBoundary(t *testing.T) {
	t.Run("watchdog", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "acme/app")
		cancelled := false
		r := approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
			if !containsAny(c.Args, "clone") {
				t.Fatal("unexpected git command")
			}
			writeFile(t, root, "growing.pack", strings.Repeat("x", 2<<20))
			<-ctx.Done()
			cancelled = true
			return execrunner.Result{}, ctx.Err()
		})
		ts := newWorkspaceTools(r, &fakeTokens{}, base, 1<<20, testSan{}, "file://"+t.TempDir()+"/")
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_, err := findTool(t, ts, "git_sync").Run(ctx, tools.Call{Repo: wsGrant(), Account: &tenant.GitHubAccount{Name: "acc"}, Input: json.RawMessage(`{"repo":"acme/app"}`)})
		wantCode(t, err, "clone_too_large")
		if !cancelled || ctx.Err() != nil {
			t.Error("watchdog failed to cancel before independent deadline")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Error("oversized clone retained")
		}
	})
	t.Run("disk_low_before_runner", func(t *testing.T) {
		base := t.TempDir()
		var st syscall.Statfs_t
		if err := syscall.Statfs(base, &st); err != nil {
			t.Fatal(err)
		}
		cap := int64(st.Bavail) * int64(st.Bsize)
		calls := 0
		r := approvedRunnerFunc(func(context.Context, execrunner.Command) (execrunner.Result, error) {
			calls++
			return execrunner.Result{}, nil
		})
		ts := newWorkspaceTools(r, &fakeTokens{}, base, cap, testSan{}, "file://"+t.TempDir()+"/")
		_, err := run(t, findTool(t, ts, "git_sync"), wsGrant(), `{"repo":"acme/app"}`)
		wantCode(t, err, "disk_low")
		if calls != 0 {
			t.Fatal("git started without disk reserve")
		}
	})
}
