package orchestrator

import (
	"slices"
	"strings"
)

// NextPlan merges a replanned plan into the next plan version (REQ-8). It
// is pure: storage keeps every version immutable, and the caller charges
// the replan with Budget.StartPlanIteration(true, …) before the Plan call
// that produced next.
//
// version is prevVersion+1 (1 when prevVersion < 1). A task of prev whose
// record in prevTasks is SUCCEEDED is finished: merged keeps its definition
// from prev (the new plan cannot change its capabilities, dependencies or
// executor), keeps it even when next leaves it out, and carried returns its
// record (result, result step, evidence) with PlanID cleared, to store with
// the new version. A finished task's dependencies are reduced to finished
// tasks; they were satisfied before it ran. Every other task comes from
// next unchanged. Inputs are not modified.
func NextPlan(prev Plan, prevVersion int, prevTasks []PlanTaskRecord, next Plan) (version int, merged Plan, carried []PlanTaskRecord) {
	version = max(prevVersion, 0) + 1

	done := map[string]PlanTaskRecord{}
	for _, r := range prevTasks {
		if r.State != TaskSucceeded {
			continue
		}
		if _, ok := prev.Task(r.Key); ok {
			done[r.Key] = r
		}
	}

	finished := func(t PlanTask) PlanTask {
		t.Capabilities = slices.Clone(t.Capabilities)
		var deps []string
		for _, d := range t.DependsOn {
			if _, ok := done[d]; ok {
				deps = append(deps, d)
			}
		}
		t.DependsOn = deps
		return t
	}

	merged = Plan{Decision: next.Decision, Budget: next.Budget}
	inNext := map[string]bool{}
	for _, t := range next.Tasks {
		inNext[t.Key] = true
	}
	// Finished tasks the new plan left out come first, in prev order.
	for _, t := range prev.Tasks {
		if _, ok := done[t.Key]; ok && !inNext[t.Key] {
			merged.Tasks = append(merged.Tasks, finished(t))
		}
	}
	for _, t := range next.Tasks {
		if _, ok := done[t.Key]; ok {
			p, _ := prev.Task(t.Key)
			merged.Tasks = append(merged.Tasks, finished(p))
			continue
		}
		t.Capabilities = slices.Clone(t.Capabilities)
		t.DependsOn = slices.Clone(t.DependsOn)
		merged.Tasks = append(merged.Tasks, t)
	}

	if order, ok := topoOrder(merged.Tasks); ok {
		merged.Order = order
	} else {
		// next passed the plan checks and finished tasks depend only on
		// finished tasks, so this cannot happen; keep task order.
		for _, t := range merged.Tasks {
			merged.Order = append(merged.Order, t.Key)
		}
	}

	for _, t := range merged.Tasks {
		r, ok := done[t.Key]
		if !ok {
			continue
		}
		r.PlanID = 0
		r.Capabilities = slices.Clone(t.Capabilities)
		r.DependsOn = slices.Clone(t.DependsOn)
		r.Result = slices.Clone(r.Result)
		r.EvidenceRefs = slices.Clone(r.EvidenceRefs)
		carried = append(carried, r)
	}
	return version, merged, carried
}

// ResolveRepo picks the repository of a repo.write task (REQ-14). An
// explicit repo wins when it is registered; an explicit repo that is not
// registered needs clarification (it never falls back to another repo).
// Without an explicit repo, the remembered repo of the task is used when it
// is still registered, else the only registered repo when exactly one is
// registered (like /task). Otherwise needClarification is true. Matching is
// case-insensitive and returns the registered spelling. remembered may be
// nil.
func ResolveRepo(explicit string, remembered func() (string, bool), registered []string) (repo string, needClarification bool) {
	find := func(name string) (string, bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", false
		}
		for _, r := range registered {
			if strings.EqualFold(strings.TrimSpace(r), name) {
				return strings.TrimSpace(r), true
			}
		}
		return "", false
	}
	if strings.TrimSpace(explicit) != "" {
		if r, ok := find(explicit); ok {
			return r, false
		}
		return "", true
	}
	if remembered != nil {
		if last, ok := remembered(); ok {
			if r, ok := find(last); ok {
				return r, false
			}
		}
	}
	var only []string
	for _, r := range registered {
		if r = strings.TrimSpace(r); r != "" {
			only = append(only, r)
		}
	}
	if len(only) == 1 {
		return only[0], false
	}
	return "", true
}
