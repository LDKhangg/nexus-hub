package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// githubAPIBase and githubHTTP are overridden in tests.
var (
	githubAPIBase = "https://api.github.com"
	githubHTTP    = func() *http.Client { return netguard.Client(30*time.Second, netguard.DefaultHosts) }
)

// knownTools lists every tool the server registers, for config validation.
func knownTools() map[string]tenant.Tier {
	all := append([]tools.Tool{}, githubtools.NewReadTools(nil)...)
	all = append(all, githubtools.NewWriteTools(nil, nil)...)
	all = append(all, githubtools.NewDangerousTools(nil, nil)...)
	all = append(all, clickuptools.NewTaskTool(nil, nil))
	all = append(all, codetools.NewReadTools(nil, "/nonexistent")...)
	all = append(all, codetools.NewWorkspaceTools(nil, nil, "/nonexistent", 0, nil)...)
	out := map[string]tenant.Tier{}
	for _, t := range all {
		s := t.Spec()
		out[s.Name] = s.Tier
	}
	return out
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func anyList(l []string) []any {
	out := make([]any, len(l))
	for i, v := range l {
		out[i] = v
	}
	return out
}

// editAgentConfig mutates config.agent of a ClickUp source in one
// transaction and validates the result before committing.
func editAgentConfig(ctx context.Context, pool *pgxpool.Pool, sourceID string, edit func(agent map[string]any) error) (tenant.AgentConfig, error) {
	if !tenant.ValidUUID(sourceID) {
		return tenant.AgentConfig{}, errors.New("valid --source-id required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return tenant.AgentConfig{}, errors.New("database operation failed")
	}
	defer rollback(tx)
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1 AND platform='clickup' AND role IN ('source','both') FOR UPDATE`, sourceID).Scan(&data); err != nil {
		return tenant.AgentConfig{}, errors.New("ClickUp source not found")
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil || cfg == nil {
		return tenant.AgentConfig{}, errors.New("invalid source configuration")
	}
	agent, _ := cfg["agent"].(map[string]any)
	if agent == nil {
		agent = map[string]any{}
	}
	if err := edit(agent); err != nil {
		return tenant.AgentConfig{}, err
	}
	cfg["agent"] = agent
	parsed, err := tenant.ParseAgentConfig(cfg, knownTools())
	if err != nil {
		return parsed, err
	}
	encoded, _ := json.Marshal(cfg)
	if _, err := tx.Exec(ctx, `UPDATE integrations SET config=$2, updated_at=now() WHERE id=$1`, sourceID, encoded); err != nil {
		return parsed, errors.New("config update failed")
	}
	if tx.Commit(ctx) != nil {
		return parsed, errors.New("config commit failed")
	}
	return parsed, nil
}

func sub(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	return m
}

func setAgentEnabled(ctx context.Context, pool *pgxpool.Pool, name string, args []string, out io.Writer, enabled bool) error {
	f := commandFlags(name)
	source := f.String("source-id", "", "ClickUp source UUID")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return errors.New(name + " requires --source-id")
	}
	if _, err := editAgentConfig(ctx, pool, *source, func(a map[string]any) error { a["enabled"] = enabled; return nil }); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"source_id": *source, "agent_enabled": enabled})
}

func githubAccountCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: github-account add ...")
	}
	f := commandFlags("github-account add")
	source := f.String("source-id", "", "")
	name := f.String("name", "", "")
	patRef := f.String("pat-ref", "", "")
	login := f.String("login", "", "")
	appID := f.String("app-id", "", "")
	inst := f.String("installation-id", "", "")
	keyRef := f.String("key-ref", "", "")
	commitName := f.String("commit-name", "", "")
	commitEmail := f.String("commit-email", "", "")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || *name == "" {
		return errors.New("github-account add requires --source-id --name and either --pat-ref/--login or --app-id/--installation-id/--key-ref")
	}
	acc := map[string]any{"commit_name": *commitName, "commit_email": *commitEmail}
	switch {
	case *patRef != "" && *appID == "":
		acc["type"], acc["token_ref"], acc["login"] = "pat", *patRef, *login
	case *appID != "" && *patRef == "":
		acc["type"], acc["app_id"], acc["installation_id"], acc["private_key_ref"] = "app", *appID, *inst, *keyRef
	default:
		return errors.New("choose exactly one of --pat-ref or --app-id")
	}
	if _, err := editAgentConfig(ctx, pool, *source, func(a map[string]any) error {
		sub(a, "tools", "github", "accounts")[*name] = acc
		return nil
	}); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"source_id": *source, "account": *name, "type": acc["type"]})
}

func githubRepoCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New("usage: github-repo set ...")
	}
	f := flag.NewFlagSet("github-repo set", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	source := f.String("source-id", "", "")
	repo := f.String("repo", "", "")
	account := f.String("account", "", "")
	managed := f.Bool("managed", false, "")
	localPath := f.String("local-path", "", "")
	noChecks := f.Bool("no-require-checks", false, "")
	var allow, push, protected, methods, bases listFlag
	f.Var(&allow, "allow", "")
	f.Var(&push, "push-branch", "")
	f.Var(&protected, "protected-branch", "")
	f.Var(&methods, "merge-method", "")
	f.Var(&bases, "merge-base", "")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidRepo(*repo) || *account == "" {
		return errors.New("github-repo set requires --source-id --repo owner/name --account")
	}
	grant := map[string]any{"account": *account, "managed": *managed, "local_path": *localPath, "require_checks_green": !*noChecks}
	for key, l := range map[string]listFlag{"allow": allow, "push_branches": push, "protected_branches": protected, "merge_methods": methods, "merge_base_branches": bases} {
		if len(l) > 0 {
			grant[key] = anyList(l)
		}
	}
	parsed, err := editAgentConfig(ctx, pool, *source, func(a map[string]any) error {
		sub(a, "tools", "github", "repos")[*repo] = grant
		return nil
	})
	if err != nil {
		return err
	}
	g := parsed.Tools.Repos[*repo]
	return json.NewEncoder(out).Encode(map[string]any{"source_id": *source, "repo": *repo, "account": g.Account, "managed": g.Managed, "allow": g.Allow,
		"push_branches": g.PushBranches, "protected_branches": g.ProtectedBranches, "merge_methods": g.MergeMethods, "merge_base_branches": g.MergeBaseBranches, "require_checks_green": g.RequireChecksGreen})
}

// githubCheck verifies every configured account without printing tokens.
func githubCheck(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("github-check")
	source := f.String("source-id", "", "")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*source) {
		return errors.New("github-check requires --source-id")
	}
	var ws string
	var data []byte
	if err := pool.QueryRow(ctx, `SELECT workspace_id::text, config FROM integrations WHERE id=$1 AND platform='clickup'`, *source).Scan(&ws, &data); err != nil {
		return errors.New("ClickUp source not found")
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return errors.New("invalid source configuration")
	}
	agent, err := tenant.ParseAgentConfig(cfg, knownTools())
	if err != nil {
		return err
	}
	resolver, _, err := openResolver(pool, lookup)
	if err != nil {
		return err
	}
	creds := githubapi.NewCredentialsAt(githubAPIBase, githubHTTP(), resolver, nil, nil)
	names := make([]string, 0, len(agent.Tools.Accounts))
	for n := range agent.Tools.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	enc := json.NewEncoder(out)
	for _, n := range names {
		acc := agent.Tools.Accounts[n]
		repo := firstRepo(agent.Tools, n)
		row := map[string]any{"account": n, "type": acc.Type, "ok": false}
		if acc.Type == "app" && repo == "" {
			row["code"] = "no_repo_grant"
		} else if _, err := creds.Token(ctx, ws, acc, repo, map[string]string{"metadata": "read"}); err != nil {
			row["code"] = safeCode(err)
		} else {
			row["ok"] = true
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func firstRepo(p tenant.ToolPolicyConfig, account string) string {
	var repos []string
	for name, g := range p.Repos {
		if g.Account == account {
			repos = append(repos, name)
		}
	}
	sort.Strings(repos)
	if len(repos) == 0 {
		return ""
	}
	return repos[0]
}

func safeCode(err error) string {
	var ge *githubapi.GitHubError
	if errors.As(err, &ge) {
		return ge.Code
	}
	var se *secrets.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return "check_failed"
}
