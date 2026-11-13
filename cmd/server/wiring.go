package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	githubapi "github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/behavior/chatpoll"
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

// buildTools assembles every agent tool plus extra (the engine's own read
// tools, such as memory_search). Code tools are skipped (with a warning)
// when git is unavailable.
func buildTools(s settings.Settings, resolver *secrets.Resolver, redactor *agent.Redactor, extra ...tools.Tool) (*tools.Registry, error) {
	apiClient := netguard.Client(s.ToolTimeout, netguard.DefaultHosts)
	creds := githubapi.NewCredentials(apiClient, resolver, redactor, nil)
	gh := githubapi.NewClient(apiClient, creds)
	all := append([]tools.Tool{}, githubtools.NewReadTools(gh)...)
	all = append(all, githubtools.NewWriteTools(gh, redactor)...)
	all = append(all, githubtools.NewDangerousTools(gh, redactor)...)
	all = append(all, clickuptools.NewTaskTool(apiClient, resolver))
	// ClickUp writes of the behavior action library: tier write, not repo
	// scoped, so agent jobs can never call them (no repo grant allows them).
	all = append(all, clickuptools.NewWriteTools(apiClient, resolver)...)
	// Run-level actions of the behavior engine (approve a pull request as
	// the owner's pat account, react to a ClickUp Chat message). They are
	// ActionOnly: offered and run only by the behavior action executor.
	all = append(all, githubtools.NewApproveTool(gh))
	all = append(all, clickuptools.NewChatReactionTool(apiClient, resolver, clickupchat.New(apiClient)))
	// Undo of a run-level reaction (/undo), also ActionOnly.
	all = append(all, clickuptools.NewChatReactionRemoveTool(apiClient, resolver, clickupchat.New(apiClient)))
	all = append(all, extra...)
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

func buildAgent(pool *pgxpool.Pool, s settings.Settings, resolver *secrets.Resolver, redactor *agent.Redactor, sender *telegram.Client, extra ...tools.Tool) (agentServices, error) {
	reg, err := buildTools(s, resolver, redactor, extra...)
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
	// The provider comes from llm_provider: the Anthropic API (key from the
	// workspace credential) or a CLI signed in on the host. Only the
	// provider name is logged, never a credential or a path.
	factory, err := llm.NewFactory(s, resolver, llmHTTP, s.TmpDir)
	if err != nil {
		return agentServices{}, errors.New("llm provider startup failed")
	}
	log.Printf("llm provider: %s", factory.Provider())
	if !factory.Ready() {
		log.Printf("llm provider %s: binary not usable; LLM work falls back to notices (llm_unconfigured)", factory.Provider())
	}
	client := factory.Client
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

// buildBehaviorEngine wires the behavior run executor and the action
// library: the shared tool registry and LLM client, one tool-run audit
// scoped to behavior runs (reads and action writes), the workspace's
// ClickUp source agent config as the run's grants and credential, and the
// owner's Telegram chat, decision memory and the digest queue. The action
// engine is the run worker's dispatcher and failure notifier, the inbox's
// callback handler and undoer, and a service worker of its own (the
// approval expiry sweep and digest batches).
func buildBehaviorEngine(pool *pgxpool.Pool, s settings.Settings, store *storage.BehaviorStore, integrations *storage.IntegrationRepository, ag agentServices, redactor *agent.Redactor, resolver *secrets.Resolver, messenger behavior.Messenger) (*behavior.RunWorker, *behavior.Engine, error) {
	audit, err := storage.NewBehaviorToolRunStore(pool)
	if err != nil {
		return nil, nil, errors.New("behavior tool run store startup failed")
	}
	policies := behaviorPolicies{integrations: integrations, known: ag.known}
	engine, err := behavior.NewEngine(behavior.ActionDeps{
		Store: store, Defs: store, Profiles: store, Policies: policies,
		Tasks:     taskReader{r: clickuptools.NewTaskReader(netguard.Client(s.ToolTimeout, netguard.DefaultHosts), resolver)},
		Chats:     ownerChats{integrations: integrations, ownerChatID: s.OwnerTelegramChatID},
		Messenger: messenger,
		Callers:   behavior.ActionExecutorFactory(ag.reg, audit, redactor, ag.limits),
		Decisions: behavior.MemoryRecorder{Store: store},
		Digests:   store,
		Redactor:  redactor,
	}, behavior.ActionSettings{OwnerClickUpUserID: s.OwnerClickUpUserID, RequestTimeout: s.NoticeRequestTimeout})
	if err != nil {
		return nil, nil, errors.New("behavior action engine startup failed")
	}
	runs := behavior.NewRunWorker(behavior.RunWorkerDeps{
		Runs: store, Defs: store, Profiles: store,
		Policies:   policies,
		Models:     ag.provider,
		Callers:    behavior.ExecutorFactory(ag.reg, audit, redactor, ag.limits),
		Dispatcher: engine,
		Notifier:   engine,
		Redactor:   redactor,
	}, behavior.RunWorkerSettings{
		Workers: int(s.BehaviorWorkers), PollInterval: s.BehaviorPollInterval,
		DefaultModel: s.ActorModel, DefaultMaxTokens: int(s.BehaviorMaxTokens), MaxOutputTokens: int(s.MaxOutputTokens),
	})
	return runs, engine, nil
}

// taskReader adapts the ClickUp exact task reader to the engine.
type taskReader struct{ r *clickuptools.TaskReader }

func (t taskReader) ReadTask(ctx context.Context, workspaceID, tokenRef, taskID string) (behavior.TaskState, error) {
	st, err := t.r.ReadTask(ctx, workspaceID, tokenRef, taskID)
	if err != nil {
		return behavior.TaskState{}, err
	}
	return behavior.TaskState{ID: st.ID, Status: st.Status, AssigneeIDs: st.AssigneeIDs, Description: st.Description, Markdown: st.Markdown}, nil
}

// ownerChats reaches the owner: through the bot and chat a Telegram command
// came from, otherwise through the workspace's first enabled Telegram
// control integration and the owner_telegram_chat_id setting.
type ownerChats struct {
	integrations *storage.IntegrationRepository
	ownerChatID  int64
}

func telegramControl(in tenant.Integration) bool {
	return in.IsEnabled && in.Platform == "telegram" && (in.Role == tenant.IntegrationRoleControl || in.Role == tenant.IntegrationRoleBoth)
}

func (o ownerChats) OwnerChat(ctx context.Context, workspaceID string, t behavior.TriggerData) (behavior.ChatRef, error) {
	if t.TelegramChatID != 0 && t.IntegrationID != "" {
		if in, err := o.integrations.Get(ctx, workspaceID, t.IntegrationID); err == nil && telegramControl(in) {
			return behavior.ChatRef{WorkspaceID: workspaceID, IntegrationID: in.ID, ChatID: t.TelegramChatID}, nil
		}
	}
	if o.ownerChatID == 0 {
		return behavior.ChatRef{}, errors.New("owner_telegram_chat_id is not set")
	}
	list, err := o.integrations.List(ctx, workspaceID, 100, 0)
	if err != nil {
		return behavior.ChatRef{}, err
	}
	for _, in := range list {
		if telegramControl(in) {
			return behavior.ChatRef{WorkspaceID: workspaceID, IntegrationID: in.ID, ChatID: o.ownerChatID}, nil
		}
	}
	return behavior.ChatRef{}, errors.New("no Telegram control integration in workspace")
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

// buildChatPoller wires the ClickUp Chat poller: every enabled ClickUp
// source with a team ID, its agent config's ClickUp token reference, the
// behavior store for enabled versions, the poll state and the chat run
// starter, and the owner's Telegram chat for the unauthorized notice.
func buildChatPoller(s settings.Settings, store *storage.BehaviorStore, workspaces *storage.WorkspaceRepository, integrations *storage.IntegrationRepository, known map[string]tenant.Tier, resolver *secrets.Resolver, messenger behavior.Messenger) *chatpoll.Poller {
	return chatpoll.New(chatpoll.Deps{
		Targets:   chatPollTargets{workspaces: workspaces, integrations: integrations, known: known},
		Versions:  store,
		State:     store,
		Runs:      store,
		Chat:      clickupchat.New(netguard.Client(s.ToolTimeout, netguard.DefaultHosts)),
		Secrets:   resolver,
		Chats:     ownerChats{integrations: integrations, ownerChatID: s.OwnerTelegramChatID},
		Messenger: messenger,
		Profiles:  store,
	}, chatpoll.Settings{Interval: s.ClickUpChatPollInterval, OwnerClickUpUserID: s.OwnerClickUpUserID})
}

// chatPollTargets lists, across all workspaces, the enabled ClickUp
// sources that have a clickup_team_id and a valid agent config; the token
// reference is the agent config's clickup_token_ref (default "clickup"),
// the same one the ClickUp tools resolve.
type chatPollTargets struct {
	workspaces   *storage.WorkspaceRepository
	integrations *storage.IntegrationRepository
	known        map[string]tenant.Tier
}

// chatPollPage is the page size of the workspace and integration listings.
const chatPollPage = 100

func (c chatPollTargets) ChatPollTargets(ctx context.Context) ([]chatpoll.Target, error) {
	var out []chatpoll.Target
	for off := int32(0); ; off += chatPollPage {
		wss, err := c.workspaces.List(ctx, chatPollPage, off)
		if err != nil {
			return nil, err
		}
		for _, ws := range wss {
			targets, err := c.workspaceTargets(ctx, ws.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, targets...)
		}
		if len(wss) < chatPollPage {
			return out, nil
		}
	}
}

func (c chatPollTargets) workspaceTargets(ctx context.Context, workspaceID string) ([]chatpoll.Target, error) {
	var out []chatpoll.Target
	for off := int32(0); ; off += chatPollPage {
		list, err := c.integrations.List(ctx, workspaceID, chatPollPage, off)
		if err != nil {
			return nil, err
		}
		for _, in := range list {
			if !clickUpSource(in) {
				continue
			}
			team, _ := in.Config["clickup_team_id"].(string)
			team = strings.TrimSpace(team)
			if team == "" {
				continue
			}
			cfg, err := tenant.ParseAgentConfig(in.Config, c.known)
			if err != nil {
				log.Printf("clickup chat poll: invalid agent config workspace=%s integration=%s", workspaceID, in.ID)
				continue
			}
			out = append(out, chatpoll.Target{WorkspaceID: workspaceID, IntegrationID: in.ID, TeamID: team, TokenRef: cfg.ClickUpTokenRef})
		}
		if len(list) < chatPollPage {
			return out, nil
		}
	}
}
