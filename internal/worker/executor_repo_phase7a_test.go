package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- payload and branch names (T-B-10 .. T-B-12) ----

// T-B-10 (REQ-5): ParsePayload accepts hub payloads and rejects hostile ones.
func TestParsePayload(t *testing.T) {
	ok := []string{
		`{"repo":"o/r","task_id":"FC-6583","clickup_task_id":"86abc","title":"t","instruction":"do it"}`,
		`{"repo":"o/r","instruction":"x","instruction_version":3,"future_key":{"a":1}}`,
		`{"repo":"o/r","clickup_task_id":"86abc","instruction":""}`,
		`{"repo":"o/r","instruction":"` + strings.Repeat("a", maxInstructionBytes) + `"}`,
		`{"repo":"o/r","instruction":"tiếng Việt 😀"}`,
	}
	for _, raw := range ok {
		if _, err := ParsePayload([]byte(raw)); err != nil {
			t.Errorf("rejected %.60s: %v", raw, err)
		}
	}
	bad := map[string]string{
		"empty":                  ``,
		"not json":               `nope`,
		"array":                  `[]`,
		"no repo":                `{"instruction":"x"}`,
		"repo traversal":         `{"repo":"../../etc/passwd","instruction":"x"}`,
		"repo with dotdot":       `{"repo":"o/r..x/../y","instruction":"x"}`,
		"repo .git":              `{"repo":"o/r.git","instruction":"x"}`,
		"repo url":               `{"repo":"https://evil.example/o/r","instruction":"x"}`,
		"task id traversal":      `{"repo":"o/r","task_id":"../x","instruction":"x"}`,
		"task id slash":          `{"repo":"o/r","task_id":"FC-1/../../x","instruction":"x"}`,
		"task id newline":        `{"repo":"o/r","task_id":"FC-1\nx","instruction":"x"}`,
		"clickup id traversal":   `{"repo":"o/r","clickup_task_id":"../../x","instruction":"x"}`,
		"instruction too big":    `{"repo":"o/r","instruction":"` + strings.Repeat("a", maxInstructionBytes+1) + `"}`,
		"blank, no clickup task": `{"repo":"o/r","instruction":"   \n "}`,
	}
	for name, raw := range bad {
		if _, err := ParsePayload([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// invalid UTF-8 inside a JSON string is replaced by the decoder; a raw lone
	// surrogate escape must not crash either.
	if _, err := ParsePayload([]byte(`{"repo":"o/r","instruction":"\ud800 x"}`)); err != nil {
		t.Logf("lone surrogate: %v", err)
	}
}

// T-B-11 (REQ-5): BranchName is nexus/<task>-<short> and never escapes the namespace.
func TestBranchName(t *testing.T) {
	const job = "0123abcd-4567-89ef-0123-456789abcdef"
	if got := BranchName("FC-6583", job); got != "nexus/fc-6583-0123abcd" {
		t.Fatalf("got %q", got)
	}
	safe := regexp.MustCompile(`^nexus/[a-z0-9-]+$`)
	hostile := []string{"", "../../etc/passwd", "FC-1/../../x", "a b c", "TASK-😀", "x\nrefs/heads/main", "--force", "main", "-", "___", strings.Repeat("A", 500), "FC_1.2", "~^:?*[\\"}
	for _, id := range hostile {
		got := BranchName(id, job)
		if !safe.MatchString(got) || strings.Contains(got, "..") || len(got) > len("nexus/")+40+1+8 {
			t.Errorf("BranchName(%q) = %q", id, got)
		}
		if got == "main" || got == "master" || !strings.HasPrefix(got, "nexus/") {
			t.Errorf("BranchName(%q) can hit the default branch: %q", id, got)
		}
	}
	if a, b := BranchName("FC-1", "aaaaaaaa-1"), BranchName("FC-1", "bbbbbbbb-1"); a == b {
		t.Fatalf("two jobs of one task share a branch: %q", a)
	}
}

// ---- executors (T-B-13 .. T-B-18) ----

func claudeRunner(out string, exit int) *fakeRunner {
	return &fakeRunner{script: func(c Cmd) (int, error) {
		if c.Stdout != nil {
			_, _ = c.Stdout.Write([]byte(out))
		}
		return exit, nil
	}}
}

// T-B-13 (REQ-5): the claude argv has an allow-list, a deny-list, a fixed prompt and never skips permissions.
func TestClaudeExecutor_ArgvIsLockedDown(t *testing.T) {
	run := claudeRunner(`{"subtype":"success","num_turns":4,"result":"done","session_id":"sess_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}}`, 0)
	e := &ClaudeExecutor{Bin: "claude", Model: "sonnet", AllowedTools: DefaultClaudeAllowedTools, Runner: run}
	res, err := e.Run(context.Background(), ExecRequest{Dir: "/wt", MaxTurns: 12, Timeout: time.Minute})
	if err != nil || res.ExitCode != 0 || res.FinalMessage != "done" || res.SessionID != "sess_1" || res.Turns != 4 || res.InputTokens != 15 || res.OutputTokens != 7 {
		t.Fatalf("res %+v err %v", res, err)
	}
	c := run.calls()[0]
	if c.Dir != "/wt" || c.Name != "claude" || c.Env != nil {
		t.Fatalf("cmd %+v (Env must be nil so the scrubbed env applies)", c)
	}
	args := strings.Join(c.Args, "\x00")
	for _, forbidden := range []string{"--dangerously-skip-permissions", "bypassPermissions", "--permission-mode\x00bypassPermissions", "--mcp-config"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("argv contains %q", forbidden)
		}
	}
	if argAfter(c.Args, "--permission-mode") != "acceptEdits" || argAfter(c.Args, "--max-turns") != "12" || argAfter(c.Args, "--model") != "sonnet" {
		t.Fatalf("argv %v", c.Args)
	}
	allowed := strings.Split(argAfter(c.Args, "--allowedTools"), ",")
	for _, a := range allowed {
		if a == "Bash" || strings.HasPrefix(a, "Bash(gh") || strings.Contains(a, "push") {
			t.Fatalf("allow-list contains %q", a)
		}
	}
	denied := argAfter(c.Args, "--disallowedTools")
	for _, must := range []string{"Bash(git push:*)", "Bash(gh:*)", "Bash(git commit:*)", "WebFetch"} {
		if !strings.Contains(denied, must) {
			t.Fatalf("deny-list lacks %q: %s", must, denied)
		}
	}
	prompt := argAfter(c.Args, "-p")
	if prompt != execPrompt || !strings.Contains(prompt, ".nexus/instructions.md") {
		t.Fatalf("prompt %q", prompt)
	}
}

// T-B-14 (REQ-5): resume only takes a clean session id; a hostile one is dropped; max-turns is at least 1.
func TestClaudeExecutor_ResumeAndTurnFloor(t *testing.T) {
	run := claudeRunner(`{}`, 0)
	e := &ClaudeExecutor{Bin: "claude", AllowedTools: DefaultClaudeAllowedTools, Runner: run}
	_, _ = e.Run(context.Background(), ExecRequest{Dir: "/wt", Resume: "abc-DEF_1", MaxTurns: 0, Timeout: time.Minute, FollowUp: true})
	_, _ = e.Run(context.Background(), ExecRequest{Dir: "/wt", Resume: "x; rm -rf ~", MaxTurns: -5, Timeout: time.Minute})
	calls := run.calls()
	if argAfter(calls[0].Args, "--resume") != "abc-DEF_1" || argAfter(calls[0].Args, "--max-turns") != "1" || argAfter(calls[0].Args, "-p") != followUpPrompt {
		t.Fatalf("first %v", calls[0].Args)
	}
	if hasArg(calls[1].Args, "--resume") || argAfter(calls[1].Args, "--max-turns") != "1" {
		t.Fatalf("second %v", calls[1].Args)
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c.Args, " "), "rm -rf") {
			t.Fatal("hostile session id reached argv")
		}
	}
}

