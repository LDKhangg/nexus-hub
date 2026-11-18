package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Bounds of the investigate executor.
const (
	// investigateSummaryBytes bounds the stored summary (the orchestrator
	// stores at most 500 bytes of result text).
	investigateSummaryBytes = 500
	// investigateRequestRunes and investigateFieldRunes bound the untrusted
	// text handed to the model.
	investigateRequestRunes = 4000
	investigateFieldRunes   = 500
	// investigateMaxEvidence bounds the tool resource refs of one task.
	investigateMaxEvidence = 8
	// investigateKeyScope namespaces the executor's idempotency keys.
	investigateKeyScope = "orchestrator"
)

// investigateGoal is the trusted goal of every investigate task; the task
// itself is only in the <ticket_content> data block.
const investigateGoal = "Investigate the plan task described in <ticket_content> (its title, acceptance hint and the owner's request) " +
	"using only the read tools. Report what you found, citing the ClickUp task IDs and GitHub URLs you read."

var clickUpTaskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// OrchestratorInvestigate is the orchestrator's investigate executor: it
// runs the read-only Tier-2 loop for one plan task. The tool policy is the
// workspace's ClickUp source agent configuration (as for the comment
// pipeline) reduced to read tools: no write, dangerous or action tool is
// reachable. Tool calls are audited in memory for the evidence refs and,
// when Audit is set, durably in tool_runs (owned by the workflow
// execution; sanitized summaries only). Its token usage is returned for the
// orchestrator budget.
type OrchestratorInvestigate struct {
	// Policies returns the workspace's agent configuration (grants and
	// ClickUp credential reference).
	Policies PolicySource
	// Investigators returns the workspace's Tier-2 investigator; nil when
	// the provider has no tool calling.
	Investigators func(ctx context.Context, workspaceID string) (agent.Investigator, error)
	Registry      *tools.Registry
	Sanitizer     tools.Sanitizer
	Limits        tools.Limits
	// Audit is the durable tool_runs store (workflow execution owner); nil
	// keeps the audit in memory only.
	Audit tools.ToolRunStore
	// MaxInputTokens and MaxOutputTokens cap one task further (<= 0: only
	// the orchestrator budget).
	MaxInputTokens, MaxOutputTokens int64
}

var _ orchestrator.TaskExecutor = (*OrchestratorInvestigate)(nil)

// RunTask runs one investigate task. A missing configuration, model or
// read tool fails the task with a reason code (no model call); a context
// cancellation is returned as an error.
func (e *OrchestratorInvestigate) RunTask(ctx context.Context, run orchestrator.TaskRun) (orchestrator.TaskOutcome, error) {
	if e == nil || e.Policies == nil || e.Investigators == nil || e.Registry == nil || e.Sanitizer == nil {
		return orchestrator.TaskOutcome{ReasonCode: orchestrator.ReasonExecutorUnavailable}, nil
	}
	if run.TokensLeft <= 0 {
		return orchestrator.TaskOutcome{ReasonCode: agent.CodeTokenBudget}, nil
	}
	cfg, err := e.Policies.RunPolicy(ctx, run.WorkspaceID, run.Route.IntegrationID)
	if err != nil {
		if ctx.Err() != nil {
			return orchestrator.TaskOutcome{}, ctx.Err()
		}
		return orchestrator.TaskOutcome{ReasonCode: orchestrator.ReasonExecutorUnavailable}, nil
	}
	inv, err := e.Investigators(ctx, run.WorkspaceID)
	if err != nil || inv == nil {
		if ctx.Err() != nil {
			return orchestrator.TaskOutcome{}, ctx.Err()
		}
		return orchestrator.TaskOutcome{ReasonCode: agent.CodeLLMUnconfigured}, nil
	}
	limits := e.Limits
	limits.MaxDangerous = 0
	audit := &investigateAudit{store: e.Audit}
	// Read tools only: no policy gate, so a non-read call would be denied.
	exec := tools.NewExecutor(e.Registry, readOnlyPolicy(cfg.Tools, e.Registry.Tiers()), limits, audit, e.Sanitizer, run.ExecutionID, run.WorkspaceID, tools.WithExecutionID(run.ExecutionID))
	exec.ClickUpTokenRef = cfg.ClickUpTokenRef
	exec.KeyScope = investigateKeyScope
	specs := exec.Specs()
	if len(specs) == 0 {
		return orchestrator.TaskOutcome{ReasonCode: "no_read_tools"}, nil
	}
	// MaxTotal keeps input+output+cache of this task within the
	// orchestrator's remaining tokens; the per-side caps alone could spend
	// more than TokensLeft in sum.
	budget := agent.TokenBudget{
		MaxInput:  capTokens(run.TokensLeft, e.MaxInputTokens),
		MaxOutput: capTokens(run.TokensLeft, e.MaxOutputTokens),
		MaxTotal:  run.TokensLeft,
	}
	report, err := inv.Investigate(ctx, e.packet(run), agent.TriageDecision{Goal: investigateGoal}, agent.CleanInput(cfg.Persona), specs, exec, budget)
	out := orchestrator.TaskOutcome{Usage: orchestrator.Usage{Tokens: report.Usage, Attempts: report.Steps}}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.ReasonCode = "investigate_failed"
		var te *agent.TriageError
		if errors.As(err, &te) && te.Code != "" {
			out.ReasonCode = te.Code
		}
		return out, nil
	}
	out.Summary = boundBytes(strings.TrimSpace(e.Sanitizer.CleanOutbound(report.Text, false)), investigateSummaryBytes)
	out.Evidence = audit.evidence(run.Route.ClickUpTaskID)
	switch {
	case report.Stopped != "":
		out.ReasonCode = "investigate_" + report.Stopped
	case out.Summary == "":
		out.ReasonCode = "investigate_empty"
	case len(out.Evidence) == 0:
		// Success needs at least one ref from a tool read; the summary
		// alone is not evidence.
		out.ReasonCode = "investigate_no_evidence"
	default:
		out.Succeeded = true
	}
	return out, nil
}

