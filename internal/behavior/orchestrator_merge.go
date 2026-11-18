package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is the github.merge path of the behavior-actions executor
// (Phase 9 REQ-9..11). The merge always goes through the Phase 2 tool
// executor under the policy engine (actor workflow): allow merges once;
// deny blocks the task (policy_denied) with no side effect; approval
// required creates a policy_gate approval bound to the exact merge input
// (repo, number, head SHA, merge method), asks the owner in the execution's
// topic and waits. After the owner approves, the same call runs again with
// the approval as its grant: the executor verifies and consumes it, decides
// again (FR-130), and the merge tool checks that the head is unchanged and
// the checks are green. The idempotency key is the execution's plan task,
// so a restart replays the merge instead of merging twice.

// MergeToolName is the tool the github.merge capability runs.
const MergeToolName = "github_pr_merge"

// Reason codes of github.merge tasks.
const (
	ReasonMergeNotConfigured = "merge_not_configured"
	ReasonMergeUnknown       = "merge_outcome_unknown"
	ReasonApprovalRejected   = "approval_rejected"
	ReasonApprovalExpired    = "approval_expired"
	ReasonApprovalMissing    = "approval_missing"
	ReasonApprovalNotSent    = "approval_not_sent"
	ReasonApprovalElsewhere  = "approval_pending_elsewhere"
	ReasonApprovalNoApprover = "no_owner_member"
	// ReasonMergeTargetUnproven: policy allows the merge without the
	// owner, but no earlier succeeded task of the execution names the
	// target's repo#number and head SHA (review B R-B-3).
	ReasonMergeTargetUnproven = "merge_target_unproven"
	ReasonMergePolicyDenied   = tools.DenyPolicyDenied
	ReasonMergeRevalidateDeny = tools.DenyRevalidateDenied
)

// mergeKeyScope namespaces the merge's idempotency keys; the plan task
// (step and key) is part of the scope, the execution is the executor's id.
const mergeKeyScope = "orchestrator_merge:"

// OrchestratorMerge runs github.merge tasks.
type OrchestratorMerge struct {
	Registry  *tools.Registry
	Audit     tools.ToolRunStore // workflow execution audit (GrantedRunStore)
	Sanitizer tools.Sanitizer
	Limits    tools.Limits
	// Policies returns the workspace's agent configuration: the repo
	// grant (allowed tools, merge methods and base branches, green checks)
	// stays the inner check; policy only narrows it.
	Policies PolicySource
	Engine   tools.PolicyGate
	Gates    *PolicyGates
	Now      func() time.Time
}

func (m *OrchestratorMerge) ready() bool {
	return m != nil && m.Registry != nil && m.Registry.Get(MergeToolName) != nil && m.Audit != nil &&
		m.Sanitizer != nil && m.Policies != nil && m.Engine != nil && m.Gates != nil && m.Gates.Store != nil
}

func (m *OrchestratorMerge) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Offers reports whether the executor can run capability c. github.merge
// needs the merge tool, the audit, the policy engine and the gates.
func (a *OrchestratorActions) Offers(c string) bool {
	if c == orchestrator.CapGitHubMerge {
		return a.Merge.ready()
	}
	return true
}

var _ orchestrator.CapabilityOfferer = (*OrchestratorActions)(nil)

// decisionCapture remembers the last decision the executor got, so the
// gate uses the deciding rule and its approval TTL (one recorded decision
// per call). Without the owner's approval (no grant in the context), an
// allow for a merge target that earlier task evidence does not name is
// turned into a deny before any side effect (review B R-B-3); under
// approval_required the owner's approval of the shown target binds it.
type decisionCapture struct {
	gate     tools.PolicyGate
	proven   bool
	mu       sync.Mutex
	last     policy.Decision
	unproven bool
}

func (c *decisionCapture) Decide(ctx context.Context, req policy.Request) (policy.Decision, error) {
	d, err := c.gate.Decide(ctx, req)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = d
	if _, granted := tools.ApprovalGrantFrom(ctx); err == nil && d.Effect == policy.EffectAllow && !c.proven && !granted {
		c.unproven = true
		d.Effect = policy.EffectDeny
	}
	return d, err
}