// T-B-15 (REQ-8): the claude JSON result maps to turn limit, bounded summary and clean session id.
func TestParseClaudeJSON(t *testing.T) {
	var r ExecResult
	parseClaudeJSON("noise line\n"+`{"subtype":"error_max_turns","num_turns":50,"result":"partial","session_id":"s1"}`, &r)
	if !r.TurnLimit || r.Turns != 50 || r.FinalMessage != "partial" || r.SessionID != "s1" {
		t.Fatalf("%+v", r)
	}
	r = ExecResult{}
	parseClaudeJSON(`{"subtype":"success","result":"`+strings.Repeat("é", 9000)+`","session_id":"bad id!","num_turns":-3,"usage":{"input_tokens":-9}}`, &r)
	if utf8.RuneCountInString(r.FinalMessage) != maxSummaryRunes || r.SessionID != "" || r.Turns != 0 || r.InputTokens != 0 || r.TurnLimit {
		t.Fatalf("bounds: runes=%d %+v", utf8.RuneCountInString(r.FinalMessage), r)
	}
	for _, junk := range []string{"", "not json", "{", `["a"]`, "\x00\x01"} {
		r = ExecResult{}
		parseClaudeJSON(junk, &r) // must not panic
		if r.FinalMessage != "" {
			t.Fatalf("junk %q produced %q", junk, r.FinalMessage)
		}
	}
}