// packet puts every untrusted part (task title, acceptance hint, request,
// ClickUp task reference) in the data block of the prompt.
func (e *OrchestratorInvestigate) packet(run orchestrator.TaskRun) memory.ContextPacket {
	clean := func(s string, n int) string {
		return truncateRunes(strings.TrimSpace(agent.CleanInput(s)), n)
	}
	var b strings.Builder
	if h := clean(run.Task.AcceptanceHint, investigateFieldRunes); h != "" {
		b.WriteString("Acceptance hint: " + h + "\n")
	}
	if id := clean(run.Route.ClickUpTaskID, investigateFieldRunes); id != "" {
		b.WriteString("ClickUp task: " + id + "\n")
	}
	if r := clean(run.Request, investigateRequestRunes); r != "" {
		b.WriteString("Request: " + r)
	}
	return memory.ContextPacket{Title: clean(run.Task.Title, investigateFieldRunes), Summary: strings.TrimSpace(b.String())}
}

// readOnlyPolicy keeps only the read tools of p: enabled read-tier tools,
// repo grants without any allowed (write) tool, no write budget. Accounts
// and grants stay, so repo-scoped reads keep their grant checks.
func readOnlyPolicy(p tenant.ToolPolicyConfig, tiers map[string]tenant.Tier) tenant.ToolPolicyConfig {
	out := tenant.ToolPolicyConfig{DryRun: p.DryRun, Accounts: maps.Clone(p.Accounts)}
	for _, n := range p.Enabled {
		if tiers[n] == tenant.TierRead && !slices.Contains(out.Enabled, n) {
			out.Enabled = append(out.Enabled, n)
		}
	}
	if len(p.Repos) > 0 {
		out.Repos = make(map[string]tenant.RepoGrant, len(p.Repos))
		for k, g := range p.Repos {
			out.Repos[k] = tenant.RepoGrant{Repo: g.Repo, Account: g.Account, LocalPath: g.LocalPath, Managed: g.Managed}
		}
	}
	return out
}

func capTokens(left, limit int64) int64 {
	if limit > 0 && limit < left {
		return limit
	}
	return left
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func boundBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// investigateAudit is the in-memory audit of one investigate task. It
// accepts read-tier runs only (a non-read run is refused, so the executor
// denies it as audit_unavailable) and keeps the resources of successful
// calls for the evidence refs. It never stores tool inputs or outputs
// beyond the validated repo and ClickUp task ID.
type investigateAudit struct {
	store tools.ToolRunStore // optional durable audit
	mu    sync.Mutex
	runs  []investigateRun
}

type investigateRun struct {
	tool, repo, taskID string
	ok                 bool
	durableID          int64
}

var errReadOnlyAudit = errors.New("investigate: only read tools may run")

func (a *investigateAudit) Begin(ctx context.Context, r tools.RunStart) (int64, *tools.PriorRun, error) {
	if r.Tier != tenant.TierRead {
		return 0, nil, errReadOnlyAudit
	}
	run := investigateRun{tool: r.Tool, repo: r.Repo}
	if strings.HasPrefix(r.Tool, "clickup_") {
		// InputRedacted is the redacted input as a JSON string.
		var raw string
		var in struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(r.InputRedacted, &raw) == nil && json.Unmarshal([]byte(raw), &in) == nil && clickUpTaskIDRe.MatchString(in.TaskID) {
			run.taskID = in.TaskID
		}
	}
	if a.store != nil {
		id, _, err := a.store.Begin(ctx, r)
		if err != nil {
			return 0, nil, err
		}
		run.durableID = id
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runs = append(a.runs, run)
	return int64(len(a.runs)), nil, nil
}

func (a *investigateAudit) Finish(ctx context.Context, f tools.RunFinish) error {
	var durable int64
	a.mu.Lock()
	if f.RunID >= 1 && int(f.RunID) <= len(a.runs) {
		a.runs[f.RunID-1].ok = f.Status == tools.StatusOK
		durable = a.runs[f.RunID-1].durableID
	}
	a.mu.Unlock()
	if a.store != nil && durable != 0 {
		f.RunID = durable
		return a.store.Finish(ctx, f)
	}
	return nil
}

func (a *investigateAudit) Deny(ctx context.Context, r tools.RunStart, code string) error {
	if a.store != nil {
		return a.store.Deny(ctx, r, code)
	}
	return nil
}

// evidence returns the resources successful calls read: GitHub repo URLs
// and ClickUp task IDs (the route's task when a ClickUp read named none).
func (a *investigateAudit) evidence(routeTaskID string) []orchestrator.EvidenceRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []orchestrator.EvidenceRef
	add := func(kind, id string) {
		for _, e := range out {
			if e.ID == id {
				return
			}
		}
		if len(out) < investigateMaxEvidence {
			out = append(out, orchestrator.EvidenceRef{Kind: kind, ID: id})
		}
	}
	for _, r := range a.runs {
		if !r.ok {
			continue
		}
		if r.repo != "" && tenant.ValidRepo(r.repo) {
			add(orchestrator.EvidenceArtifact, "https://github.com/"+r.repo)
		}
		if strings.HasPrefix(r.tool, "clickup_") {
			id := r.taskID
			if id == "" && clickUpTaskIDRe.MatchString(routeTaskID) {
				id = routeTaskID
			}
			if id != "" {
				add(orchestrator.EvidenceStepOutput, "clickup:"+id)
			}
		}
	}
	return out
}
