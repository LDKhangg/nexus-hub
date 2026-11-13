package tenant

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Tier is a tool permission tier.
type Tier string

const (
	TierRead      Tier = "read"
	TierWrite     Tier = "write"
	TierDangerous Tier = "dangerous"
)

// GitHubAccount is a GitHub identity referenced by repo grants. It holds
// credential references only, never secret values.
type GitHubAccount struct {
	Name, Type, TokenRef, Login, AppID, InstallationID, PrivateKeyRef, CommitName, CommitEmail string
}

// RepoGrant is the per-repository tool policy.
type RepoGrant struct {
	Repo, Account, LocalPath                                                string
	Managed                                                                 bool
	Allow, PushBranches, ProtectedBranches, MergeMethods, MergeBaseBranches []string
	RequireChecksGreen                                                      bool
}

// ToolPolicyConfig is the tool section of an agent configuration.
type ToolPolicyConfig struct {
	Enabled         []string
	DryRun          bool
	MaxWritesPerJob int
	Accounts        map[string]GitHubAccount
	Repos           map[string]RepoGrant
}

// AgentConfig is the parsed integrations.config.agent of a ClickUp source.
type AgentConfig struct {
	Enabled             bool
	ConfidenceThreshold float64
	Persona             string
	ClickUpTokenRef     string
	// ClickUpTeamID is the integration's clickup_team_id (the ClickUp
	// workspace ID), "" when absent or not numeric. It is informational:
	// a missing or invalid value never fails the config.
	ClickUpTeamID string
	Tools         ToolPolicyConfig
}

const (
	DefaultConfidenceThreshold = 0.85
	DefaultMaxWritesPerJob     = 2
	MaxWritesPerJobLimit       = 5
	DefaultClickUpTokenRef     = "clickup"
	maxPersonaRunes            = 2000
)

// DefaultEnabledTools are the read tools enabled when the config lists none.
var DefaultEnabledTools = []string{
	"github_pr_view", "github_pr_checks", "github_pr_files", "github_issue_view",
	"code_list_files", "code_search", "code_read_file", "code_outline",
	"git_log", "git_diff", "git_blame", "git_status", "clickup_get_task",
}

// WorkspaceTools mutate a managed clone and require managed:true.
var WorkspaceTools = map[string]bool{
	"git_sync": true, "git_pull": true, "git_checkout": true, "git_commit": true,
	"git_config_set": true, "git_push": true, "git_reset_hard": true, "git_clean": true,
}

var (
	repoRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	credRefRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	branchRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	numericIDRe  = regexp.MustCompile(`^[0-9]{1,20}$`)
	mergeMethods = map[string]bool{"merge": true, "squash": true, "rebase": true}
)

// ValidRepo reports whether s is a safe "owner/repo" name.
func ValidRepo(s string) bool {
	if !repoRe.MatchString(s) {
		return false
	}
	name := s[strings.IndexByte(s, '/')+1:]
	return name != "." && name != ".."
}

// ValidCredentialRef reports whether s is a valid credentials.name.
func ValidCredentialRef(s string) bool { return credRefRe.MatchString(s) }

// ValidBranchName applies the syntactic branch rules shared by config and
// git tools (git check-ref-format is applied additionally at run time).
func ValidBranchName(s string) bool {
	if !branchRe.MatchString(s) || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	return !strings.HasSuffix(s, ".lock") && !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".")
}

// MatchBranchPattern matches a branch against a pattern that is either an
// exact name or a prefix followed by a single trailing "*". The "*" must
// match at least one character.
func MatchBranchPattern(pattern, branch string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return len(branch) > len(prefix) && strings.HasPrefix(branch, prefix)
	}
	return pattern == branch
}

func validPushPattern(p string) bool {
	prefix, _ := strings.CutSuffix(p, "*")
	if prefix == "" || strings.Contains(prefix, "*") {
		return false
	}
	// A prefix such as "nexus/" is not a valid branch on its own, so check
	// the pattern with a placeholder for the glob.
	if strings.HasSuffix(p, "*") {
		return ValidBranchName(prefix + "x")
	}
	return ValidBranchName(prefix)
}

var errAgentConfig = errors.New("invalid agent configuration")

func agentErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errAgentConfig, fmt.Sprintf(format, args...))
}

// clickUpTeamIDRe accepts a ClickUp workspace (team) ID.
var clickUpTeamIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)