// T-B-16 (REQ-8): a timeout, a cancel and a missing binary are told apart.
func TestRunExec_TimeoutCancelNotStarted(t *testing.T) {
	block := &fakeRunner{script: func(Cmd) (int, error) { return 0, nil }}
	res, err := runExec(context.Background(), block, Cmd{Name: "x"}, 0)
	if err != nil || !res.TimedOut || len(block.calls()) != 0 {
		t.Fatalf("zero budget: %+v %v calls=%d (must not start the executor)", res, err, len(block.calls()))
	}
	hang := runnerFunc(func(ctx context.Context, c Cmd) (int, error) { <-ctx.Done(); return -1, ctx.Err() })
	start := time.Now()
	res, err = runExec(context.Background(), hang, Cmd{Name: "x"}, 30*time.Millisecond)
	if err != nil || !res.TimedOut || res.Cancelled || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	res, err = runExec(ctx, hang, Cmd{Name: "x"}, time.Minute)
	if err != nil || !res.Cancelled || res.TimedOut {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	ns := runnerFunc(func(context.Context, Cmd) (int, error) { return -1, ErrNotStarted })
	if _, err = runExec(context.Background(), ns, Cmd{Name: "x"}, time.Minute); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("not started: %v", err)
	}
}

type runnerFunc func(ctx context.Context, c Cmd) (int, error)

func (f runnerFunc) Run(ctx context.Context, c Cmd) (int, error) { return f(ctx, c) }

// T-B-17 (REQ-5): opencode gets the fixed prompt, --continue only for follow-ups, and ANSI-free tail output.
func TestOpencodeExecutor(t *testing.T) {
	run := claudeRunner("\x1b[32mgreen\x1b[0m line one\n"+strings.Repeat("x", 5000)+"\nfinal answer ✓", 0)
	e := &OpencodeExecutor{Bin: "opencode", Model: "openai/gpt", Agent: "implementer", Runner: run}
	res, err := e.Run(context.Background(), ExecRequest{Dir: "/wt", Timeout: time.Minute})
	if err != nil || !strings.HasSuffix(res.FinalMessage, "final answer ✓") || strings.Contains(res.FinalMessage, "\x1b") || utf8.RuneCountInString(res.FinalMessage) > maxSummaryRunes {
		t.Fatalf("res %q err %v", res.FinalMessage, err)
	}
	if _, err := e.Run(context.Background(), ExecRequest{Dir: "/wt", Timeout: time.Minute, FollowUp: true}); err != nil {
		t.Fatal(err)
	}
	first, second := run.calls()[0].Args, run.calls()[1].Args
	if hasArg(first, "--continue") || !hasArg(second, "--continue") || first[len(first)-1] != execPrompt || second[len(second)-1] != followUpPrompt {
		t.Fatalf("argv %v / %v", first, second)
	}
	if argAfter(first, "--model") != "openai/gpt" || argAfter(first, "--agent") != "implementer" {
		t.Fatalf("argv %v", first)
	}
	if strings.Contains(strings.Join(first, " "), "dangerously") {
		t.Fatal("unsafe flag")
	}
}

