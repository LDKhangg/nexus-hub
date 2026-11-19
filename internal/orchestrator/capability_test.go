package orchestrator

import (
	"slices"
	"testing"
)

// TC-K1 (REQ-5): least-privilege order, recommendation, blocked.
func TestMatch_Table(t *testing.T) {
	r := NewCapabilityRegistry()
	cases := []struct {
		name string
		caps []string
		rec  string
		want string
		ok   bool
	}{
		{"read only goes to investigate", []string{CapRepoRead}, "", ExecutorInvestigate, true},
		{"read prefers investigate over worker", []string{CapRepoRead}, "", ExecutorInvestigate, true},
		{"write needs worker", []string{CapRepoWrite}, "", ExecutorLocalWorker, true},
		{"read and write needs worker", []string{CapRepoRead, CapRepoWrite}, "", ExecutorLocalWorker, true},
		{"clickup write", []string{CapClickUpWrite}, "", ExecutorBehaviorActions, true},
		{"telegram notify", []string{CapTelegramNotify}, "", ExecutorBehaviorActions, true},
		{"clickup read and github read", []string{CapClickUpRead, CapGitHubRead}, "", ExecutorInvestigate, true},
		{"duplicates ignored", []string{CapRepoRead, CapRepoRead, " "}, "", ExecutorInvestigate, true},
		{"read plus clickup write has no executor", []string{CapRepoRead, CapClickUpWrite}, "", "", false},
		{"write plus notify has no executor", []string{CapRepoWrite, CapTelegramNotify}, "", "", false},
		{"unknown capability blocked", []string{"shell.root"}, "", "", false},
		{"known plus unknown blocked", []string{CapRepoRead, "shell.root"}, "", "", false},
		{"empty blocked", nil, "", "", false},
		{"blank blocked", []string{"", "  "}, "", "", false},
		// R-A-3 (REQ-5, least privilege): a recommendation can never raise a task to a more privileged executor.
		{"more privileged covering recommendation ignored", []string{CapRepoRead}, ExecutorLocalWorker, ExecutorInvestigate, true},
		{"read task ignores behavior-actions recommendation", []string{CapRepoRead}, ExecutorBehaviorActions, ExecutorInvestigate, true},
		{"clickup write ignores local-worker recommendation", []string{CapClickUpWrite}, ExecutorLocalWorker, ExecutorBehaviorActions, true},
		{"covering same recommendation", []string{CapRepoRead}, ExecutorInvestigate, ExecutorInvestigate, true},
		{"non covering recommendation ignored", []string{CapRepoWrite}, ExecutorInvestigate, ExecutorLocalWorker, true},
		{"unknown recommendation ignored", []string{CapRepoRead}, "evil-executor", ExecutorInvestigate, true},
		{"recommendation cannot unblock", []string{"shell.root"}, ExecutorLocalWorker, "", false},
		{"recommendation cannot unblock mixed", []string{CapRepoRead, CapClickUpWrite}, ExecutorBehaviorActions, "", false},
		{"recommendation is case sensitive", []string{CapRepoRead}, "Local-Worker", ExecutorInvestigate, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := r.Match(c.caps, c.rec)
			if got != c.want || ok != c.ok {
				t.Fatalf("Match(%v, %q) = %q,%v want %q,%v", c.caps, c.rec, got, ok, c.want, c.ok)
			}
		})
	}
}

// TC-K2 (REQ-5): a repo.read-only task is investigate, never the worker, even when the model asks.
func TestMatch_RepoReadOnlyIsInvestigate(t *testing.T) {
	r := NewCapabilityRegistry()
	got, ok := r.Match([]string{CapRepoRead}, "")
	if !ok || got != ExecutorInvestigate {
		t.Fatal(got)
	}
	p, _ := r.Profile(ExecutorInvestigate)
	if p.Worker {
		t.Fatal("investigate must run in process")
	}
	w, _ := r.Profile(ExecutorLocalWorker)
	if !w.Worker {
		t.Fatal("local-worker must be a worker job")
	}
}

