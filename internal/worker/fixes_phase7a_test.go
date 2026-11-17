package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Regression tests for Fix Brief B1 (R-B-1 .. R-B-8, D-B-1 .. D-B-3, B-CI-1).

func envLast(env []string, key string) (string, bool) {
	v, found := "", false
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v, found = val, true
		}
	}
	return v, found
}

// T-B-70 (REQ-5, R-B-1): the claude argv allows file tools only, always denies Bash, web and six credential/system read paths, and confines reads to the worktree.
func TestClaudeExecutor_ReadConfinementAndNoShell(t *testing.T) {
	run := claudeRunner(`{}`, 0)
	e := &ClaudeExecutor{Bin: "claude", AllowedTools: DefaultClaudeAllowedTools, Runner: run}
	if _, err := e.Run(context.Background(), ExecRequest{Dir: "/wt", MaxTurns: 5, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	args := run.calls()[0].Args
	for _, a := range strings.Split(argAfter(args, "--allowedTools"), ",") {
		l := strings.ToLower(a)
		if strings.HasPrefix(l, "bash") || strings.HasPrefix(l, "web") || strings.HasPrefix(l, "mcp__") {
			t.Errorf("allow-list holds %q", a)
		}
	}
	denied := strings.Split(argAfter(args, "--disallowedTools"), ",")
	has := func(want string) bool {
		for _, d := range denied {
			if d == want {
				return true
			}
		}
		return false
	}
	for _, must := range []string{"Bash", "WebFetch", "WebSearch", "Read(~/.ssh/**)", "Read(~/.config/**)", "Read(~/.local/state/**)", "Read(~/.gnupg/**)", "Read(~/.aws/**)", "Read(//etc/**)"} {
		if !has(must) {
			t.Errorf("deny-list lacks %q: %v", must, denied)
		}
	}
	var settings struct {
		Permissions struct {
			Block bool `json:"blockReadsOutsideWorkingDirectories"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(argAfter(args, "--settings")), &settings); err != nil || !settings.Permissions.Block {
		t.Fatalf("--settings %q must set blockReadsOutsideWorkingDirectories true (%v)", argAfter(args, "--settings"), err)
	}
	if hasArg(args, "--dangerously-skip-permissions") {
		t.Fatal("argv skips permissions")
	}
}

// T-B-71 (REQ-5, R-B-1): the config refuses shell, web and MCP rules and any path-widening rule; the default is file tools only.
func TestConfig_RefusesShellWebMCPAndPathRules(t *testing.T) {
	bad := map[string]string{
		"Bash prefix":      "claude:\n  allowed_tools: ['Bash(ls:*)']\n",
		"lowercase bash":   "claude:\n  allowed_tools: [bash]\n",
		"WebFetch":         "claude:\n  allowed_tools: [WebFetch]\n",
		"WebSearch":        "claude:\n  allowed_tools: [WebSearch]\n",
		"mcp tool":         "claude:\n  allowed_tools: ['mcp__github__create_issue']\n",
		"absolute read":    "claude:\n  allowed_tools: ['Read(//etc/**)']\n",
		"home read":        "claude:\n  allowed_tools: ['Read(~/.ssh/**)']\n",
		"parent traversal": "claude:\n  allowed_tools: ['Read(../x)']\n",
	}
	for name, extra := range bad {
		if _, err := ParseConfig(baseYAML(t, extra), testHome); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := mustConfig(t, "claude:\n  allowed_tools: [Read, Glob]\n")
	if len(c.Claude.AllowedTools) != 2 {
		t.Fatalf("tools %v", c.Claude.AllowedTools)
	}
	d := mustConfig(t, "")
	if strings.Join(d.Claude.AllowedTools, ",") != strings.Join(DefaultClaudeAllowedTools, ",") {
		t.Fatalf("default tools %v", d.Claude.AllowedTools)
	}
	for _, tool := range d.Claude.AllowedTools {
		if strings.HasPrefix(strings.ToLower(tool), "bash") {
			t.Fatalf("default allows %q", tool)
		}
	}
}

// T-B-72 (REQ-5, R-B-2): `executor: opencode` is refused; claude (explicit or default) loads.
func TestConfig_OpencodeExecutorRefused(t *testing.T) {
	if _, err := ParseConfig(baseYAML(t, "executor: opencode\n"), testHome); err == nil {
		t.Fatal("opencode accepted")
	}
	if c := mustConfig(t, "executor: claude\n"); c.Executor != ExecutorClaude {
		t.Fatalf("executor %q", c.Executor)
	}
	if c := mustConfig(t, ""); c.Executor != ExecutorClaude {
		t.Fatalf("default executor %q", c.Executor)
	}
}

// T-B-73 (REQ-5, R-B-1): every git/gh call overrides core.fsmonitor and core.hooksPath and drops inherited git config injection.
func TestRepos_RunAppliesGitSafetyEnv(t *testing.T) {
	run := &fakeRunner{}
	r := &Repos{Git: "git", GH: "gh", ReposDir: t.TempDir(), Runner: run, Env: []string{
		"PATH=/usr/bin", "GIT_CONFIG_COUNT=9", "GIT_CONFIG_KEY_0=core.sshCommand", "GIT_CONFIG_VALUE_0=evil",
		"GIT_CONFIG_KEY_5=core.pager", "GIT_CONFIG_VALUE_5=evil", "GIT_CONFIG_PARAMETERS='core.editor=evil'",
		"GIT_DIR=/evil", "GIT_WORK_TREE=/evil",
	}}
	if _, err := r.git(context.Background(), "/wt", "status"); err != nil {
		t.Fatal(err)
	}
	env := run.calls()[0].Env
	if v, _ := envLast(env, "GIT_CONFIG_COUNT"); v != "2" {
		t.Fatalf("GIT_CONFIG_COUNT %q", v)
	}
	if k, _ := envLast(env, "GIT_CONFIG_KEY_0"); k != "core.fsmonitor" {
		t.Fatalf("key 0 %q", k)
	}
	if v, _ := envLast(env, "GIT_CONFIG_VALUE_0"); v != "false" {
		t.Fatalf("fsmonitor %q", v)
	}
	if k, _ := envLast(env, "GIT_CONFIG_KEY_1"); k != "core.hooksPath" {
		t.Fatalf("key 1 %q", k)
	}
	if v, _ := envLast(env, "GIT_CONFIG_VALUE_1"); v != os.DevNull {
		t.Fatalf("hooksPath %q", v)
	}
	keys := 0
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
			keys++
		}
		for _, gone := range []string{"GIT_CONFIG_PARAMETERS", "GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_KEY_5", "GIT_CONFIG_VALUE_5"} {
			if k == gone {
				t.Errorf("inherited %s survived", gone)
			}
		}
	}
	if keys != 2 {
		t.Fatalf("%d config key entries, want exactly 2", keys)
	}
}

// T-B-74 (REQ-5, R-B-1): a worktree whose .git link is not a regular file pointing under <ReposDir>/…/.git/worktrees/ is refused before any command runs.
func TestRepos_RefusesTamperedGitLink(t *testing.T) {
	run := &fakeRunner{}
	reposDir := t.TempDir()
	r := &Repos{Git: "git", GH: "gh", ReposDir: reposDir, Runner: run, Env: []string{}}
	wt := filepath.Join(reposDir, worktreesSubdir, "job1")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(reposDir, "owner", "name", ".git", "worktrees", "job1")
	if err := os.MkdirAll(gitdir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wt, ".git")
	write := func(content string) {
		t.Helper()
		_ = os.RemoveAll(link)
		if err := os.WriteFile(link, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("gitdir: " + gitdir + "\n")
	if _, err := r.git(context.Background(), wt, "status"); err != nil {
		t.Fatalf("honest link refused: %v", err)
	}
	if _, err := r.git(context.Background(), filepath.Join(wt, "sub"), "status"); err != nil {
		t.Fatalf("honest link refused from a subdirectory: %v", err)
	}
	ok := len(run.calls())
	outside := t.TempDir()
	other := filepath.Join(reposDir, worktreesSubdir, "job2", ".git", "worktrees", "x")
	for name, setup := range map[string]func(){
		"outside repos dir":        func() { write("gitdir: " + outside + "\n") },
		"inside worktrees dir":     func() { write("gitdir: " + other + "\n") },
		"not under .git/worktrees": func() { write("gitdir: " + filepath.Join(reposDir, "owner", "name") + "\n") },
		"empty gitdir":             func() { write("gitdir:\n") },
		"no gitdir prefix":         func() { write(gitdir + "\n") },
		"directory":                func() { _ = os.RemoveAll(link); _ = os.MkdirAll(link, 0o700) },
		"symlink": func() {
			write("gitdir: " + gitdir + "\n")
			real := link + ".real"
			_ = os.Rename(link, real)
			_ = os.Symlink(real, link)
		},
		"missing": func() { _ = os.RemoveAll(link) },
	} {
		setup()
		if _, err := r.git(context.Background(), wt, "status"); !errors.Is(err, errGitLink) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(run.calls()) != ok {
		t.Fatalf("%d commands ran against a tampered worktree", len(run.calls())-ok)
	}
}

// T-B-75 (REQ-5, R-B-3): the test command runs with an empty HOME and no git, GitHub or SSH credentials; real Go caches are kept; the executor keeps HOME.
func TestTestEnvAndExecutorEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/home/real", "XDG_CONFIG_HOME=/home/real/.config", "XDG_DATA_HOME=/home/real/.local/share", "XDG_STATE_HOME=/home/real/.local/state",
		"SSH_AUTH_SOCK=/run/agent", "SSH_AGENT_PID=1", "GH_TOKEN=ghp_x", "GITHUB_TOKEN=ghp_y", "GH_HOST=h", "GIT_ASKPASS=/bin/ask",
		"GIT_SSH_COMMAND=ssh -i k", "GIT_CONFIG_GLOBAL=/home/real/.gitconfig", "GCM_CREDENTIAL_STORE=x",
		"GOPATH=/home/real/gopath", "GOMODCACHE=/home/real/gopath/pkg/mod", "GOCACHE=/home/real/.cache/go-build",
		"ANTHROPIC_API_KEY=sk-ant", "NEXUS_DATABASE_URL=postgres://x", "LANG=C",
	}
	te := TestEnv(in, "/sandbox/home", "/sandbox/gh")
	for k, want := range map[string]string{
		"HOME": "/sandbox/home", "GH_CONFIG_DIR": "/sandbox/gh", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0", "GOPATH": "/home/real/gopath", "GOMODCACHE": "/home/real/gopath/pkg/mod",
		"GOCACHE": "/home/real/.cache/go-build", "PATH": "/usr/bin", "LANG": "C",
	} {
		if got, ok := envLast(te, k); !ok || got != want {
			t.Errorf("test env %s = %q (set %v), want %q", k, got, ok, want)
		}
	}
	for _, k := range []string{"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GIT_SSH_COMMAND", "GCM_CREDENTIAL_STORE",
		"ANTHROPIC_API_KEY", "NEXUS_DATABASE_URL", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if _, ok := envLast(te, k); ok {
			t.Errorf("test env keeps %s", k)
		}
	}
	if v, _ := envLast(te, "GIT_ASKPASS"); v != "" {
		t.Errorf("GIT_ASKPASS %q", v)
	}
	homes := 0
	for _, kv := range te {
		if strings.HasPrefix(kv, "HOME=") {
			homes++
		}
	}
	if homes != 1 {
		t.Errorf("%d HOME entries", homes)
	}
	ee := ExecutorEnv(in, "/sandbox/gh")
	if v, _ := envLast(ee, "HOME"); v != "/home/real" {
		t.Errorf("executor HOME %q (claude needs its login)", v)
	}
	if v, _ := envLast(ee, "GH_CONFIG_DIR"); v != "/sandbox/gh" {
		t.Errorf("executor GH_CONFIG_DIR %q", v)
	}
	for _, k := range []string{"SSH_AUTH_SOCK", "GH_TOKEN", "GITHUB_TOKEN", "GIT_SSH_COMMAND", "ANTHROPIC_API_KEY", "NEXUS_DATABASE_URL"} {
		if _, ok := envLast(ee, k); ok {
			t.Errorf("executor env keeps %s", k)
		}
	}
	if v, _ := envLast(ee, "GIT_CONFIG_GLOBAL"); v != os.DevNull {
		t.Errorf("executor GIT_CONFIG_GLOBAL %q", v)
	}
}

// T-B-76 (REQ-5, D-B-2): Push refuses ref names git would reject and names outside nexus/, without running any command.
func TestRepos_PushRefusesBadRefNames(t *testing.T) {
	r, run := fakeRepos(t, nil)
	for _, branch := range []string{"nexus/.hidden", "nexus/a.lock", "nexus/a/b.lock", "nexus/a//b", "nexus/a/", "nexus/a.", "nexus/a@{b",
		"nexus/a..b", "nexus/../main", "nexus/a/.b", "nexus/ a", "nexus", "nexus/", "Nexus/a", "nexus/a:b", "nexus/a~1", "nexus/a^"} {
		if err := r.Push(context.Background(), "/wt", branch, "main"); err == nil {
			t.Errorf("push of %q allowed", branch)
		}
	}
	if n := len(run.calls()); n != 0 {
		t.Fatalf("%d commands ran for refused branches", n)
	}
	for _, branch := range []string{"nexus/fc-1-abcd1234", "nexus/a/b", "nexus/a.b-c_d"} {
		if err := r.Push(context.Background(), "/wt", branch, "main"); err != nil {
			t.Errorf("push of valid %q refused: %v", branch, err)
		}
	}
}

// T-B-77 (REQ-5, D-B-3): a hub-chosen job id that fails the id rule maps to a short x-<sha8> name in the worktree path and the branch, never a path.
func TestSafeJobNameInPathsAndBranch(t *testing.T) {
	r, _ := fakeRepos(t, nil)
	if got := safeJobName("abc-1"); got != "abc-1" {
		t.Fatalf("valid id changed: %q", got)
	}
	nameRE := regexp.MustCompile(`^x-[0-9a-f]{16}$`)
	root := filepath.Join(r.ReposDir, worktreesSubdir) + string(filepath.Separator)
	for _, id := range []string{"../../etc", "a/b", "", "-x", "a b", strings.Repeat("a", 65), "x\n"} {
		if got := safeJobName(id); !nameRE.MatchString(got) {
			t.Errorf("safeJobName(%q) = %q", id, got)
		}
		wt := r.WorktreePath(id)
		if !strings.HasPrefix(wt, root) || strings.Contains(strings.TrimPrefix(wt, root), string(filepath.Separator)) {
			t.Errorf("worktree path %q escapes %q", wt, root)
		}
		if b := BranchName("FC-1", id); !regexp.MustCompile(`^nexus/fc-1-x[0-9a-f]{0,7}$`).MatchString(b) {
			t.Errorf("branch %q", b)
		}
	}
	if safeJobName("../a") == safeJobName("../b") {
		t.Fatal("different hostile ids share a name")
	}
}

// T-B-78 (REQ-5, REQ-10, D-B-1): a changed binary file counts BinaryFileWeight lines toward the diff limit.
func TestDiffStat_BinaryFileWeight(t *testing.T) {
	d := DiffStat{Files: 3, Added: 10, Removed: 2, Binary: 2}
	if d.Changed() != 12 || d.Weighted() != 12+2*BinaryFileWeight || BinaryFileWeight != 100 {
		t.Fatalf("changed %d weighted %d", d.Changed(), d.Weighted())
	}
	r, _ := fakeRepos(t, func(c Cmd) (int, error) {
		if c.Args[0] == "diff" {
			_, _ = c.Stdout.Write([]byte("-\t-\timg.png\n"))
		}
		return 0, nil
	})
	got, err := r.StageAndDiff(context.Background(), "/wt", strings.Repeat("a", 40))
	if err != nil || got.Files != 1 || got.Binary != 1 || got.Changed() != 0 || got.Weighted() != 100 {
		t.Fatalf("%+v %v", got, err)
	}
}

// T-B-79 (REQ-15, R-B-4): the heartbeat body carries ack_instruction_ids only when there are some, at most 64 (the newest).
func TestHTTPHub_HeartbeatAckBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"running"}`))
	}))
	defer srv.Close()
	h := NewHTTPHub(srv.URL, secretToken, nil)
	var _ InstructionAcker = h
	ctx := context.Background()
	if _, err := h.HeartbeatAck(ctx, "job-1", "lease", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.HeartbeatAck(ctx, "job-1", "lease", nil, []string{"v1", "v2"}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 70)
	for i := range many {
		many[i] = "v" + strconv.Itoa(i+1)
	}
	if _, err := h.HeartbeatAck(ctx, "job-1", "lease", nil, many); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies[0]["ack_instruction_ids"]; ok {
		t.Fatal("empty ack list was sent")
	}
	if ids, _ := bodies[1]["ack_instruction_ids"].([]any); len(ids) != 2 || ids[0] != "v1" || ids[1] != "v2" {
		t.Fatalf("ids %v", bodies[1]["ack_instruction_ids"])
	}
	ids, _ := bodies[2]["ack_instruction_ids"].([]any)
	if len(ids) != MaxAckInstructionIDs || MaxAckInstructionIDs != 64 || ids[0] != "v7" || ids[63] != "v70" {
		t.Fatalf("%d ids, first %v last %v", len(ids), ids[0], ids[len(ids)-1])
	}
	for _, b := range bodies {
		if b["lease_token"] != "lease" {
			t.Fatalf("body %v", b)
		}
	}
}

var _ = behavior.WorkerStatusFailed
