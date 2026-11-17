// Package worker is the laptop side of the Phase 7a local worker: it polls
// the hub for jobs, runs each one with a local executor in its own git
// worktree, opens a draft PR with the owner's credentials and reports
// progress and a structured result through a durable outbox.
//
// The worker never logs or sends prompts, instructions, code, diffs,
// executor output (except the bounded result summary), tokens, paths,
// hostnames or process lists.
package worker

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Executors the worker can drive.
const (
	ExecutorClaude   = "claude"
	ExecutorOpencode = "opencode"
)

// W8 defaults.
const (
	DefaultWallMinutes      = 60
	DefaultMaxTurns         = 50
	DefaultMaxChangedLines  = 2000
	DefaultRetentionDays    = 7
	DefaultTestTimeoutMin   = 15
	maxConfigBytes          = 64 << 10
	maxTestCommandArgs      = 32
	maxAllowedTools         = 64
	defaultConfigName       = "worker.yaml"
	defaultTokenName        = "worker-token"
	defaultStateSubdir      = "nexus-hub/worker"
	defaultConfigSubdir     = "nexus-hub"
	maxConfigRepos          = 200
	maxExecutorPathLen      = 4096
	maxTestCommandArgLength = 4096
)

// DefaultClaudeAllowedTools is the allow-list passed to `claude -p
// --allowedTools` when the config names none: file tools only. No shell at
// all: Bash prefix rules cannot be made safe (`git diff --no-index` reads
// any file, `git log --output` writes anywhere, a written .git/config runs
// code in the worker's own git calls). Anything else is denied in headless
// mode because no one can approve it.
var DefaultClaudeAllowedTools = []string{
	"Read", "Edit", "MultiEdit", "Write", "Glob", "Grep", "LS", "TodoWrite",
}

// claudeDisallowedTools are always denied, whatever the allow-list says.
var claudeDisallowedTools = []string{
	"Bash", "Bash(git push:*)", "Bash(git commit:*)", "Bash(git reset:*)", "Bash(git checkout:*)",
	"Bash(git switch:*)", "Bash(git rebase:*)", "Bash(git merge:*)", "Bash(gh:*)",
	"WebFetch", "WebSearch",
}

// Config is ~/.config/nexus-hub/worker.yaml. It never holds a secret: the
// hub device token lives in worker-token and the ClickUp token in the file
// named by clickup.token_file, both mode 0600.
type Config struct {
	HubURL        string                `yaml:"hub_url"`
	ReposDir      string                `yaml:"repos_dir"`
	Executor      string                `yaml:"executor"`
	ExecutorPaths ExecutorPaths         `yaml:"executor_paths"`
	Claude        ClaudeConfig          `yaml:"claude"`
	Opencode      OpencodeConfig        `yaml:"opencode"`
	Repos         map[string]RepoConfig `yaml:"repos"`
	Limits        Limits                `yaml:"limits"`
	RetentionDays int                   `yaml:"retention_days"`
	CloneWith     string                `yaml:"clone_with"` // gh (default) | git
	ClickUp       ClickUpConfig         `yaml:"clickup"`
	StateDir      string                `yaml:"state_dir"` // default ~/.local/state/nexus-hub/worker
	TokenFile     string                `yaml:"token_file"`
}

// ExecutorPaths are the binaries the worker runs; bare names use PATH.
type ExecutorPaths struct {
	Claude   string `yaml:"claude"`
	Opencode string `yaml:"opencode"`
	Git      string `yaml:"git"`
	GH       string `yaml:"gh"`
}

// ClaudeConfig tunes `claude -p`.
type ClaudeConfig struct {
	Model        string   `yaml:"model"`
	AllowedTools []string `yaml:"allowed_tools"`
}

// OpencodeConfig tunes `opencode run`.
type OpencodeConfig struct {
	Model string `yaml:"model"`
	Agent string `yaml:"agent"`
}

// RepoConfig is per-repository ("owner/name") settings.
type RepoConfig struct {
	TestCommand        []string `yaml:"test_command"` // argv; no shell
	TestTimeoutMinutes int      `yaml:"test_timeout_minutes"`
}

// Limits are the W8 per-job limits.
type Limits struct {
	WallMinutes     int `yaml:"wall_minutes"`
	MaxTurns        int `yaml:"max_turns"`
	MaxChangedLines int `yaml:"max_changed_lines"`
}

// ClickUpConfig points to the laptop's read-only ClickUp token file.
type ClickUpConfig struct {
	TokenFile string `yaml:"token_file"`
	BaseURL   string `yaml:"base_url"` // default https://api.clickup.com
}