func TestMatch_NilRegistry(t *testing.T) {
	var r *CapabilityRegistry
	if _, ok := r.Match([]string{CapRepoRead}, ""); ok {
		t.Fatal("nil registry matched")
	}
	if r.Profiles() != nil || r.KnownCapabilities() != nil {
		t.Fatal("nil registry lists")
	}
	if _, ok := r.Profile("x"); ok {
		t.Fatal("nil registry profile")
	}
	var zero CapabilityRegistry
	if _, ok := zero.Match([]string{CapRepoRead}, ""); ok {
		t.Fatal("zero registry matched")
	}
}

// TC-K3 (REQ-5): preference order is fixed and every known capability is matchable alone.
func TestRegistry_OrderAndKnown(t *testing.T) {
	r := NewCapabilityRegistry()
	var names []string
	for _, p := range r.Profiles() {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{ExecutorInvestigate, ExecutorBehaviorActions, ExecutorLocalWorker}) {
		t.Fatalf("order %v", names)
	}
	known := r.KnownCapabilities()
	if !slices.IsSorted(known) {
		t.Fatalf("not sorted %v", known)
	}
	want := []string{CapClickUpRead, CapClickUpWrite, CapGitHubMerge, CapGitHubRead, CapRepoRead, CapRepoWrite, CapTelegramNotify}
	if !slices.Equal(known, want) {
		t.Fatalf("known %v", known)
	}
	for _, c := range known {
		if _, ok := r.Match([]string{c}, ""); !ok {
			t.Fatalf("known capability %s has no executor", c)
		}
	}
	// Returned slices are copies.
	ps := r.Profiles()
	ps[0].Capabilities[0] = "tampered"
	ps[0].Name = "tampered"
	if p, _ := r.Profile(ExecutorInvestigate); p.Name != ExecutorInvestigate || p.Capabilities[0] == "tampered" {
		t.Fatal("registry state exposed to callers")
	}
	if _, ok := r.Match([]string{"tampered"}, ""); ok {
		t.Fatal("tampering changed matching")
	}
}

// TC-K4 (REQ-5): AssignExecutors sets executors, blocks uncovered tasks, never mutates the input.
func TestAssignExecutors(t *testing.T) {
	r := NewCapabilityRegistry()
	in := Plan{
		Tasks: []PlanTask{
			{Key: "a", Capabilities: []string{CapRepoRead}, Executor: ExecutorLocalWorker},
			{Key: "b", Capabilities: []string{CapRepoWrite}, DependsOn: []string{"a"}, Executor: ExecutorInvestigate},
			{Key: "c", Capabilities: []string{CapRepoRead, CapClickUpWrite}, Executor: ExecutorBehaviorActions},
			{Key: "d", Capabilities: []string{"shell.root"}},
			{Key: "e", Capabilities: nil},
		},
		Order:    []string{"a", "b", "c", "d", "e"},
		Decision: DecisionExecute,
		Budget:   BudgetRequest{PlanIterations: 1},
	}
	orig := Plan{Order: slices.Clone(in.Order), Tasks: slices.Clone(in.Tasks)}
	out, blocked := r.AssignExecutors(in)
	if !slices.Equal(blocked, []string{"c", "d", "e"}) {
		t.Fatalf("blocked %v", blocked)
	}
	get := func(k string) string { tk, _ := out.Task(k); return tk.Executor }
	if get("a") != ExecutorInvestigate || get("b") != ExecutorLocalWorker || get("c") != "" || get("d") != "" || get("e") != "" {
		t.Fatalf("executors a=%q b=%q c=%q d=%q e=%q", get("a"), get("b"), get("c"), get("d"), get("e"))
	}
	if out.Decision != DecisionExecute || out.Budget != in.Budget || !slices.Equal(out.Order, in.Order) {
		t.Fatalf("plan fields lost %+v", out)
	}
	for i, tk := range in.Tasks {
		if tk.Executor != orig.Tasks[i].Executor {
			t.Fatalf("input task %s mutated", tk.Key)
		}
	}
	// Output slices are independent of the input.
	out.Tasks[1].DependsOn[0] = "zzz"
	out.Order[0] = "zzz"
	if in.Tasks[1].DependsOn[0] != "a" || in.Order[0] != "a" {
		t.Fatal("output aliases input slices")
	}
	// A blocked task is never assigned, even when the model proposed an executor.
	if tk, _ := out.Task("c"); tk.Executor != "" {
		t.Fatalf("blocked task assigned %q", tk.Executor)
	}
}
