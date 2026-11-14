package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// MaxAgentAttempts bounds how often one job is tried before it falls back
// to a plain notice.
const MaxAgentAttempts = 5

var agentBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// ErrStaleAgentClaim means another worker owns the job now (fencing).
var ErrStaleAgentClaim = errors.New("stale agent claim")

// AgentJob is a claimed job. Usage is what earlier attempts already spent.
type AgentJob struct {
	ID                                         int64
	WorkspaceID, ThreadID, SourceIntegrationID string
	ClaimToken                                 string
	Attempts                                   int
	Usage                                      domain.Usage
}

// JobContext is everything the orchestrator reads for one job.
type JobContext struct {
	Title, TaskURL, Summary string
	History                 []memory.Message // ordered by ID
	LastTriagedID           int64
	Config                  tenant.AgentConfig
	// MoreUntriaged means the untriaged batch was capped: History holds only
	// the oldest untriaged external messages, and the commit enqueues a
	// follow-up job for the rest.
	MoreUntriaged bool
}

// JobResult is committed atomically with the job's completion. Text is the
// outbound Telegram message; empty means nothing is sent.
type JobResult struct {
	Outcome               Outcome
	RouterNote, ActorNote string
	Text                  string
	ThroughMessageID      int64
}

// AgentStore is the durable job queue (storage.AgentStore). Every write is
// fenced by the claim token and returns ErrStaleAgentClaim on mismatch.
type AgentStore interface {
	Claim(ctx context.Context, now time.Time) (*AgentJob, error)
	Load(ctx context.Context, job AgentJob) (JobContext, error)
	AddUsage(ctx context.Context, job AgentJob, u domain.Usage) error
	CommitOutcome(ctx context.Context, job AgentJob, r JobResult) error
	CommitFallback(ctx context.Context, job AgentJob, code, text string, through int64) error
	Retry(ctx context.Context, job AgentJob, at time.Time, code string) error
}

// Triager is the Tier-1 router.
type Triager interface {
	Triage(ctx context.Context, packet memory.ContextPacket, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error)
}

// Investigator is the Tier-2 actor.
type Investigator interface {
	Investigate(ctx context.Context, packet memory.ContextPacket, d TriageDecision, persona string, specs []tools.ToolSpec, exec *tools.Executor, budget TokenBudget) (ActorReport, error)
}

// ModelSource builds the models for a workspace (its LLM credential lives
// in that workspace's credentials).
type ModelSource func(ctx context.Context, workspaceID string) (Triager, Investigator, error)

// Compressor maintains the rolling thread summary after a job succeeds.
type Compressor interface {
	Compress(ctx context.Context, workspaceID, threadID string) (bool, domain.Usage, error)
}

// OrchestratorSettings come from the DB settings.
type OrchestratorSettings struct {
	PollInterval                    time.Duration
	Limits                          tools.Limits
	AllowDangerous                  bool
	MaxInputTokens, MaxOutputTokens int64
}

// Orchestrator runs claimed agent jobs to an outcome.
type Orchestrator struct {
	store      AgentStore
	models     ModelSource
	reg        *tools.Registry
	runs       tools.ToolRunStore
	redactor   *Redactor
	compressor Compressor
	settings   OrchestratorSettings
	now        func() time.Time
}

func NewOrchestrator(store AgentStore, models ModelSource, reg *tools.Registry, runs tools.ToolRunStore, redactor *Redactor, compressor Compressor, s OrchestratorSettings) *Orchestrator {
	if s.PollInterval <= 0 {
		s.PollInterval = time.Second
	}
	if redactor == nil {
		redactor = NewRedactor(nil)
	}
	return &Orchestrator{store: store, models: models, reg: reg, runs: runs, redactor: redactor, compressor: compressor, settings: s, now: time.Now}
}

// Run polls for due jobs until ctx is done.
func (o *Orchestrator) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := o.processNext(ctx); err != nil && ctx.Err() == nil {
			log.Print("agent worker: storage operation failed; job retained for lease recovery")
		}
		t := time.NewTimer(o.settings.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	return nil
}

// stale drops a job another worker now owns.
func stale(err error, job AgentJob) error {
	if errors.Is(err, ErrStaleAgentClaim) {
		log.Printf("agent worker: job %d claim is stale; dropped", job.ID)
		return nil
	}
	return err
}