// ParseAgentConfig parses source["agent"] (and reads source's
// clickup_team_id into ClickUpTeamID). known maps every registered tool
// name to its tier; tool names outside it are rejected.
func ParseAgentConfig(source map[string]any, known map[string]Tier) (AgentConfig, error) {
	c := AgentConfig{
		ConfidenceThreshold: DefaultConfidenceThreshold,
		ClickUpTokenRef:     DefaultClickUpTokenRef,
		Tools: ToolPolicyConfig{
			MaxWritesPerJob: DefaultMaxWritesPerJob,
			Accounts:        map[string]GitHubAccount{},
			Repos:           map[string]RepoGrant{},
		},
	}
	if team, ok := source["clickup_team_id"].(string); ok && clickUpTeamIDRe.MatchString(strings.TrimSpace(team)) {
		c.ClickUpTeamID = strings.TrimSpace(team)
	}
	raw, present := source["agent"]
	if !present || raw == nil {
		c.Tools.Enabled = defaultEnabled(known)
		return c, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return c, agentErr("agent must be an object")
	}
	var err error
	if c.Enabled, err = optBool(m, "enabled", false); err != nil {
		return c, err
	}
	if v, ok := m["confidence_threshold"]; ok {
		f, isNum := v.(float64)
		if !isNum || f <= 0 || f > 1 {
			return c, agentErr("confidence_threshold must be in (0,1]")
		}
		c.ConfidenceThreshold = f
	}
	if v, ok := m["persona"]; ok {
		s, isStr := v.(string)
		if !isStr || utf8.RuneCountInString(s) > maxPersonaRunes {
			return c, agentErr("persona must be a string of at most %d runes", maxPersonaRunes)
		}
		c.Persona = strings.TrimSpace(s)
	}
	if v, ok := m["clickup_token_ref"]; ok {
		s, isStr := v.(string)
		if !isStr || !ValidCredentialRef(s) {
			return c, agentErr("invalid clickup_token_ref")
		}
		c.ClickUpTokenRef = s
	}
	toolsRaw, ok := m["tools"]
	if !ok || toolsRaw == nil {
		c.Tools.Enabled = defaultEnabled(known)
		return c, nil
	}
	tm, ok := toolsRaw.(map[string]any)
	if !ok {
		return c, agentErr("tools must be an object")
	}
	if err := parseToolPolicy(tm, known, &c.Tools); err != nil {
		return c, err
	}
	return c, nil
}

func defaultEnabled(known map[string]Tier) []string {
	out := make([]string, 0, len(DefaultEnabledTools))
	for _, name := range DefaultEnabledTools {
		if known[name] == TierRead {
			out = append(out, name)
		}
	}
	return out
}

func parseToolPolicy(tm map[string]any, known map[string]Tier, p *ToolPolicyConfig) error {
	if _, ok := tm["enabled"]; ok {
		enabled, err := stringList(tm, "enabled")
		if err != nil {
			return err
		}
		for _, name := range enabled {
			if _, ok := known[name]; !ok {
				return agentErr("unknown tool %q", name)
			}
		}
		p.Enabled = enabled
	} else {
		p.Enabled = defaultEnabled(known)
	}
	var err error
	if p.DryRun, err = optBool(tm, "dry_run", false); err != nil {
		return err
	}
	if v, ok := tm["max_writes_per_job"]; ok {
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || f < 0 || f > MaxWritesPerJobLimit {
			return agentErr("max_writes_per_job must be an integer in [0,%d]", MaxWritesPerJobLimit)
		}
		p.MaxWritesPerJob = int(f)
	}
	ghRaw, ok := tm["github"]
	if !ok || ghRaw == nil {
		return nil
	}
	gh, ok := ghRaw.(map[string]any)
	if !ok {
		return agentErr("github must be an object")
	}
	if err := parseAccounts(gh, p); err != nil {
		return err
	}
	return parseRepos(gh, known, p)
}

