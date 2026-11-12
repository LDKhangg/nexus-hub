package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Audit statuses (tool_runs.status).
const (
	StatusStarted        = "started"
	StatusOK             = "ok"
	StatusError          = "error"
	StatusDenied         = "denied"
	StatusTimeout        = "timeout"
	StatusUnknownOutcome = "unknown_outcome"
	StatusDryRun         = "dry_run"
)

// MaxInputBytes caps a tool call's JSON input.
const MaxInputBytes = 16 * 1024

const truncatedSuffix = "\n[truncated]"

// Limits bounds one job's tool use.
type Limits struct {
	MaxCalls       int
	MaxDangerous   int
	ReadTimeout    time.Duration
	NetworkTimeout time.Duration
	OutputCap      int
}

// DefaultLimits returns the design defaults.
func DefaultLimits() Limits {
	return Limits{MaxCalls: 12, MaxDangerous: 1, ReadTimeout: 30 * time.Second, NetworkTimeout: 120 * time.Second, OutputCap: 6144}
}

// RunStart is the audit row written before a call runs (or when denied).
type RunStart struct {
	WorkspaceID   string
	JobID         int64
	Step          int
	Tool          string
	Tier          tenant.Tier
	Repo, Account string
	Key           string
	InputRedacted json.RawMessage
}

// RunFinish completes a started audit row.
type RunFinish struct {
	RunID       int64
	Status      string
	ErrorCode   string
	DurationMS  int64
	OutputBytes int
	Summary     string
}

// PriorRun is an earlier non-read run of the same idempotency key. Status
// is "ok" or "unknown_outcome".
type PriorRun struct{ Status, Summary string }

// ToolRunStore persists the audit trail. For non-read tiers, Begin returns
// a prior run instead of inserting when the key already has an ok or an
// ambiguous (started, timeout, unknown_outcome) row.
type ToolRunStore interface {
	Begin(ctx context.Context, r RunStart) (runID int64, prior *PriorRun, err error)
	Finish(ctx context.Context, f RunFinish) error
	Deny(ctx context.Context, r RunStart, code string) error
}

// CallResult is what the actor feeds back to the LLM.
type CallResult struct {
	Content string
	IsError bool
	Status  string
	Tier    tenant.Tier
}

// Executor enforces the tool policy for one agent job.
type Executor struct {
	reg            *Registry
	policy         tenant.ToolPolicyConfig
	limits         Limits
	allowDangerous bool
	store          ToolRunStore
	san            Sanitizer
	jobID          int64
	workspaceID    string

	// ClickUpTokenRef is passed to tools that call ClickUp.
	ClickUpTokenRef string

	mu        sync.Mutex
	calls     int
	writes    int
	dangerous int
}

// NewExecutor builds the executor for one job attempt.
func NewExecutor(reg *Registry, policy tenant.ToolPolicyConfig, limits Limits, allowDangerous bool, store ToolRunStore, s Sanitizer, jobID int64, workspaceID string) *Executor {
	return &Executor{reg: reg, policy: policy, limits: limits, allowDangerous: allowDangerous, store: store, san: s, jobID: jobID, workspaceID: workspaceID}
}

// Enabled reports whether a tool may be offered to the LLM: read tools in
// the enabled list, and non-read tools allowed for at least one repo.
func (e *Executor) Enabled(name string) bool {
	t := e.reg.Get(name)
	if t == nil {
		return false
	}
	if t.Spec().Tier == tenant.TierRead {
		return slices.Contains(e.policy.Enabled, name)
	}
	for _, g := range e.policy.Repos {
		if slices.Contains(g.Allow, name) {
			return true
		}
	}
	return slices.Contains(e.policy.Enabled, name)
}

// Specs returns the specs of every tool Enabled for this job.
func (e *Executor) Specs() []ToolSpec {
	var names []string
	for n := range e.reg.Tiers() {
		if e.Enabled(n) {
			names = append(names, n)
		}
	}
	return e.reg.Specs(names)
}