// T-B-18 (REQ-18): Version parses `--version` output and survives a broken binary.
func TestExecutorVersion(t *testing.T) {
	good := claudeRunner("claude-code 2.1.45 (Claude Code)\n", 0)
	if v := (&ClaudeExecutor{Bin: "claude", Runner: good}).Version(context.Background()); v != "2.1.45" {
		t.Fatalf("version %q", v)
	}
	if v := (&OpencodeExecutor{Bin: "opencode", Runner: claudeRunner("garbage", 0)}).Version(context.Background()); v != "" {
		t.Fatalf("no version should be empty: %q", v)
	}
	if v := (&ClaudeExecutor{Bin: "claude", Runner: claudeRunner("1.2.3", 1)}).Version(context.Background()); v != "" {
		t.Fatalf("failing binary: %q", v)
	}
	if v := (&ClaudeExecutor{Bin: "claude", Runner: runnerFunc(func(context.Context, Cmd) (int, error) { return -1, ErrNotStarted })}).Version(context.Background()); v != "" {
		t.Fatalf("missing binary: %q", v)
	}
	long := "1.2." + strings.Repeat("9", 200)
	if v := (&ClaudeExecutor{Bin: "claude", Runner: claudeRunner(long, 0)}).Version(context.Background()); len(v) > 64 {
		t.Fatalf("version not bounded: %d", len(v))
	}
}

// ---- git / gh with a fake runner (T-B-20 .. T-B-27) ----

func fakeRepos(t *testing.T, script func(c Cmd) (int, error)) (*Repos, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{script: script}
	return &Repos{Git: "git", GH: "gh", CloneWith: "gh", ReposDir: t.TempDir(), Runner: run, Env: []string{}}, run
}

// T-B-20 (REQ-5): Push never targets the default branch or anything outside nexus/, and never forces.
func TestRepos_PushRefusesAndNeverForces(t *testing.T) {
	r, run := fakeRepos(t, nil)
	for _, branch := range []string{"main", "master", "develop", "feature/x", "nexus", "nexus/", "nexus/../main", "nexus/a b", "nexus/a;b", "", "-f", "nexus/x\nmain", "refs/heads/main"} {
		if err := r.Push(context.Background(), "/wt", branch, "main"); err == nil && !regexp.MustCompile(`^nexus/[A-Za-z0-9._-]+$`).MatchString(branch) {
			t.Errorf("push of %q allowed", branch)
		}
	}
	if err := r.Push(context.Background(), "/wt", "nexus/main", "nexus/main"); err == nil {
		t.Error("push allowed although branch equals the default branch")
	}
	before := len(run.calls())
	if err := r.Push(context.Background(), "/wt", "nexus/fc-1-abcd1234", "main"); err != nil {
		t.Fatal(err)
	}
	c := run.calls()[before]
	line := strings.Join(c.Args, " ")
	if c.Name != "git" || c.Args[0] != "push" || !strings.Contains(line, "refs/heads/nexus/fc-1-abcd1234:refs/heads/nexus/fc-1-abcd1234") {
		t.Fatalf("push argv %q", line)
	}
	for _, bad := range []string{"--force", "-f", "--force-with-lease", "--mirror", "--delete", "+refs"} {
		if strings.Contains(line, bad) {
			t.Fatalf("push argv has %q: %s", bad, line)
		}
	}
	if strings.Contains(line, "main") {
		t.Fatalf("push argv mentions the default branch: %s", line)
	}
	// a failing push is an error, a start failure too
	r2, _ := fakeRepos(t, func(Cmd) (int, error) { return 1, nil })
	if r2.Push(context.Background(), "/wt", "nexus/x-1", "main") == nil {
		t.Fatal("exit 1 push reported success")
	}
}