func (o *Orchestrator) processNext(ctx context.Context) error {
	job, err := o.store.Claim(ctx, o.now())
	if err != nil || job == nil {
		return err
	}
	jc, err := o.store.Load(ctx, *job)
	if err != nil {
		return stale(err, *job)
	}
	through := batchThrough(jc)
	if job.Attempts > MaxAgentAttempts {
		return o.fallback(ctx, *job, jc, "attempts_exhausted", through)
	}
	if !jc.Config.Enabled {
		return o.fallback(ctx, *job, jc, "agent_disabled", through)
	}
	packet := memory.BuildPacket(jc.Title, jc.TaskURL, jc.Summary, jc.History, jc.LastTriagedID, o.cleanInput)
	if len(packet.NewBatch) == 0 {
		return stale(o.store.CommitOutcome(ctx, *job, JobResult{Outcome: OutcomeIgnore, ThroughMessageID: through}), *job)
	}
	err = o.run(ctx, job, jc, packet, through)
	if err == nil {
		o.compress(ctx, *job)
		return nil
	}
	if errors.Is(err, ErrStaleAgentClaim) || ctx.Err() != nil {
		return stale(err, *job)
	}
	var te *TriageError
	if !errors.As(err, &te) {
		return err // storage failure: keep the lease for recovery
	}
	if !te.Retryable || job.Attempts >= MaxAgentAttempts {
		return o.fallback(ctx, *job, jc, te.Code, through)
	}
	delay := agentBackoff[min(job.Attempts-1, len(agentBackoff)-1)]
	return stale(o.store.Retry(ctx, *job, o.now().Add(delay), te.Code), *job)
}

func (o *Orchestrator) run(ctx context.Context, job *AgentJob, jc JobContext, packet memory.ContextPacket, through int64) error {
	if o.models == nil {
		return &TriageError{Code: CodeLLMUnconfigured}
	}
	triager, investigator, err := o.models(ctx, job.WorkspaceID)
	if err != nil || triager == nil {
		return &TriageError{Code: CodeLLMUnconfigured}
	}
	cfg := jc.Config
	exec := tools.NewExecutor(o.reg, cfg.Tools, o.settings.Limits, o.settings.AllowDangerous, o.runs, o.redactor, job.ID, job.WorkspaceID)
	exec.ClickUpTokenRef = cfg.ClickUpTokenRef
	specs := exec.Specs()
	toolsAvailable := len(specs) > 0 && investigator != nil

	persona := o.cleanInput(cfg.Persona)
	total := job.Usage
	if o.budgetExhausted(total) {
		return &TriageError{Code: CodeTokenBudget}
	}
	d, u, err := triager.Triage(ctx, packet, persona, toolsAvailable)
	if uerr := o.addUsage(ctx, *job, &total, u); uerr != nil {
		return uerr
	}
	if err != nil {
		return err
	}
	threshold := cfg.ConfidenceThreshold
	if threshold <= 0 {
		threshold = tenant.DefaultConfidenceThreshold
	}
	outcome := Route(d, RoutingPolicy{Threshold: threshold, ToolsAvailable: toolsAvailable})
	res := JobResult{Outcome: outcome, ThroughMessageID: through, RouterNote: o.routerNote(d, outcome)}
	switch outcome {
	case OutcomeNotify:
		res.Text = o.notifyText(jc, d)
	case OutcomeInvestigate:
		budget := TokenBudget{MaxInput: o.settings.MaxInputTokens, MaxOutput: o.settings.MaxOutputTokens, Used: total}
		if budget.exhausted() {
			return &TriageError{Code: CodeTokenBudget}
		}
		d.Goal = o.cleanInput(d.Goal)
		report, err := investigator.Investigate(ctx, packet, d, persona, specs, exec, budget)
		if uerr := o.addUsage(ctx, *job, &total, report.Usage); uerr != nil {
			return uerr
		}
		if err != nil {
			return err
		}
		res.ActorNote = o.clean(report.Text)
		res.Text = o.investigateText(jc, d, report.Text, toolsLine(report.Calls), total.Input+total.CacheRead+total.CacheWrite, total.Output)
	}
	res.Text = o.withUnseenNote(jc, res.Text, unseenComments(jc, packet))
	return o.store.CommitOutcome(ctx, *job, res)
}

// routerNote is the stored Tier-1 note of a routed decision.
func (o *Orchestrator) routerNote(d TriageDecision, outcome Outcome) string {
	return o.clean(fmt.Sprintf("%s · %s · %.2f → %s: %s", d.Category, urgency(d), d.Confidence, outcome, d.Summary))
}

// notifyText is the Telegram report of a Notify outcome.
func (o *Orchestrator) notifyText(jc JobContext, d TriageDecision) string {
	return o.clean(fmt.Sprintf("🧠 Nexus · %s\n%s · %s\n%s", jc.Title, urgency(d), d.Summary, jc.TaskURL))
}

// investigateText is the Telegram report of an Investigate outcome. tokensIn
// counts input, cache-read and cache-write tokens of the whole job.
func (o *Orchestrator) investigateText(jc JobContext, d TriageDecision, reportText, tools string, tokensIn, tokensOut int64) string {
	return o.clean(fmt.Sprintf("🔎 Nexus · %s\n%s · %s\n\n%s\n\nTools: %s\nTokens: %d/%d\n%s",
		jc.Title, urgency(d), d.Summary, reportText, tools, tokensIn, tokensOut, jc.TaskURL))
}

