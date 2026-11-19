package orchestrator

import (
	"slices"
	"sort"
)

// Capability names (O3a). Plans may only use KnownCapabilities.
const (
	CapRepoRead       = "repo.read"
	CapRepoWrite      = "repo.write"
	CapClickUpRead    = "clickup.read"
	CapGitHubRead     = "github.read"
	CapClickUpWrite   = "clickup.write"
	CapTelegramNotify = "telegram.notify"
	// CapGitHubMerge merges one pull request (repo, number and the exact
	// head SHA from earlier task evidence, PlanTask.Merge) under the policy
	// engine; the default rule asks the owner first (Phase 9 REQ-9).
	CapGitHubMerge = "github.merge"
)

// Executor names of the static registry (O3a).
const (
	// ExecutorInvestigate runs read-only work in process.
	ExecutorInvestigate = "investigate"
	// ExecutorBehaviorActions runs behavior actions in process, under the
	// existing action authorizer and approval path.
	ExecutorBehaviorActions = "behavior-actions"
	// ExecutorLocalWorker is the Phase 7a local worker (a job through the
	// local-task machinery).
	ExecutorLocalWorker = "local-worker"
)

// CapabilityOfferer is optionally implemented by an in-process executor
// that can run only some of its profile's capabilities (the merge needs
// its tool and the policy engine wired). Plan is offered a capability of
// such an executor only when Offers reports true.
type CapabilityOfferer interface {
	Offers(capability string) bool
}

// ExecutorProfile is one executor and the capabilities it covers.
type ExecutorProfile struct {
	Name         string
	Capabilities []string
	// Worker is true when the executor runs as a 7a worker job instead of
	// in process.
	Worker bool
}

func (p ExecutorProfile) covers(caps []string) bool {
	for _, c := range caps {
		if !slices.Contains(p.Capabilities, c) {
			return false
		}
	}
	return true
}

// CapabilityRegistry holds static executor profiles in a fixed preference
// order: least privileged first. Go, not the model, chooses executors from
// it (REQ-5). The zero value has no profiles; use NewCapabilityRegistry.
type CapabilityRegistry struct {
	profiles []ExecutorProfile
}

// NewCapabilityRegistry returns the static O3a registry. Preference order:
// investigate (read-only, in process), behavior-actions (in process, under
// policy), local-worker (repo.write; also covers repo.read).
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{profiles: []ExecutorProfile{
		{Name: ExecutorInvestigate, Capabilities: []string{CapClickUpRead, CapGitHubRead, CapRepoRead}},
		{Name: ExecutorBehaviorActions, Capabilities: []string{CapClickUpWrite, CapTelegramNotify, CapGitHubMerge}},
		{Name: ExecutorLocalWorker, Capabilities: []string{CapRepoWrite, CapRepoRead}, Worker: true},
	}}
}

// Profiles returns a copy of the profiles in preference order.
func (r *CapabilityRegistry) Profiles() []ExecutorProfile {
	if r == nil {
		return nil
	}
	out := make([]ExecutorProfile, len(r.profiles))
	for i, p := range r.profiles {
		p.Capabilities = slices.Clone(p.Capabilities)
		out[i] = p
	}
	return out
}

// Profile returns the profile named name.
func (r *CapabilityRegistry) Profile(name string) (ExecutorProfile, bool) {
	if r == nil {
		return ExecutorProfile{}, false
	}
	for _, p := range r.profiles {
		if p.Name == name {
			p.Capabilities = slices.Clone(p.Capabilities)
			return p, true
		}
	}
	return ExecutorProfile{}, false
}

// KnownCapabilities returns every capability some profile covers, sorted.
// It is the Capabilities input of a Plan call.
func (r *CapabilityRegistry) KnownCapabilities() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, p := range r.profiles {
		for _, c := range p.Capabilities {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CapabilitiesFor returns the capabilities covered by the profiles for
// which available reports true, sorted. Steps use it to offer Plan only the
// capabilities of wired executors, so a plan naming a capability without a
// wired executor fails the known-capability check (repair).
func (r *CapabilityRegistry) CapabilitiesFor(available func(ExecutorProfile) bool) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, p := range r.profiles {
		if available != nil && !available(p) {
			continue
		}
		for _, c := range p.Capabilities {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Match picks the executor for a task that needs caps. It is deterministic
// and least-privilege: profiles are tried in their fixed order (least
// privileged first). The recommended executor (the model's proposal) is
// honoured only when its profile covers every capability and no earlier
// profile covers them; otherwise the first covering profile is used, so a
// model can never raise a task to a more privileged executor. Empty caps, an
// unknown capability or no covering profile return ok=false.
func (r *CapabilityRegistry) Match(caps []string, recommended string) (string, bool) {
	if r == nil {
		return "", false
	}
	caps = uniqueNonEmpty(caps)
	if len(caps) == 0 {
		return "", false
	}
	for _, p := range r.profiles {
		if !p.covers(caps) {
			continue
		}
		// p is the least-privilege covering profile: a recommendation
		// either names p (honoured) or a later profile (ignored).
		return p.Name, true
	}
	return "", false
}

// AssignExecutors returns a copy of plan with every task's Executor set by
// Match, and the keys (plan task order) of tasks no profile covers. A
// blocked task's Executor is cleared, so it is never assigned. The input
// plan is not modified.
func (r *CapabilityRegistry) AssignExecutors(plan Plan) (Plan, []string) {
	out := plan
	out.Tasks = make([]PlanTask, len(plan.Tasks))
	out.Order = slices.Clone(plan.Order)
	var blocked []string
	for i, t := range plan.Tasks {
		t.Capabilities = slices.Clone(t.Capabilities)
		t.DependsOn = slices.Clone(t.DependsOn)
		exec, ok := r.Match(t.Capabilities, t.Executor)
		if ok {
			t.Executor = exec
		} else {
			t.Executor = ""
			blocked = append(blocked, t.Key)
		}
		out.Tasks[i] = t
	}
	return out, blocked
}