// T-B-21 (REQ-5): the draft PR is draft, uses argv only, and its URL must be a github.com pull URL.
func TestRepos_CreateDraftPR(t *testing.T) {
	out := "warning: something\nhttps://github.com/owner/repo/pull/42\n"
	r, run := fakeRepos(t, func(c Cmd) (int, error) { _, _ = c.Stdout.Write([]byte(out)); return 0, nil })
	url, err := r.CreateDraftPR(context.Background(), "/wt", "owner/repo", "nexus/x-1", "main", "[FC-1] title; rm -rf /", "/wt/.nexus/pr-body.md")
	if err != nil || url != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("%q %v", url, err)
	}
	c := run.calls()[0]
	if c.Name != "gh" || !hasArg(c.Args, "--draft") || argAfter(c.Args, "--repo") != "owner/repo" || argAfter(c.Args, "--head") != "nexus/x-1" ||
		argAfter(c.Args, "--base") != "main" || argAfter(c.Args, "--body-file") != "/wt/.nexus/pr-body.md" || argAfter(c.Args, "--title") != "[FC-1] title; rm -rf /" {
		t.Fatalf("argv %v", c.Args)
	}
	for _, bad := range []string{"no url here", "https://evil.example/owner/repo/pull/1", "http://github.com/o/r/pull/1", "https://github.com.evil.example/o/r/pull/1", ""} {
		rb, _ := fakeRepos(t, func(c Cmd) (int, error) { _, _ = c.Stdout.Write([]byte(bad)); return 0, nil })
		if u, err := rb.CreateDraftPR(context.Background(), "/wt", "o/r", "nexus/x", "main", "t", "/b"); err == nil && !strings.HasPrefix(u, "https://github.com/") {
			t.Errorf("accepted output %q -> %q", bad, u)
		}
	}
	rf, _ := fakeRepos(t, func(Cmd) (int, error) { return 1, nil })
	if _, err := rf.CreateDraftPR(context.Background(), "/wt", "o/r", "nexus/x", "main", "t", "/b"); err == nil {
		t.Fatal("gh failure reported success")
	}
}

// T-B-22 (REQ-5): the first run clones with gh (or https git, no token in the URL); a later run fetches.
func TestRepos_EnsureClone(t *testing.T) {
	r, run := fakeRepos(t, func(c Cmd) (int, error) {
		if hasArg(c.Args, "clone") { // simulate a successful clone
			dir := c.Args[len(c.Args)-1]
			if c.Args[0] == "repo" {
				dir = c.Args[3]
			}
			_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o700)
		}
		return 0, nil
	})
	dir, err := r.EnsureClone(context.Background(), "owner/name")
	if err != nil || dir != filepath.Join(r.ReposDir, "owner", "name") {
		t.Fatalf("%q %v", dir, err)
	}
	if got := run.line(0); got != "gh repo clone owner/name "+dir+" -- --quiet" {
		t.Fatalf("clone cmd %q", got)
	}
	if _, err := r.EnsureClone(context.Background(), "owner/name"); err != nil {
		t.Fatal(err)
	}
	if got := run.line(1); got != "git fetch --prune --quiet origin" || run.calls()[1].Dir != dir {
		t.Fatalf("fetch cmd %q in %q", got, run.calls()[1].Dir)
	}
	// plain git clone uses a token-free https URL
	rg, rung := fakeRepos(t, nil)
	rg.CloneWith = "git"
	if _, err := rg.EnsureClone(context.Background(), "owner/name"); err != nil {
		t.Fatal(err)
	}
	if got := rung.calls()[0].Args; !hasArg(got, "https://github.com/owner/name.git") {
		t.Fatalf("git clone args %v", got)
	}
}