// unprovenBlocked reports whether an allow was refused because the target
// is not in earlier evidence.
func (c *decisionCapture) unprovenBlocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unproven
}

// mergeTargetProven reports whether one earlier succeeded task's evidence
// (run.Proofs) names both the target's PR (repo#number or its GitHub pull
// URL) and its full head SHA.
func mergeTargetProven(proofs []string, t orchestrator.MergeTarget) bool {
	num := strconv.FormatInt(t.Number, 10)
	pr := regexp.MustCompile(`(?i)(?:github\.com/` + regexp.QuoteMeta(t.Repo) + `/pull/|(?:^|[^A-Za-z0-9_.-])` +
		regexp.QuoteMeta(t.Repo) + `#)` + num + `(?:[^0-9]|$)`)
	sha := regexp.MustCompile(`(?:^|[^0-9a-f])` + regexp.QuoteMeta(t.HeadSHA) + `(?:[^0-9a-f]|$)`)
	for _, p := range proofs {
		if pr.MatchString(p) && sha.MatchString(strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func (c *decisionCapture) decision() policy.Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// mergeCall is one prepared merge call.
type mergeCall struct {
	exec    *tools.Executor
	capture *decisionCapture
	input   json.RawMessage
	target  orchestrator.MergeTarget
}

// prepare builds the merge input and the executor of run. The merge method
// is the repo grant's first allowed method. A missing grant or method
// blocks the task with merge_not_configured.
func (m *OrchestratorMerge) prepare(ctx context.Context, run orchestrator.TaskRun) (*mergeCall, orchestrator.TaskOutcome, bool, error) {
	t := run.Task.Merge
	if !orchestrator.ValidMergeTarget(t) || len(run.Task.Capabilities) != 1 {
		return nil, actionFailed(ReasonActionInvalidText), false, nil
	}
	cfg, err := m.Policies.RunPolicy(ctx, run.WorkspaceID, run.Route.IntegrationID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, orchestrator.TaskOutcome{}, false, ctx.Err()
		}
		return nil, actionFailed(orchestrator.ReasonExecutorUnavailable), false, nil
	}
	grant, ok := cfg.Tools.Repos[t.Repo]
	if !ok || len(grant.MergeMethods) == 0 || !slices.Contains(cfg.Tools.Enabled, MergeToolName) {
		return nil, orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonMergeNotConfigured}, false, nil
	}
	input, _ := json.Marshal(map[string]any{
		"repo": t.Repo, "number": t.Number, "head_sha": t.HeadSHA, "merge_method": grant.MergeMethods[0],
	})
	// Only the merge tool and this repo's grant: the executor's own grant,
	// tier and budget checks run as for any call.
	pol := tenant.ToolPolicyConfig{
		Enabled: []string{MergeToolName}, DryRun: cfg.Tools.DryRun, MaxWritesPerJob: cfg.Tools.MaxWritesPerJob,
		Repos: map[string]tenant.RepoGrant{t.Repo: grant}, Accounts: cfg.Tools.Accounts,
	}
	limits := m.Limits
	limits.MaxCalls = 2
	limits.MaxDangerous = max(limits.MaxDangerous, 1)
	capture := &decisionCapture{gate: m.Engine, proven: mergeTargetProven(run.Proofs, *t)}
	exec := tools.NewExecutor(m.Registry, pol, limits, m.Audit, m.Sanitizer, run.ExecutionID, run.WorkspaceID,
		tools.WithPolicy(capture, policy.ActorWorkflow), tools.WithExecutionID(run.ExecutionID))
	exec.ClickUpTokenRef = cfg.ClickUpTokenRef
	exec.KeyScope = mergeKeyScope + run.StepKey + ":" + run.Task.Key
	return &mergeCall{exec: exec, capture: capture, input: input, target: *t}, orchestrator.TaskOutcome{}, true, nil
}

var mergedSHARe = regexp.MustCompile(`([0-9a-f]{7,40})\s*$`)

// mergeOutcome maps the merge call's result. A refusal after approval or
// any failure blocks the task (nothing merged, or not known): Evaluate
// decides.
func mergeOutcome(res tools.CallResult, t orchestrator.MergeTarget) orchestrator.TaskOutcome {
	switch {
	case res.Status == tools.StatusOK:
		sha := ""
		if mm := mergedSHARe.FindStringSubmatch(res.Content); mm != nil {
			sha = mm[1]
		}
		ref := fmt.Sprintf("github:merge:%s#%d:%s", t.Repo, t.Number, sha)
		if len(ref) > 128 {
			ref = fmt.Sprintf("github:merge:%d:%s", t.Number, sha)
		}
		return orchestrator.TaskOutcome{
			Succeeded: true, Summary: fmt.Sprintf("Đã merge PR %s#%d (head %s).", t.Repo, t.Number, shortSHA(t.HeadSHA)),
			Evidence: []orchestrator.EvidenceRef{{Kind: orchestrator.EvidenceArtifact, ID: ref}},
		}
	case res.Status == tools.StatusDryRun:
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: OutcomePolicyDryRun}
	case res.Status == tools.StatusUnknownOutcome:
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonMergeUnknown}
	}
	return orchestrator.TaskOutcome{Blocked: true, ReasonCode: resultCode(res)}
}