// Call runs one tool call through every policy check, in order.
func (e *Executor) Call(ctx context.Context, step int, name string, input json.RawMessage) CallResult {
	start := RunStart{WorkspaceID: e.workspaceID, JobID: e.jobID, Step: step, Tool: name, Tier: tenant.TierRead}
	start.InputRedacted = e.redactInput(input)

	e.mu.Lock()
	e.calls++
	overCalls := e.calls > e.limits.MaxCalls
	e.mu.Unlock()

	// 1. exists
	tool := e.reg.Get(name)
	if tool == nil {
		return e.deny(ctx, start, "unknown_tool")
	}
	spec := tool.Spec()
	start.Tier = spec.Tier
	// 2. enabled
	if !e.Enabled(name) {
		return e.deny(ctx, start, "tool_disabled")
	}
	// 3. input size and shape
	if len(input) > MaxInputBytes {
		return e.deny(ctx, start, "input_too_large")
	}
	if !isJSONObject(input) {
		return e.deny(ctx, start, "invalid_input")
	}
	// 4. repo configured
	call := Call{Input: input, WorkspaceID: e.workspaceID, ClickUpTokenRef: e.ClickUpTokenRef}
	if rs, ok := tool.(RepoScoped); ok {
		repo, err := rs.Repo(input)
		if err != nil {
			return e.deny(ctx, start, "invalid_input")
		}
		start.Repo = repo
		grant, ok := e.policy.Repos[repo]
		if !ok {
			return e.deny(ctx, start, "repo_not_configured")
		}
		account, ok := e.policy.Accounts[grant.Account]
		if !ok {
			return e.deny(ctx, start, "account_missing")
		}
		start.Account = account.Name
		call.Repo, call.Account = &grant, &account
	}
	// 5. tier gate
	tier := spec.Tier
	if esc, ok := tool.(TierEscalator); ok && call.Repo != nil {
		if t := esc.EffectiveTier(input, call.Repo); tierRank(t) > tierRank(tier) {
			tier = t
		}
	}
	start.Tier = tier
	if tier != tenant.TierRead {
		if call.Repo == nil || !slices.Contains(call.Repo.Allow, name) {
			return e.deny(ctx, start, "tool_not_allowed")
		}
		if tier == tenant.TierDangerous && !e.allowDangerous {
			return e.deny(ctx, start, "dangerous_disabled")
		}
	}
	// 6. budgets
	if overCalls {
		return e.deny(ctx, start, "call_budget")
	}
	if tier != tenant.TierRead {
		e.mu.Lock()
		switch {
		case tier == tenant.TierDangerous && e.dangerous >= e.limits.MaxDangerous:
			e.mu.Unlock()
			return e.deny(ctx, start, "dangerous_budget")
		case e.writes >= e.policy.MaxWritesPerJob:
			e.mu.Unlock()
			return e.deny(ctx, start, "write_budget")
		}
		e.writes++
		if tier == tenant.TierDangerous {
			e.dangerous++
		}
		e.mu.Unlock()
	}
	// 7-8. idempotency and record started
	start.Key = CanonicalKey(e.jobID, name, input)
	runID, prior, err := e.store.Begin(ctx, start)
	if err != nil {
		return CallResult{Content: "error: audit_unavailable", IsError: true, Status: StatusError, Tier: tier}
	}
	if prior != nil {
		if prior.Status == StatusOK {
			return CallResult{Content: "already done earlier in this job: " + prior.Summary, Status: StatusOK, Tier: tier}
		}
		return CallResult{Content: "error: unknown_outcome (an earlier attempt of this exact call did not finish; do not retry it)", IsError: true, Status: StatusUnknownOutcome, Tier: tier}
	}
	// 9. dry run
	if tier != tenant.TierRead && e.policy.DryRun {
		msg := fmt.Sprintf("DRY RUN: would call %s (%s) with %s", name, tier, e.san.CleanOutbound(string(input), false))
		msg = capText(msg, e.limits.OutputCap)
		e.finish(ctx, RunFinish{RunID: runID, Status: StatusDryRun, Summary: summarize(msg)})
		return CallResult{Content: msg, Status: StatusDryRun, Tier: tier}
	}
	// 10. run with the tier timeout
	timeout := e.limits.ReadTimeout
	if nt, ok := tool.(NetworkTool); ok && nt.UsesNetwork() {
		timeout = e.limits.NetworkTimeout
	}
	call.DryRun = e.policy.DryRun
	began := time.Now()
	out, runErr := runTool(ctx, tool, call, timeout)
	dur := time.Since(began).Milliseconds()
	if runErr != nil {
		status, code := classify(runErr, tier)
		e.finish(ctx, RunFinish{RunID: runID, Status: status, ErrorCode: code, DurationMS: dur})
		return CallResult{Content: "error: " + code, IsError: true, Status: status, Tier: tier}
	}
	// 11. sanitize, compact, cap
	text := capText(compact(e.san.CleanOutbound(out.Text, false)), e.limits.OutputCap)
	summary := out.Summary
	if summary == "" {
		summary = text
	}
	// 12. final status
	e.finish(ctx, RunFinish{RunID: runID, Status: StatusOK, DurationMS: dur, OutputBytes: len(out.Text), Summary: summarize(e.san.CleanOutbound(summary, false))})
	return CallResult{Content: text, Status: StatusOK, Tier: tier}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "timeout" }