// T-B-23 (REQ-5): bad repos and foreign directories are never cloned over; a failed clone is cleaned.
func TestRepos_EnsureCloneRefusals(t *testing.T) {
	r, run := fakeRepos(t, nil)
	for _, bad := range []string{"../x/y", "a/b.git", "a/../b", "", "x"} {
		if _, err := r.EnsureClone(context.Background(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(run.calls()) != 0 {
		t.Fatalf("commands ran for invalid repos: %d", len(run.calls()))
	}
	// something unrelated sits where the clone would go: keep it, run nothing
	foreign := filepath.Join(r.ReposDir, "owner", "name")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(foreign, "precious.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureClone(context.Background(), "owner/name"); err == nil {
		t.Fatal("cloned over a foreign directory")
	}
	if _, err := os.Stat(keep); err != nil || len(run.calls()) != 0 {
		t.Fatalf("foreign directory touched (calls=%d): %v", len(run.calls()), err)
	}
	// a failed clone leaves nothing half-written
	rf, _ := fakeRepos(t, func(c Cmd) (int, error) {
		dir := c.Args[3]
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "partial"), []byte("x"), 0o600)
		return 128, nil
	})
	if _, err := rf.EnsureClone(context.Background(), "o/r"); err == nil {
		t.Fatal("failed clone reported success")
	}
	if _, err := os.Stat(rf.ClonePath("o/r")); err == nil {
		t.Fatal("failed clone left a directory")
	}
}

// T-B-24 (REQ-5): the default branch is read from origin/HEAD and validated.
func TestRepos_DefaultBranch(t *testing.T) {
	answer := func(out string, failFirst bool) (*Repos, *fakeRunner) {
		n := 0
		return fakeRepos(t, func(c Cmd) (int, error) {
			if c.Args[0] == "symbolic-ref" {
				n++
				if failFirst && n == 1 {
					return 1, nil
				}
				_, _ = c.Stdout.Write([]byte(out))
			}
			return 0, nil
		})
	}
	r, _ := answer("origin/main\n", false)
	if b, err := r.DefaultBranch(context.Background(), "/c"); err != nil || b != "main" {
		t.Fatalf("%q %v", b, err)
	}
	r, run := answer("origin/release/1.0\n", true)
	if b, err := r.DefaultBranch(context.Background(), "/c"); err != nil || b != "release/1.0" || !strings.Contains(run.line(1), "remote set-head origin --auto") {
		t.Fatalf("fallback: %q %v", b, err)
	}
	for _, out := range []string{"main\n", "origin/../etc\n", "origin/-x\n", "origin/a b\n", "origin/\n", ""} {
		r, _ := answer(out, false)
		if b, err := r.DefaultBranch(context.Background(), "/c"); err == nil {
			t.Errorf("%q accepted as %q", out, b)
		}
	}
}

// T-B-25 (REQ-5, REQ-8): the diff stat counts lines and files, binary files count as files, and .nexus is excluded.
func TestRepos_StageAndDiff(t *testing.T) {
	numstat := "10\t2\ta.go\n-\t-\timg.png\n0\t5\tdir/b.go\n\nbroken line\n"
	r, run := fakeRepos(t, func(c Cmd) (int, error) {
		if c.Args[0] == "diff" {
			_, _ = c.Stdout.Write([]byte(numstat))
		}
		return 0, nil
	})
	d, err := r.StageAndDiff(context.Background(), "/wt", strings.Repeat("a", 40))
	if err != nil || d.Files != 3 || d.Added != 10 || d.Removed != 7 || d.Changed() != 17 {
		t.Fatalf("%+v %v", d, err)
	}
	add := strings.Join(run.calls()[0].Args, " ")
	if !strings.HasPrefix(add, "add --all") || !strings.Contains(add, ":(exclude).nexus") {
		t.Fatalf("add argv %q", add)
	}
	if diff := strings.Join(run.calls()[1].Args, " "); !strings.Contains(diff, strings.Repeat("a", 40)) || !strings.Contains(diff, "--cached") {
		t.Fatalf("diff argv %q", diff)
	}
	rf, _ := fakeRepos(t, func(Cmd) (int, error) { return 1, nil })
	if _, err := rf.StageAndDiff(context.Background(), "/wt", "b"); err == nil {
		t.Fatal("git failure reported success")
	}
}

// T-B-26 (REQ-5): Commit does nothing when nothing is staged, and commits with a message file otherwise.
func TestRepos_Commit(t *testing.T) {
	r, run := fakeRepos(t, func(c Cmd) (int, error) { return 0, nil })
	if err := r.Commit(context.Background(), "/wt", "/m"); err != nil || len(run.calls()) != 1 {
		t.Fatalf("clean tree: %v calls=%d", err, len(run.calls()))
	}
	r, run = fakeRepos(t, func(c Cmd) (int, error) {
		if c.Args[0] == "diff" {
			return 1, nil
		}
		return 0, nil
	})
	if err := r.Commit(context.Background(), "/wt", "/wt/.nexus/commit-msg.txt"); err != nil {
		t.Fatal(err)
	}
	if got := run.line(1); got != "git commit --quiet -F /wt/.nexus/commit-msg.txt" {
		t.Fatalf("commit %q", got)
	}
}

// T-B-27 (REQ-10): RemoveWorktree never deletes outside the worktrees directory.
func TestRepos_RemoveWorktreeContained(t *testing.T) {
	r, _ := fakeRepos(t, func(Cmd) (int, error) { return 1, nil }) // git worktree remove fails -> manual path
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{victim, filepath.Join(r.ReposDir, ".nexus-worktrees"), filepath.Join(r.ReposDir, ".nexus-worktrees", "..", "x"), "/", r.ReposDir} {
		if err := r.RemoveWorktree(context.Background(), "", p); err == nil {
			t.Errorf("removal of %q allowed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatalf("victim directory damaged: %v", err)
	}
	inside := r.WorktreePath("job-1")
	if err := os.MkdirAll(filepath.Join(inside, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveWorktree(context.Background(), "", inside); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Fatal("worktree still there")
	}
}

// ---- real git: a local bare remote (T-B-28) ----

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// T-B-28 (REQ-5, REQ-10): worktree on a new nexus/ branch, .nexus never committed, push never touches main.
func TestRepos_RealGitWorktreeCommitPush(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitIn(t, root, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-m", "init")
	gitIn(t, seed, "push", "-u", "origin", "main")
	mainBefore := gitIn(t, remote, "rev-parse", "refs/heads/main")

	reposDir := filepath.Join(root, "repos")
	r := &Repos{Git: "git", GH: "gh", CloneWith: "git", ReposDir: reposDir, Runner: OSRunner{},
		Env: append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")}
	clone := r.ClonePath("owner/name")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "clone", remote, clone)
	ctx := context.Background()
	if dir, err := r.EnsureClone(ctx, "owner/name"); err != nil || dir != clone {
		t.Fatalf("fetch of an existing clone: %q %v", dir, err)
	}
	def, err := r.DefaultBranch(ctx, clone)
	if err != nil || def != "main" {
		t.Fatalf("default branch %q %v", def, err)
	}
	job := "0123abcd-0000-0000-0000-000000000000"
	branch := BranchName("FC-1", job)
	wt, base, err := r.PrepareWorktree(ctx, clone, job, branch, def)
	if err != nil || base != mainBefore {
		t.Fatalf("worktree %q base %q err %v", wt, base, err)
	}
	if cur, _ := r.CurrentBranch(ctx, wt); cur != branch {
		t.Fatalf("current branch %q", cur)
	}
	// reusing the worktree (a resumed attempt) works and keeps the branch
	if wt2, _, err := r.PrepareWorktree(ctx, clone, job, branch, def); err != nil || wt2 != wt {
		t.Fatalf("reuse: %q %v", wt2, err)
	}
	// private files are git-excluded; real changes are staged
	if err := os.WriteFile(filepath.Join(wt, ".nexus", "instructions.md"), []byte("SECRET TASK"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "new.go"), []byte("package x\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := r.StageAndDiff(ctx, wt, base)
	if err != nil || d.Files != 2 || d.Added != 4 || d.Removed != 0 {
		t.Fatalf("diff %+v %v", d, err)
	}
	msg := filepath.Join(wt, ".nexus", "commit-msg.txt")
	if err := os.WriteFile(msg, []byte("[FC-1] change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(ctx, wt, msg); err != nil {
		t.Fatal(err)
	}
	files := gitIn(t, wt, "show", "--name-only", "--pretty=format:", "HEAD")
	if strings.Contains(files, ".nexus") || !strings.Contains(files, "new.go") {
		t.Fatalf("committed files:\n%s", files)
	}
	if err := r.Push(ctx, wt, branch, def); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, remote, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatalf("main moved: %s -> %s", mainBefore, got)
	}
	if gitIn(t, remote, "rev-parse", "refs/heads/"+branch) == "" {
		t.Fatal("job branch not on the remote")
	}
	// the executor switched to main: the worker notices before pushing
	gitIn(t, wt, "checkout", "--quiet", "--detach")
	if cur, _ := r.CurrentBranch(ctx, wt); cur == branch {
		t.Fatal("branch check cannot tell that HEAD moved")
	}
	// removal
	if err := r.RemoveWorktree(ctx, clone, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree still exists")
	}
}