func parseAccounts(gh map[string]any, p *ToolPolicyConfig) error {
	raw, ok := gh["accounts"]
	if !ok || raw == nil {
		return nil
	}
	accounts, ok := raw.(map[string]any)
	if !ok {
		return agentErr("accounts must be an object")
	}
	for name, v := range accounts {
		if !ValidCredentialRef(name) {
			return agentErr("invalid account name %q", name)
		}
		am, ok := v.(map[string]any)
		if !ok {
			return agentErr("account %q must be an object", name)
		}
		a := GitHubAccount{
			Name: name, Type: stringField(am, "type"), TokenRef: stringField(am, "token_ref"),
			Login: stringField(am, "login"), AppID: stringField(am, "app_id"),
			InstallationID: stringField(am, "installation_id"), PrivateKeyRef: stringField(am, "private_key_ref"),
			CommitName: stringField(am, "commit_name"), CommitEmail: stringField(am, "commit_email"),
		}
		switch a.Type {
		case "pat":
			if !ValidCredentialRef(a.TokenRef) || a.Login == "" {
				return agentErr("account %q: pat needs token_ref and login", name)
			}
		case "app":
			if !numericIDRe.MatchString(a.AppID) || !numericIDRe.MatchString(a.InstallationID) || !ValidCredentialRef(a.PrivateKeyRef) {
				return agentErr("account %q: app needs numeric app_id, installation_id and private_key_ref", name)
			}
		default:
			return agentErr("account %q: type must be pat or app", name)
		}
		if a.CommitName == "" || !strings.Contains(a.CommitEmail, "@") || strings.ContainsAny(a.CommitName+a.CommitEmail, "\n\r<>") {
			return agentErr("account %q: commit_name and commit_email required", name)
		}
		p.Accounts[name] = a
	}
	return nil
}

func parseRepos(gh map[string]any, known map[string]Tier, p *ToolPolicyConfig) error {
	raw, ok := gh["repos"]
	if !ok || raw == nil {
		return nil
	}
	repos, ok := raw.(map[string]any)
	if !ok {
		return agentErr("repos must be an object")
	}
	for key, v := range repos {
		if !ValidRepo(key) {
			return agentErr("invalid repo %q", key)
		}
		rm, ok := v.(map[string]any)
		if !ok {
			return agentErr("repo %q must be an object", key)
		}
		g := RepoGrant{Repo: key, Account: stringField(rm, "account"), LocalPath: stringField(rm, "local_path")}
		if _, ok := p.Accounts[g.Account]; !ok {
			return agentErr("repo %q: unknown account %q", key, g.Account)
		}
		if g.LocalPath != "" && (!filepath.IsAbs(g.LocalPath) || filepath.Clean(g.LocalPath) != g.LocalPath) {
			return agentErr("repo %q: local_path must be absolute and clean", key)
		}
		var err error
		if g.Managed, err = optBool(rm, "managed", false); err != nil {
			return err
		}
		if g.RequireChecksGreen, err = optBool(rm, "require_checks_green", true); err != nil {
			return err
		}
		if g.Allow, err = stringList(rm, "allow"); err != nil {
			return err
		}
		for _, name := range g.Allow {
			tier, ok := known[name]
			if !ok {
				return agentErr("repo %q: unknown tool %q", key, name)
			}
			if tier == TierRead {
				return agentErr("repo %q: read tool %q does not belong in allow", key, name)
			}
			if WorkspaceTools[name] && !g.Managed {
				return agentErr("repo %q: workspace tool %q requires managed", key, name)
			}
		}
		if g.PushBranches, err = listWithDefault(rm, "push_branches", []string{"nexus/*"}); err != nil {
			return err
		}
		for _, pat := range g.PushBranches {
			if !validPushPattern(pat) {
				return agentErr("repo %q: invalid push pattern %q", key, pat)
			}
		}
		if g.ProtectedBranches, err = listWithDefault(rm, "protected_branches", []string{"main", "master"}); err != nil {
			return err
		}
		if g.MergeBaseBranches, err = listWithDefault(rm, "merge_base_branches", []string{}); err != nil {
			return err
		}
		for _, b := range append(append([]string{}, g.ProtectedBranches...), g.MergeBaseBranches...) {
			if !ValidBranchName(b) {
				return agentErr("repo %q: invalid branch %q", key, b)
			}
		}
		if g.MergeMethods, err = listWithDefault(rm, "merge_methods", []string{}); err != nil {
			return err
		}
		for _, mm := range g.MergeMethods {
			if !mergeMethods[mm] {
				return agentErr("repo %q: invalid merge method %q", key, mm)
			}
		}
		p.Repos[key] = g
	}
	return nil
}

func optBool(m map[string]any, key string, def bool) (bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return def, agentErr("%s must be a boolean", key)
	}
	return b, nil
}

func stringList(m map[string]any, key string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return []string{}, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, agentErr("%s must be a list", key)
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, agentErr("%s must contain non-empty strings", key)
		}
		s = strings.TrimSpace(s)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func listWithDefault(m map[string]any, key string, def []string) ([]string, error) {
	if v, ok := m[key]; !ok || v == nil {
		return def, nil
	}
	return stringList(m, key)
}
