package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

type agentServices struct {
	orchestrator *agent.Orchestrator
	actions      *relay.ActionWorker
	known        map[string]tenant.Tier
	reg          *tools.Registry
	limits       tools.Limits
	// provider returns the workspace's default LLM provider.
	provider func(ctx context.Context, ws string) (domain.LLMProvider, error)
}

// ensurePrivateDirs creates the managed-clone and git tmp directories 0700.
func ensurePrivateDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return errors.New("cannot create agent data directory")
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return errors.New("cannot secure agent data directory")
		}
	}
	return nil
}

// resolveGit returns the configured git path, or git from PATH.
func resolveGit(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// buildTools assembles every agent tool. Code tools are skipped (with a
// warning) when git is unavailable.
func buildTools(s settings.Settings, resolver *secrets.Resolver, redactor *agent.Redactor) (*tools.Registry, error) {
	apiClient := netguard.Client(s.ToolTimeout, netguard.DefaultHosts)
	creds := githubapi.NewCredentials(apiClient, resolver, redactor, nil)
	gh := githubapi.NewClient(apiClient, creds)
	all := append([]tools.Tool{}, githubtools.NewReadTools(gh)...)
	all = append(all, githubtools.NewWriteTools(gh, redactor)...)
	all = append(all, githubtools.NewDangerousTools(gh, redactor)...)
	all = append(all, clickuptools.NewTaskTool(apiClient, resolver))
	if gitPath, err := resolveGit(s.GitPath); err != nil {
		log.Print("git not found; code and workspace tools disabled")
	} else if runner, err := execrunner.New(gitPath, s.TmpDir); err != nil {
		log.Printf("git runner unavailable (%v); code and workspace tools disabled", err)
	} else {
		all = append(all, codetools.NewReadTools(runner, s.ReposDir)...)
		all = append(all, codetools.NewWorkspaceTools(runner, creds, s.ReposDir, s.MaxCloneBytes, redactor)...)
	}
	return tools.NewRegistry(all...)
}

func buildAgent(pool *pgxpool.Pool, s settings.Settings, resolver *secrets.Resolver, redactor *agent.Redactor, sender *telegram.Client) (agentServices, error) {
	reg, err := buildTools(s, resolver, redactor)
	if err != nil {
		return agentServices{}, errors.New("tool registry startup failed")
	}
	store, err := storage.NewAgentStore(pool)
	if err != nil {
		return agentServices{}, errors.New("agent store startup failed")
	}
	known := reg.Tiers()
	store.SetKnownTools(known)
	runs, err := storage.NewToolRunStore(pool)
	if err != nil {
		return agentServices{}, errors.New("tool run store startup failed")
	}
	actions, err := storage.NewActionStore(pool)
	if err != nil {
		return agentServices{}, errors.New("action store startup failed")
	}
	llmHTTP := netguard.Client(0, netguard.DefaultHosts) // the SDK applies its own request timeouts
	client := func(ctx context.Context, ws string) (*llm.Client, error) {
		return llm.NewClientFromSecret(ctx, resolver, ws, s, llmHTTP)
	}
	models := func(ctx context.Context, ws string) (agent.Triager, agent.Investigator, error) {
		c, err := client(ctx, ws)
		if err != nil {
			return nil, nil, err
		}
		var inv agent.Investigator
		if tc := c.ToolCalling(); tc != nil {
			inv = agent.NewTier2Actor(tc, s.ActorModel)
		}
		return agent.NewTier1Router(c.Default(), s.TriageModel), inv, nil
	}
	provider := func(ctx context.Context, ws string) (domain.LLMProvider, error) {
		c, err := client(ctx, ws)
		if err != nil {
			return nil, err
		}
		return c.Default(), nil
	}
	compressor := memory.NewCompressor(store, provider, s.SummaryModel, func(v string) string { return redactor.RedactSecrets(agent.CleanInput(v)) })
	limits := tools.DefaultLimits()
	limits.ReadTimeout, limits.NetworkTimeout = s.ToolTimeout, s.GitNetworkTimeout
	orch := agent.NewOrchestrator(store, models, reg, runs, redactor, compressor, agent.OrchestratorSettings{
		PollInterval: s.AgentPollInterval, Limits: limits, AllowDangerous: s.AllowDangerous,
		MaxInputTokens: s.MaxInputTokens, MaxOutputTokens: s.MaxOutputTokens,
	})
	worker := relay.NewActionWorker(actions, sender, relay.WorkerSettings{PollInterval: s.NoticePollInterval, RequestTimeout: s.NoticeRequestTimeout})
	return agentServices{orchestrator: orch, actions: worker, known: known, reg: reg, limits: limits, provider: provider}, nil
}

// buildRunWorker wires the behavior run executor: the shared tool registry
// and LLM client, a tool-run audit scoped to behavior runs, and the
// workspace's ClickUp source agent config as the run's grants. The action
// library (dispatcher) and the Telegram failure notice come later; until
// then items stay drafted and failures are logged by code.
func buildRunWorker(pool *pgxpool.Pool, s settings.Settings, store *storage.BehaviorStore, integrations *storage.IntegrationRepository, ag agentServices, redactor *agent.Redactor) (*behavior.RunWorker, error) {
	audit, err := storage.NewBehaviorToolRunStore(pool)
	if err != nil {
		return nil, errors.New("behavior tool run store startup failed")
	}
	return behavior.NewRunWorker(behavior.RunWorkerDeps{
		Runs: store, Defs: store, Profiles: store,
		Policies: behaviorPolicies{integrations: integrations, known: ag.known},
		Models:   ag.provider,
		Callers:  behavior.ExecutorFactory(ag.reg, audit, redactor, ag.limits),
		Redactor: redactor,
	}, behavior.RunWorkerSettings{
		Workers: int(s.BehaviorWorkers), PollInterval: s.BehaviorPollInterval,
		DefaultModel: s.ActorModel, DefaultMaxTokens: int(s.BehaviorMaxTokens), MaxOutputTokens: int(s.MaxOutputTokens),
	}), nil
}

// behaviorPolicies finds a run's grants: the agent config of the ClickUp
// source the trigger came through, or, for a trigger from another
// integration (a Telegram command), the workspace's first enabled ClickUp
// source.
type behaviorPolicies struct {
	integrations *storage.IntegrationRepository
	known        map[string]tenant.Tier
}

func clickUpSource(in tenant.Integration) bool {
	return in.IsEnabled && in.Platform == "clickup" && (in.Role == tenant.IntegrationRoleSource || in.Role == tenant.IntegrationRoleBoth)
}

func (p behaviorPolicies) RunPolicy(ctx context.Context, workspaceID, integrationID string) (tenant.AgentConfig, error) {
	if integrationID != "" {
		in, err := p.integrations.Get(ctx, workspaceID, integrationID)
		if err == nil && clickUpSource(in) {
			return tenant.ParseAgentConfig(in.Config, p.known)
		}
	}
	list, err := p.integrations.List(ctx, workspaceID, 100, 0)
	if err != nil {
		return tenant.AgentConfig{}, err
	}
	for _, in := range list {
		if clickUpSource(in) {
			return tenant.ParseAgentConfig(in.Config, p.known)
		}
	}
	return tenant.AgentConfig{}, errors.New("no ClickUp source in workspace")
}