// merge runs a github.merge task: allow merges, deny blocks, approval
// required asks the owner and waits on the gate.
func (a *OrchestratorActions) merge(ctx context.Context, run orchestrator.TaskRun) (orchestrator.TaskOutcome, error) {
	m := a.Merge
	if !m.ready() {
		return actionFailed(orchestrator.ReasonExecutorUnavailable), nil
	}
	call, out, ok, err := m.prepare(ctx, run)
	if !ok || err != nil {
		return out, err
	}
	res := call.exec.Call(ctx, 1, MergeToolName, call.input)
	if res.Status == tools.StatusDenied && call.capture.unprovenBlocked() {
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonMergeTargetUnproven}, nil
	}
	if res.Status != tools.StatusDenied || resultCode(res) != tools.DenyApprovalRequired {
		return mergeOutcome(res, call.target), nil
	}
	return a.askGate(ctx, run, call)
}

// askGate creates (or finds) the policy gate of the exact merge input,
// shows it to the owner and returns the wait on it.
func (a *OrchestratorActions) askGate(ctx context.Context, run orchestrator.TaskRun, call *mergeCall) (orchestrator.TaskOutcome, error) {
	m := a.Merge
	store := m.Gates.Store
	owner, err := store.WorkspaceOwner(ctx, run.WorkspaceID)
	switch {
	case errors.Is(err, ErrNotFound):
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalNoApprover}, nil
	case err != nil:
		return orchestrator.TaskOutcome{}, err
	}
	d := call.capture.decision()
	tool := m.Registry.Get(MergeToolName)
	// The store caps the expiry at the execution's deadline, and expires
	// an open gate of the same merge that is past its expiry or whose
	// execution ended (review B R-B-1, R-B-5).
	var gate policy.Gate
	for attempt := 0; ; attempt++ {
		now := m.now()
		gate, err = store.CreateGate(ctx, policy.GateRequest{
			WorkspaceID: run.WorkspaceID, Action: MergeToolName, ResourceKey: tools.ResourceKey(tool, call.input),
			InputSHA256: tools.InputSHA256(call.input), ExecutionID: run.ExecutionID, RequestedUserID: owner,
			ExpiresAt: policy.GateExpiry(now, d), Now: now,
		})
		if err != nil {
			return orchestrator.TaskOutcome{}, err
		}
		if gate.ClosedID > 0 && gate.ClosedID != gate.ID {
			m.Gates.closeExpired(ctx, run.WorkspaceID, gate.ClosedID)
		}
		if gate.Created || m.now().Before(gate.ExpiresAt) {
			break
		}
		// The open gate returned has passed its expiry: the next call
		// expires it and creates a new one.
		if attempt == 1 {
			return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalExpired}, nil
		}
	}
	if gate.Created && !m.now().Before(gate.ExpiresAt) {
		// Capped at a deadline that has already passed.
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalExpired}, nil
	}
	if gate.ExecutionID != run.ExecutionID {
		// Another execution already asks for the very same merge.
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalElsewhere}, nil
	}
	sent := false
	if !gate.Created {
		rec, err := store.PolicyGate(ctx, run.WorkspaceID, gate.ID)
		if err != nil {
			return orchestrator.TaskOutcome{}, err
		}
		sent = rec.MessageID > 0
	}
	if !sent {
		chat := ChatRef{WorkspaceID: run.Route.WorkspaceID, IntegrationID: run.Route.IntegrationID, ChatID: run.Route.ChatID, ThreadID: run.Route.ThreadID}
		if a.Chat != nil {
			chat = a.Chat(ctx, run.Route)
		}
		p := policyGatePayload{StepKey: run.StepKey, Repo: call.target.Repo, Number: call.target.Number,
			Head: shortSHA(call.target.HeadSHA), RuleKey: d.RuleKey}
		if !m.Gates.Ask(ctx, chat, gate, p) {
			// The owner never saw it: the open gate expires on its own,
			// and a later identical merge resends it.
			return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalNotSent}, nil
		}
	}
	return orchestrator.TaskOutcome{Wait: gateWaitRef(gate.ID)}, nil
}

