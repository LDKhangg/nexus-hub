package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- shared helpers for the Batch B worker tests ----

const testHome = "/home/tester"

func baseYAML(t *testing.T, extra string) []byte {
	t.Helper()
	dir := t.TempDir()
	return []byte("hub_url: https://hub.example.com\nrepos_dir: " + filepath.Join(dir, "repos") +
		"\nstate_dir: " + filepath.Join(dir, "state") + "\ntoken_file: " + filepath.Join(dir, "tok") + "\n" + extra)
}

func mustConfig(t *testing.T, extra string) *Config {
	t.Helper()
	c, err := ParseConfig(baseYAML(t, extra), testHome)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// fakeRunner records every command and answers through script.
type fakeRunner struct {
	mu     sync.Mutex
	cmds   []Cmd
	script func(c Cmd) (int, error)
}

func (f *fakeRunner) Run(ctx context.Context, c Cmd) (int, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	script := f.script
	f.mu.Unlock()
	if c.OnStart != nil {
		c.OnStart(4242)
	}
	if script == nil {
		return 0, nil
	}
	return script(c)
}

func (f *fakeRunner) calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.cmds...)
}

func (f *fakeRunner) line(i int) string {
	c := f.calls()[i]
	return c.Name + " " + strings.Join(c.Args, " ")
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ---- REQ-5 / REQ-8: config strictness (T-B-1 .. T-B-5) ----

// T-B-1 (REQ-5): defaults are the W8 limits and a safe allow-list.
func TestConfig_DefaultsAreSafe(t *testing.T) {
	c := mustConfig(t, "")
	if c.Limits.WallMinutes != 60 || c.Limits.MaxTurns != 50 || c.Limits.MaxChangedLines != 2000 || c.RetentionDays != 7 {
		t.Fatalf("limits %+v retention %d", c.Limits, c.RetentionDays)
	}
	if c.Executor != ExecutorClaude || c.CloneWith != "gh" || c.ClickUp.BaseURL != "https://api.clickup.com" {
		t.Fatalf("defaults %q %q %q", c.Executor, c.CloneWith, c.ClickUp.BaseURL)
	}
	for _, tool := range c.Claude.AllowedTools {
		if tool == "Bash" || strings.HasPrefix(tool, "Bash(gh") || strings.HasPrefix(tool, "Bash(git push") {
			t.Fatalf("unsafe default tool %q", tool)
		}
	}
	// "~/" expands against home.
	c2, err := ParseConfig([]byte("hub_url: https://h.example\nrepos_dir: ~/work\n"), testHome)
	if err != nil || c2.ReposDir != filepath.Join(testHome, "work") {
		t.Fatalf("home expansion: %v %v", err, c2)
	}
}

// T-B-2 (REQ-5): the config parser is strict (abuse and typos).
func TestConfig_StrictRejections(t *testing.T) {
	cases := map[string]string{
		"unknown key":              "surprise: 1\n",
		"typo in nested key":       "limits:\n  wall_minute: 5\n",
		"second document":          "---\nhub_url: https://other.example\nrepos_dir: /x\n",
		"bad executor":             "executor: gpt\n",
		"bare Bash":                "claude:\n  allowed_tools: [Read, Bash]\n",
		"gh allowed":               "claude:\n  allowed_tools: ['Bash(gh pr create:*)']\n",
		"git push allowed":         "claude:\n  allowed_tools: ['Bash(git push:*)']\n",
		"tool with newline":        "claude:\n  allowed_tools: [\"Read\\nBash\"]\n",
		"repo key traversal":       "repos:\n  '../x/y':\n    test_command: [go, test]\n",
		"repo key with .git":       "repos:\n  a/b.git:\n    test_command: [go, test]\n",
		"repo key no owner":        "repos:\n  justname:\n    test_command: [go, test]\n",
		"empty test program":       "repos:\n  a/b:\n    test_command: ['  ']\n",
		"test timeout too long":    "repos:\n  a/b:\n    test_command: [go]\n    test_timeout_minutes: 999\n",
		"wall minutes too big":     "limits:\n  wall_minutes: 99999\n",
		"negative turns":           "limits:\n  max_turns: -1\n",
		"retention too long":       "retention_days: 4000\n",
		"clone_with other":         "clone_with: ssh\n",
		"executor path relative":   "executor_paths:\n  claude: bin/claude\n",
		"executor path flag":       "executor_paths:\n  git: -c\n",
		"model with shell":         "claude:\n  model: 'x; rm -rf /'\n",
		"clickup url http remote":  "clickup:\n  base_url: http://api.example.com\n",
		"clickup url with creds":   "clickup:\n  base_url: https://u:p@api.example.com\n",
		"state dir relative":       "state_dir: state\n",
		"token file relative":      "token_file: tok\n",
		"clickup token not found":  "clickup:\n  token_file: /nonexistent/clickup-token\n",
		"clickup token relative":   "clickup:\n  token_file: clickup-token\n",
		"executor path nul":        "executor_paths:\n  gh: \"a\\u0000b\"\n",
		"test arg too long (nul)":  "repos:\n  a/b:\n    test_command: [go, \"x\\u0000y\"]\n",
		"opencode agent with flag": "opencode:\n  agent: 'a b'\n",
	}
	for name, extra := range cases {
		if _, err := ParseConfig(baseYAML(t, extra), testHome); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
	// hub_url rules
	for _, u := range []string{"", "http://hub.example.com", "ftp://hub.example.com", "https://u:p@hub.example.com", "https://hub.example.com?x=1", "https://hub.example.com#f", "hub.example.com"} {
		if _, err := ParseConfig([]byte("hub_url: \""+u+"\"\nrepos_dir: /x\n"), testHome); err == nil {
			t.Errorf("hub_url %q accepted", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:3000/", "https://hub.example.com/"} {
		c, err := ParseConfig([]byte("hub_url: "+u+"\nrepos_dir: /x\n"), testHome)
		if err != nil {
			t.Errorf("hub_url %q rejected: %v", u, err)
		} else if strings.HasSuffix(c.HubURL, "/") {
			t.Errorf("hub_url keeps trailing slash: %q", c.HubURL)
		}
	}
	if _, err := ParseConfig(nil, testHome); err == nil {
		t.Error("empty config accepted")
	}
}

// T-B-3 (REQ-5): errors name the field, never the value (a pasted secret).
func TestConfig_ErrorsNeverEchoValues(t *testing.T) {
	_, err := ParseConfig([]byte("hub_url: https://user:SuperSecretPW@hub.example.com\nrepos_dir: /x\n"), testHome)
	if err == nil || strings.Contains(err.Error(), "SuperSecretPW") {
		t.Fatalf("error leaks the value: %v", err)
	}
	_, err = ParseConfig(baseYAML(t, "claude:\n  model: 'sk-ant-TOPSECRET; x'\n"), testHome)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("model error leaks the value: %v", err)
	}
	_, err = ParseConfig(baseYAML(t, "api_key: sk-ant-TOPSECRET\n"), testHome)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("unknown key error leaks the value: %v", err)
	}
}

// T-B-4 (REQ-5): LoadConfig missing, huge and ok files; token files need mode 0600.
func TestConfig_LoadAndSecretFileMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(filepath.Join(dir, "nope.yaml"), testHome); err == nil {
		t.Fatal("missing config accepted")
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte("hub_url: https://h.example\n"+strings.Repeat("# pad\n", 20000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(big, testHome); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("huge config: %v", err)
	}
	ok := filepath.Join(dir, "ok.yaml")
	if err := os.WriteFile(ok, baseYAML(t, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(ok, testHome); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if runtime.GOOS == "windows" {
		return // no POSIX modes
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("nhw_SECRETVALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSecretFile(tok); err != nil || got != "nhw_SECRETVALUE" {
		t.Fatalf("0600 token: %q %v", got, err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(tok, mode); err != nil {
			t.Fatal(err)
		}
		got, err := ReadSecretFile(tok)
		if err == nil || got != "" || strings.Contains(err.Error(), "SECRETVALUE") {
			t.Fatalf("mode %o accepted or leaked: %q %v", mode, got, err)
		}
	}
	link := filepath.Join(dir, "link")
	_ = os.Chmod(tok, 0o600)
	if err := os.Symlink(tok, link); err != nil {
		t.Fatal(err)
	}
	if CheckSecretFile(link) == nil {
		t.Fatal("a symlink to a secret file was accepted")
	}
	// a clickup token file with a loose mode fails the whole config
	_ = os.Chmod(tok, 0o644)
	if _, err := ParseConfig(baseYAML(t, "clickup:\n  token_file: "+tok+"\n"), testHome); err == nil {
		t.Fatal("config accepted a clickup token file with mode 0644")
	}
	_ = os.Chmod(tok, 0o600)
	if _, err := ParseConfig(baseYAML(t, "clickup:\n  token_file: "+tok+"\n"), testHome); err != nil {
		t.Fatalf("0600 clickup token rejected: %v", err)
	}
	// empty, multi-line and spaced secret files are malformed
	for _, content := range []string{"", "   \n", "a b", "line1\nline2"} {
		_ = os.WriteFile(tok, []byte(content), 0o600)
		if _, err := ReadSecretFile(tok); err == nil {
			t.Fatalf("malformed secret %q accepted", content)
		}
	}
}

// T-B-5 (REQ-5): ValidRepo keeps path tricks and .git out.
func TestValidRepo(t *testing.T) {
	for _, ok := range []string{"owner/repo", "a-b/c_d.e", "A1/b"} {
		if !ValidRepo(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "owner", "owner/", "/repo", "../x", "a/../b", "a/b/c", "a/b.git", "a/..", "-a/b", "a b/c", "a/b c", "a/b\n", "ö/b", strings.Repeat("a", 40) + "/b", "a/" + strings.Repeat("b", 101), "a/b;rm", "a/$(x)"} {
		if ValidRepo(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---- REQ-5: environment scrubbing (T-B-6) ----

// T-B-6 (REQ-5): the executor and test environment lose hub and API credentials; git keeps the owner's.
func TestScrubEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/home/x", "LANG=C.UTF-8",
		"NEXUS_DATABASE_URL=postgres://u:p@h/db", "NEXUS_TOKEN=a", "nexus_lower=1", "DATABASE_URL=x", "TEST_DATABASE_URL=y",
		"ANTHROPIC_API_KEY=sk-ant-1", "OPENAI_API_KEY=sk", "GH_TOKEN=ghp_x", "GITHUB_TOKEN=ghp_y", "SOME_SECRET=1", "DB_PASSWORD=1",
		"AWS_SECRET_ACCESS_KEY=1", "SSH_PRIVATE_KEY=1", "GOOGLE_CREDENTIALS=1", "CLICKUP_TOKEN=1", "TELEGRAM_BOT_TOKEN=1",
		"malformed-entry-without-equals",
	}
	got := ScrubEnv(in)
	keep := map[string]bool{"PATH=/usr/bin": true, "HOME=/home/x": true, "LANG=C.UTF-8": true}
	if len(got) != len(keep) {
		t.Fatalf("scrubbed env = %v", got)
	}
	for _, kv := range got {
		if !keep[kv] {
			t.Fatalf("leaked %q", kv)
		}
	}
	git := GitEnv(in)
	joined := "\n" + strings.Join(git, "\n") + "\n"
	for _, gone := range []string{"NEXUS_", "DATABASE_URL", "ANTHROPIC_API_KEY", "CLICKUP", "TELEGRAM"} {
		if strings.Contains(strings.ToUpper(joined), "\n"+gone) {
			t.Errorf("git env keeps %s", gone)
		}
	}
	for _, kept := range []string{"\nGH_TOKEN=ghp_x", "\nGITHUB_TOKEN=ghp_y", "\nGIT_TERMINAL_PROMPT=0", "\nGH_PROMPT_DISABLED=1", "\nPATH=/usr/bin"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("git env lost %q", kept)
		}
	}
}

// ---- REQ-5: the OS runner kills the process group and scrubs env (T-B-7) ----

// T-B-7 (REQ-5/9): OSRunner returns exit codes, scrubs NEXUS_*, and kills on cancel.
func TestOSRunner_ExitScrubAndCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell needed")
	}
	t.Setenv("NEXUS_SECRET_PROBE", "leak-me")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-probe")
	var out strings.Builder
	exit, err := OSRunner{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", `printf "%s|%s|%s" "$NEXUS_SECRET_PROBE" "$ANTHROPIC_API_KEY" "$HOME"; exit 3`}, Stdout: &out})
	if err != nil || exit != 3 {
		t.Fatalf("exit %d err %v", exit, err)
	}
	if strings.Contains(out.String(), "leak-me") || strings.Contains(out.String(), "sk-ant-probe") {
		t.Fatalf("child saw credentials: %q", out.String())
	}
	if _, err := (OSRunner{}).Run(context.Background(), Cmd{Name: "/definitely/not/a/binary"}); err != ErrNotStarted {
		t.Fatalf("missing binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	exit, err = OSRunner{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30"}})
	if err == nil || exit != -1 || time.Since(start) > 15*time.Second {
		t.Fatalf("cancel: exit %d err %v after %v", exit, err, time.Since(start))
	}
}

// ---- buffers and clipping (T-B-8) ----

// T-B-8 (REQ-7): output buffers are bounded and clipping keeps valid UTF-8.
func TestBuffersAndClip(t *testing.T) {
	c := newCapBuffer(10)
	_, _ = c.Write([]byte("0123456789ABC"))
	_, _ = c.Write([]byte("more"))
	if c.String() != "0123456789" || !c.truncated {
		t.Fatalf("cap buffer %q", c.String())
	}
	tail := newTailBuffer(5)
	_, _ = tail.Write([]byte("abcdefgh"))
	_, _ = tail.Write([]byte("XYZ"))
	if tail.String() != "ghXYZ" {
		t.Fatalf("tail buffer %q", tail.String())
	}
	long := strings.Repeat("é", 5000) + "\xff\xfe"
	got := clipRunes(long, 2000)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 2000 {
		t.Fatalf("clip: %d runes valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if g := clipRunesTail(strings.Repeat("😀", 50), 10); utf8.RuneCountInString(g) != 10 || !strings.HasPrefix(g, "…") {
		t.Fatalf("tail clip %q", g)
	}
	if stripANSI("\x1b[31mred\x1b[0m plain") != "red plain" {
		t.Fatal("ansi not stripped")
	}
}