var (
	repoKeyRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	modelRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
	allowedToolRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}(\([^()\n]{1,200}\))?$`)
)

// ValidRepo reports whether repo is a GitHub "owner/name".
func ValidRepo(repo string) bool {
	return repoKeyRE.MatchString(repo) && !strings.Contains(repo, "..") && !strings.HasSuffix(repo, ".git")
}

// DefaultConfigPath is ~/.config/nexus-hub/worker.yaml.
func DefaultConfigPath(home string) string {
	return filepath.Join(home, ".config", defaultConfigSubdir, defaultConfigName)
}

// DefaultTokenPath is the device token written by `nh worker enroll`.
func DefaultTokenPath(home string) string {
	return filepath.Join(home, ".config", defaultConfigSubdir, defaultTokenName)
}

// DefaultStateDir is ~/.local/state/nexus-hub/worker.
func DefaultStateDir(home string) string {
	return filepath.Join(home, ".local", "state", defaultStateSubdir)
}

// LoadConfig reads and strictly validates the config file: unknown keys,
// a second YAML document or an invalid value are errors. Errors name the
// field, never its value.
func LoadConfig(path, home string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("worker config not found")
		}
		return nil, errors.New("worker config is not readable")
	}
	defer f.Close()
	raw := make([]byte, maxConfigBytes+1)
	n, _ := readFull(f, raw)
	if n > maxConfigBytes {
		return nil, errors.New("worker config is too large")
	}
	return ParseConfig(raw[:n], home)
}

func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ParseConfig decodes and validates config bytes, fills defaults and
// expands "~/" in paths against home.
func ParseConfig(raw []byte, home string) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("worker config is not valid YAML or has unknown keys")
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("worker config must be one YAML document")
	}
	if err := c.normalize(home); err != nil {
		return nil, err
	}
	return &c, nil
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func bad(field string) error { return fmt.Errorf("worker config: invalid %s", field) }

func (c *Config) normalize(home string) error {
	if err := validHubURL(c.HubURL); err != nil {
		return err
	}
	c.HubURL = strings.TrimRight(c.HubURL, "/")
	c.ReposDir = filepath.Clean(expandHome(c.ReposDir, home))
	if c.ReposDir == "." || !filepath.IsAbs(c.ReposDir) {
		return bad("repos_dir (must be an absolute path)")
	}
	if c.Executor == "" {
		c.Executor = ExecutorClaude
	}
	if c.Executor != ExecutorClaude && c.Executor != ExecutorOpencode {
		return bad("executor (claude or opencode)")
	}
	p := &c.ExecutorPaths
	for _, x := range []struct {
		v    *string
		def  string
		name string
	}{{&p.Claude, "claude", "executor_paths.claude"}, {&p.Opencode, "opencode", "executor_paths.opencode"},
		{&p.Git, "git", "executor_paths.git"}, {&p.GH, "gh", "executor_paths.gh"}} {
		if *x.v == "" {
			*x.v = x.def
		}
		*x.v = expandHome(*x.v, home)
		if len(*x.v) > maxExecutorPathLen || strings.ContainsAny(*x.v, "\x00\n\r") || strings.HasPrefix(*x.v, "-") {
			return bad(x.name)
		}
		if strings.ContainsRune(*x.v, filepath.Separator) && !filepath.IsAbs(*x.v) {
			return bad(x.name + " (a path must be absolute)")
		}
	}
	if c.Claude.Model != "" && !modelRE.MatchString(c.Claude.Model) {
		return bad("claude.model")
	}
	if c.Opencode.Model != "" && !modelRE.MatchString(c.Opencode.Model) {
		return bad("opencode.model")
	}
	if c.Opencode.Agent != "" && !modelRE.MatchString(c.Opencode.Agent) {
		return bad("opencode.agent")
	}
	if len(c.Claude.AllowedTools) > maxAllowedTools {
		return bad("claude.allowed_tools")
	}
	for _, t := range c.Claude.AllowedTools {
		if !allowedToolRE.MatchString(t) || forbiddenClaudeTool(t) {
			return bad("claude.allowed_tools (file tools only: no Bash, web or MCP tools)")
		}
	}
	if len(c.Claude.AllowedTools) == 0 {
		c.Claude.AllowedTools = append([]string(nil), DefaultClaudeAllowedTools...)
	}
	if len(c.Repos) > maxConfigRepos {
		return bad("repos")
	}
	for name, r := range c.Repos {
		if !ValidRepo(name) {
			return bad("repos key (owner/name)")
		}
		if len(r.TestCommand) > maxTestCommandArgs {
			return bad("repos test_command")
		}
		for i, a := range r.TestCommand {
			if len(a) > maxTestCommandArgLength || strings.ContainsRune(a, 0) || (i == 0 && strings.TrimSpace(a) == "") {
				return bad("repos test_command")
			}
		}
		if r.TestTimeoutMinutes == 0 {
			r.TestTimeoutMinutes = DefaultTestTimeoutMin
		}
		if r.TestTimeoutMinutes < 1 || r.TestTimeoutMinutes > 240 {
			return bad("repos test_timeout_minutes (1-240)")
		}
		c.Repos[name] = r
	}
	l := &c.Limits
	if l.WallMinutes == 0 {
		l.WallMinutes = DefaultWallMinutes
	}
	if l.MaxTurns == 0 {
		l.MaxTurns = DefaultMaxTurns
	}
	if l.MaxChangedLines == 0 {
		l.MaxChangedLines = DefaultMaxChangedLines
	}
	if l.WallMinutes < 1 || l.WallMinutes > 24*60 {
		return bad("limits.wall_minutes (1-1440)")
	}
	if l.MaxTurns < 1 || l.MaxTurns > 1000 {
		return bad("limits.max_turns (1-1000)")
	}
	if l.MaxChangedLines < 1 || l.MaxChangedLines > 1_000_000 {
		return bad("limits.max_changed_lines (1-1000000)")
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = DefaultRetentionDays
	}
	if c.RetentionDays < 1 || c.RetentionDays > 365 {
		return bad("retention_days (1-365)")
	}
	if c.CloneWith == "" {
		c.CloneWith = "gh"
	}
	if c.CloneWith != "gh" && c.CloneWith != "git" {
		return bad("clone_with (gh or git)")
	}
	if c.ClickUp.TokenFile != "" {
		c.ClickUp.TokenFile = filepath.Clean(expandHome(c.ClickUp.TokenFile, home))
		if !filepath.IsAbs(c.ClickUp.TokenFile) {
			return bad("clickup.token_file (must be an absolute path)")
		}
		if err := CheckSecretFile(c.ClickUp.TokenFile); err != nil {
			return fmt.Errorf("worker config: clickup.token_file: %w", err)
		}
	}
	if c.ClickUp.BaseURL == "" {
		c.ClickUp.BaseURL = "https://api.clickup.com"
	}
	if u, err := url.Parse(c.ClickUp.BaseURL); err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return bad("clickup.base_url")
	}
	c.ClickUp.BaseURL = strings.TrimRight(c.ClickUp.BaseURL, "/")
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir(home)
	}
	c.StateDir = filepath.Clean(expandHome(c.StateDir, home))
	if !filepath.IsAbs(c.StateDir) {
		return bad("state_dir (must be an absolute path)")
	}
	if c.TokenFile == "" {
		c.TokenFile = DefaultTokenPath(home)
	}
	c.TokenFile = filepath.Clean(expandHome(c.TokenFile, home))
	if !filepath.IsAbs(c.TokenFile) {
		return bad("token_file (must be an absolute path)")
	}
	return nil
}

// forbiddenClaudeTool reports a tool rule the worker never allows: any
// Bash rule (shell), web access and MCP tools.
func forbiddenClaudeTool(rule string) bool {
	name, _, _ := strings.Cut(rule, "(")
	switch strings.ToLower(name) {
	case "bash", "webfetch", "websearch":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "mcp__")
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func validHubURL(raw string) error {
	if raw == "" {
		return bad("hub_url (required)")
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("hub_url")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return bad("hub_url (must be https)")
	}
	return nil
}

// RepoTest returns the test command of repo, if configured.
func (c *Config) RepoTest(repo string) (RepoConfig, bool) {
	r, ok := c.Repos[repo]
	return r, ok && len(r.TestCommand) > 0
}

// ReadSecretFile returns the trimmed content of a mode-0600 secret file.
// Errors never include the content.
func ReadSecretFile(path string) (string, error) {
	if err := CheckSecretFile(path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("secret file is not readable")
	}
	s := strings.TrimSpace(string(b))
	if s == "" || len(s) > 4096 || strings.ContainsAny(s, "\r\n\x00 ") {
		return "", errors.New("secret file is empty or malformed")
	}
	return s, nil
}

// CheckSecretFile requires a regular file that only its owner can read.
func CheckSecretFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return errors.New("secret file not found")
	}
	if !fi.Mode().IsRegular() {
		return errors.New("secret file is not a regular file")
	}
	if !secretModeOK(fi.Mode()) {
		return errors.New("secret file must have mode 0600")
	}
	return nil
}