func runTool(ctx context.Context, tool Tool, call Call, timeout time.Duration) (out ToolOutput, err error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = &ToolError{Code: "tool_panic"}
		}
	}()
	out, err = tool.Run(rctx, call)
	if err != nil && rctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return out, timeoutErr{}
	}
	return out, err
}

// classify maps a run error to an audit status and a safe code. Non-read
// failures that may have taken effect are unknown_outcome so they are
// never re-executed on retry.
func classify(err error, tier tenant.Tier) (string, string) {
	var te *ToolError
	isTimeout := errors.As(err, new(timeoutErr))
	switch {
	case errors.As(err, &te) && te.Code == "tool_panic":
		if tier == tenant.TierRead {
			return StatusError, te.Code
		}
		return StatusUnknownOutcome, te.Code
	case errors.As(err, &te) && te.Code != StatusUnknownOutcome:
		return StatusError, te.Code
	case isTimeout && tier == tenant.TierRead:
		return StatusTimeout, "timeout"
	case tier == tenant.TierRead:
		return StatusError, "tool_failed"
	case isTimeout:
		return StatusUnknownOutcome, "timeout"
	default:
		return StatusUnknownOutcome, StatusUnknownOutcome
	}
}

func (e *Executor) deny(ctx context.Context, start RunStart, code string) CallResult {
	_ = e.store.Deny(ctx, start, code)
	return CallResult{Content: "denied: " + code, IsError: true, Status: StatusDenied, Tier: start.Tier}
}

func (e *Executor) finish(ctx context.Context, f RunFinish) {
	// The audit write must survive a cancelled job context.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = e.store.Finish(fctx, f)
}

func (e *Executor) redactInput(input json.RawMessage) json.RawMessage {
	s := string(input)
	if len(s) > MaxInputBytes {
		s = s[:MaxInputBytes]
	}
	b, _ := json.Marshal(e.san.CleanOutbound(strings.ToValidUTF8(s, ""), false))
	return b
}

// CanonicalKey identifies a call independently of JSON key order and
// whitespace: sha256(jobID | tool | canonical JSON).
func CanonicalKey(jobID int64, tool string, input json.RawMessage) string {
	canon := []byte(input)
	var v any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			canon = b
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", jobID, tool, canon)))
	return hex.EncodeToString(sum[:])
}

func isJSONObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

func tierRank(t tenant.Tier) int {
	switch t {
	case tenant.TierWrite:
		return 1
	case tenant.TierDangerous:
		return 2
	default:
		return 0
	}
}

// compact trims trailing whitespace on each line and outer blank lines.
func compact(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// capText limits s to max bytes, cutting at a rune boundary.
func capText(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max - len(truncatedSuffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedSuffix
}

func summarize(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}