// withUnseenNote adds the note about n batch comments the AI did not see.
// An Ignore (empty text) must not silently cover them.
func (o *Orchestrator) withUnseenNote(jc JobContext, text string, n int) string {
	if n <= 0 {
		return text
	}
	note := fmt.Sprintf("(+%d earlier comment(s) in this batch were not shown to the AI; see the task)", n)
	if text == "" {
		return o.clean(fmt.Sprintf("🧠 Nexus · %s\nAI ignored the comments it saw.\n%s\n%s", jc.Title, note, jc.TaskURL))
	}
	return o.clean(text + "\n" + note)
}

// unseenComments counts untriaged external comments that the packet's batch
// budget left out, so the report never hides them.
func unseenComments(jc JobContext, packet memory.ContextPacket) int {
	n := 0
	for _, m := range jc.History {
		if m.ID > jc.LastTriagedID && m.SenderRole == memory.SenderRoleExternalUser {
			n++
		}
	}
	return max(n-len(packet.NewBatch), 0)
}

func (o *Orchestrator) addUsage(ctx context.Context, job AgentJob, total *domain.Usage, u domain.Usage) error {
	if u == (domain.Usage{}) {
		return nil
	}
	total.Add(u)
	return o.store.AddUsage(ctx, job, u)
}

// CompressTimeout bounds one rolling-summary update after a job.
const CompressTimeout = time.Minute

func (o *Orchestrator) compress(ctx context.Context, job AgentJob) {
	if o.compressor == nil {
		return
	}
	// The job is already committed; bound the summary call so a hung
	// provider cannot stall the worker.
	cctx, cancel := context.WithTimeout(ctx, CompressTimeout)
	defer cancel()
	if _, _, err := o.compressor.Compress(cctx, job.WorkspaceID, job.ThreadID); err != nil && ctx.Err() == nil {
		log.Printf("agent worker: summary for job %d not updated", job.ID)
	}
}

func (o *Orchestrator) clean(s string) string { return o.redactor.CleanOutbound(s, false) }

// cleanInput prepares untrusted or configured text for an LLM: control
// characters are stripped and known secrets and secret patterns redacted.
func (o *Orchestrator) cleanInput(s string) string { return o.redactor.RedactSecrets(CleanInput(s)) }

// budgetExhausted reports whether the job's stored usage already reached
// the per-job token budget, counted as TokenBudget counts it.
func (o *Orchestrator) budgetExhausted(used domain.Usage) bool {
	return TokenBudget{MaxInput: o.settings.MaxInputTokens, MaxOutput: o.settings.MaxOutputTokens, Used: used}.exhausted()
}

func (o *Orchestrator) fallback(ctx context.Context, job AgentJob, jc JobContext, code string, through int64) error {
	log.Printf("agent worker: job %d falls back to a notice (%s)", job.ID, code)
	return stale(o.store.CommitFallback(ctx, job, code, o.fallbackText(jc, code), through), job)
}

// fallbackText is the plain notice of the unhandled batch.
func (o *Orchestrator) fallbackText(jc JobContext, code string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚠️ AI triage unavailable (%s)\nClickUp · Comment\n%s\n", code, jc.Title)
	for _, m := range jc.History {
		if m.ID <= jc.LastTriagedID || m.SenderRole != memory.SenderRoleExternalUser {
			continue
		}
		name := m.SenderName
		if name == "" {
			name = "Unknown user"
		}
		fmt.Fprintf(&sb, "\nBy %s:\n%s\n", name, m.Content)
	}
	sb.WriteString("\n" + jc.TaskURL)
	return o.clean(sb.String())
}

// batchThrough is the newest external_user message of the batch.
func batchThrough(jc JobContext) int64 {
	through := jc.LastTriagedID
	for _, m := range jc.History {
		if m.SenderRole == memory.SenderRoleExternalUser && m.ID > through {
			through = m.ID
		}
	}
	return through
}

func urgency(d TriageDecision) Urgency {
	if d.Urgency == "" {
		return UrgencyNormal
	}
	return d.Urgency
}

const maxToolsListed = 20

// toolsLine renders "name ✓|✗|⛔|🧪" with the tier for non-read calls.
func toolsLine(calls []CallRecord) string {
	if len(calls) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(calls))
	for i, c := range calls {
		if i == maxToolsListed {
			parts = append(parts, fmt.Sprintf("+%d more", len(calls)-maxToolsListed))
			break
		}
		mark := "✓"
		switch {
		case c.Status == tools.StatusDenied:
			mark = "⛔"
		case c.Status == tools.StatusDryRun:
			mark = "🧪"
		case c.IsError:
			mark = "✗"
		}
		p := c.Name + " " + mark
		switch c.Tier {
		case tenant.TierWrite:
			p += " (write)"
		case tenant.TierDangerous:
			p += " (DANGEROUS)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}