// resumeMerge follows a github.merge task. Without a wait reference (an
// attempt stopped before it recorded the wait) it runs the merge path
// again: the same idempotency key replays a merge that already ran, and
// the same input finds the open gate. With one, it applies the gate's
// decision: approved runs the merge once under the grant; rejected and
// expired block the task; an open gate keeps waiting.
func (a *OrchestratorActions) resumeMerge(ctx context.Context, run orchestrator.TaskRun, waitRef string) (orchestrator.TaskOutcome, error) {
	m := a.Merge
	if !m.ready() {
		return actionFailed(ReasonActionInterrupted), nil
	}
	if waitRef == "" {
		return a.merge(ctx, run)
	}
	id, ok := parseGateWaitRef(waitRef)
	if !ok {
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalMissing}, nil
	}
	rec, err := m.Gates.Store.PolicyGate(ctx, run.WorkspaceID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalMissing}, nil
	case err != nil:
		return orchestrator.TaskOutcome{}, err
	}
	if rec.ExecutionID != run.ExecutionID || rec.Action != MergeToolName {
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalMissing}, nil
	}
	switch rec.Decision {
	case "":
		// Still open (the sweep expires it and wakes the step).
		return orchestrator.TaskOutcome{Wait: waitRef}, nil
	case DecisionRejected:
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalRejected}, nil
	case DecisionApproved:
	default:
		// Expired (the expiry sweep left the message to this step).
		m.Gates.finalEdit(ctx, run.WorkspaceID, rec, DecisionExpired)
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonApprovalExpired}, nil
	}
	call, out, ok, err := m.prepare(ctx, run)
	if !ok || err != nil {
		return out, err
	}
	res := call.exec.Call(tools.WithApprovalGrant(ctx, tools.ApprovalGrant{ApprovalID: rec.ID}), 1, MergeToolName, call.input)
	if res.Status == tools.StatusDenied && resultCode(res) == tools.DenyApprovalRequired {
		// The grant was not even offered (cannot happen with a grant in
		// the context); never ask again for an approved gate.
		return orchestrator.TaskOutcome{Blocked: true, ReasonCode: ReasonMergeRevalidateDeny}, nil
	}
	out = mergeOutcome(res, call.target)
	if !out.Succeeded {
		log.Printf("orchestrator merge: execution %d gate %d not merged (%s)", run.ExecutionID, rec.ID, out.ReasonCode)
	}
	return out, nil
}
