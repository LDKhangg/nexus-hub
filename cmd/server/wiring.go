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
	compressor := memory.NewCompressor(store, func(ctx context.Context, ws string) (domain.LLMProvider, error) {
		c, err := client(ctx, ws)
		if err != nil {
			return nil, err
		}
		return c.Default(), nil
	}, s.SummaryModel, agent.CleanInput)
	limits := tools.DefaultLimits()
	limits.ReadTimeout, limits.NetworkTimeout = s.ToolTimeout, s.GitNetworkTimeout
	orch := agent.NewOrchestrator(store, models, reg, runs, redactor, compressor, agent.OrchestratorSettings{
		PollInterval: s.AgentPollInterval, Limits: limits, AllowDangerous: s.AllowDangerous,
		MaxInputTokens: s.MaxInputTokens, MaxOutputTokens: s.MaxOutputTokens,
	})
	worker := relay.NewActionWorker(actions, sender, relay.WorkerSettings{PollInterval: s.NoticePollInterval, RequestTimeout: s.NoticeRequestTimeout})
	return agentServices{orchestrator: orch, actions: worker, known: known}, nil
}
