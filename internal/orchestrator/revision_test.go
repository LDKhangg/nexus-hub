package orchestrator

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func pt(key string, deps ...string) PlanTask {
	return PlanTask{Key: key, Title: "T " + key, Capabilities: []string{CapRepoRead}, DependsOn: deps, Priority: "normal", Complexity: "s"}
}

func rec(key, state string) PlanTaskRecord {
	return PlanTaskRecord{PlanID: 7, Key: key, State: state}
}

func pos(order []string, k string) int { return slices.Index(order, k) }

func checkOrder(t *testing.T, p Plan) {
	t.Helper()
	if len(p.Order) != len(p.Tasks) {
		t.Fatalf("order %v for %d tasks", p.Order, len(p.Tasks))
	}
	for _, tk := range p.Tasks {
		for _, d := range tk.DependsOn {
			if pos(p.Order, d) < 0 || pos(p.Order, d) >= pos(p.Order, tk.Key) {
				t.Fatalf("order %v puts %s before its dependency %s", p.Order, tk.Key, d)
			}
		}
	}
}

// TC-N1 (REQ-8): version is v+1; finished tasks are kept with their old definition.
func TestNextPlan_KeepsFinishedWithOldDefinition(t *testing.T) {
	prevA := pt("a")
	prevA.Executor = ExecutorInvestigate
	prev := Plan{Tasks: []PlanTask{prevA, pt("b", "a"), pt("c", "b")}, Order: []string{"a", "b", "c"}}
	recs := []PlanTaskRecord{
		{PlanID: 7, Key: "a", State: TaskSucceeded, ResultStepID: "step-1", Result: json.RawMessage(`{"ok":true}`), EvidenceRefs: []string{"s1"}},
		rec("b", TaskFailed), rec("c", TaskPending),
	}
	// The new plan tries to rewrite a (different capabilities/executor) and replaces b and c.
	na := pt("a")
	na.Capabilities = []string{CapRepoWrite}
	na.Executor = ExecutorLocalWorker
	na.Title = "rewritten"
	next := Plan{Tasks: []PlanTask{na, pt("b2", "a"), pt("c", "b2")}, Decision: DecisionExecute, Budget: BudgetRequest{PlanIterations: 3}}
	version, merged, carried := NextPlan(prev, 1, recs, next)
	if version != 2 {
		t.Fatalf("version %d", version)
	}
	ma, ok := merged.Task("a")
	if !ok || !reflect.DeepEqual(ma.Capabilities, []string{CapRepoRead}) || ma.Executor != ExecutorInvestigate || ma.Title != "T a" {
		t.Fatalf("finished task a was changed: %+v", ma)
	}
	if len(carried) != 1 || carried[0].Key != "a" || carried[0].PlanID != 0 || carried[0].State != TaskSucceeded ||
		carried[0].ResultStepID != "step-1" || string(carried[0].Result) != `{"ok":true}` || !slices.Equal(carried[0].EvidenceRefs, []string{"s1"}) {
		t.Fatalf("carried %+v", carried)
	}
	if !slices.Equal(carried[0].Capabilities, []string{CapRepoRead}) {
		t.Fatalf("carried capabilities %v", carried[0].Capabilities)
	}
	if _, ok := merged.Task("b"); ok {
		t.Fatal("unfinished task b should not survive when the new plan drops it")
	}
	if _, ok := merged.Task("b2"); !ok {
		t.Fatal("new task missing")
	}
	if merged.Decision != DecisionExecute || merged.Budget.PlanIterations != 3 {
		t.Fatalf("decision/budget not taken from next: %+v", merged)
	}
	checkOrder(t, merged)
}

// TC-N2 (REQ-8): a finished task the new plan leaves out is kept; it comes before the new tasks.
func TestNextPlan_FinishedTaskLeftOutIsKept(t *testing.T) {
	prev := Plan{Tasks: []PlanTask{pt("a"), pt("b")}, Order: []string{"a", "b"}}
	recs := []PlanTaskRecord{rec("a", TaskSucceeded), rec("b", TaskSucceeded)}
	next := Plan{Tasks: []PlanTask{pt("c", "a")}}
	version, merged, carried := NextPlan(prev, 3, recs, next)
	if version != 4 {
		t.Fatal(version)
	}
	if len(merged.Tasks) != 3 || merged.Tasks[0].Key != "a" || merged.Tasks[1].Key != "b" || merged.Tasks[2].Key != "c" {
		t.Fatalf("%+v", merged.Tasks)
	}
	if len(carried) != 2 {
		t.Fatalf("carried %d", len(carried))
	}
	checkOrder(t, merged)
}

// TC-N3 (REQ-8): finished tasks' dependencies are pruned to finished tasks; no cycle results.
func TestNextPlan_DepsPrunedNoCycle(t *testing.T) {
	// a (done) <- b (done, was waiting on x which never finished) ; x failed.
	prev := Plan{Tasks: []PlanTask{pt("x"), pt("a"), pt("b", "a", "x")}, Order: []string{"x", "a", "b"}}
	recs := []PlanTaskRecord{rec("x", TaskFailed), rec("a", TaskSucceeded), rec("b", TaskSucceeded)}
	// The new plan makes x depend on b and redefines b to depend on x: it would be a cycle if b were redefined.
	next := Plan{Tasks: []PlanTask{pt("x", "b"), pt("b", "x"), pt("a")}}
	_, merged, carried := NextPlan(prev, 1, recs, next)
	mb, _ := merged.Task("b")
	if !slices.Equal(mb.DependsOn, []string{"a"}) {
		t.Fatalf("b deps %v", mb.DependsOn)
	}
	for _, c := range carried {
		if c.Key == "b" && !slices.Equal(c.DependsOn, []string{"a"}) {
			t.Fatalf("carried b deps %v", c.DependsOn)
		}
	}
	checkOrder(t, merged)
	if len(merged.Order) != 3 {
		t.Fatalf("order %v", merged.Order)
	}
}

// TC-N4 (REQ-8): only SUCCEEDED records of tasks in prev are carried; other states are replaced.
func TestNextPlan_OnlySucceededCarried(t *testing.T) {
	prev := Plan{Tasks: []PlanTask{pt("a"), pt("b"), pt("c"), pt("d"), pt("e")}}
	recs := []PlanTaskRecord{
		rec("a", TaskFailed), rec("b", TaskRunning), rec("c", TaskSkipped), rec("d", TaskBlocked),
		rec("ghost", TaskSucceeded), // not in prev
		rec("e", TaskSucceeded),
	}
	next := Plan{Tasks: []PlanTask{pt("a"), pt("b"), pt("c"), pt("d")}}
	_, merged, carried := NextPlan(prev, 1, recs, next)
	if len(carried) != 1 || carried[0].Key != "e" {
		t.Fatalf("carried %+v", carried)
	}
	if _, ok := merged.Task("ghost"); ok {
		t.Fatal("record without a prev task was merged")
	}
	if _, ok := merged.Task("e"); !ok {
		t.Fatal("finished e dropped")
	}
	if len(merged.Tasks) != 5 {
		t.Fatalf("%d tasks", len(merged.Tasks))
	}
}

// TC-N5 (REQ-8): version edge cases and purity.
func TestNextPlan_VersionAndPurity(t *testing.T) {
	prev := Plan{Tasks: []PlanTask{pt("a"), pt("b", "a")}, Order: []string{"a", "b"}}
	recs := []PlanTaskRecord{{PlanID: 9, Key: "a", State: TaskSucceeded, EvidenceRefs: []string{"e1"}, Result: json.RawMessage(`{"k":1}`)}}
	next := Plan{Tasks: []PlanTask{pt("b", "a"), pt("z")}}
	for _, pv := range []int{0, -3} {
		if v, _, _ := NextPlan(prev, pv, nil, next); v != 1 {
			t.Fatalf("prevVersion %d gave %d", pv, v)
		}
	}
	snapPrev, _ := json.Marshal(prev)
	snapNext, _ := json.Marshal(next)
	snapRecs, _ := json.Marshal(recs)
	_, merged, carried := NextPlan(prev, 1, recs, next)
	// Mutating outputs must not change inputs.
	merged.Tasks[0].Capabilities[0] = "tampered"
	merged.Tasks[0].Title = "tampered"
	carried[0].EvidenceRefs[0] = "tampered"
	carried[0].Result[0] = 'X'
	if g, _ := json.Marshal(prev); string(g) != string(snapPrev) {
		t.Fatalf("prev mutated: %s", g)
	}
	if g, _ := json.Marshal(next); string(g) != string(snapNext) {
		t.Fatalf("next mutated: %s", g)
	}
	if g, _ := json.Marshal(recs); string(g) != string(snapRecs) {
		t.Fatalf("records mutated: %s", g)
	}
	// No prior tasks at all: merged is next, same order.
	_, m2, c2 := NextPlan(Plan{}, 0, nil, Plan{Tasks: []PlanTask{pt("a"), pt("b", "a")}})
	if len(c2) != 0 || !slices.Equal(m2.Order, []string{"a", "b"}) {
		t.Fatalf("%+v %+v", m2, c2)
	}
}

// TC-R-A (REQ-14): ResolveRepo order.
func TestResolveRepo(t *testing.T) {
	remember := func(name string, ok bool) func() (string, bool) {
		return func() (string, bool) { return name, ok }
	}
	two := []string{"Acme/Web", "acme/api"}
	cases := []struct {
		name       string
		explicit   string
		remembered func() (string, bool)
		registered []string
		want       string
		clarify    bool
	}{
		{"explicit registered wins over remembered", "acme/api", remember("Acme/Web", true), two, "acme/api", false},
		{"explicit is case-insensitive and returns registered spelling", "ACME/WEB", nil, two, "Acme/Web", false},
		{"explicit with spaces", "  acme/web  ", nil, two, "Acme/Web", false},
		{"explicit not registered needs clarification", "evil/repo", remember("Acme/Web", true), two, "", true},
		{"explicit not registered never falls back to the only repo", "evil/repo", nil, []string{"Acme/Web"}, "", true},
		{"explicit blank is not explicit", "   ", remember("acme/api", true), two, "acme/api", false},
		{"remembered still registered", "", remember("ACME/API", true), two, "acme/api", false},
		{"remembered no longer registered falls to clarification", "", remember("gone/repo", true), two, "", true},
		{"remembered no longer registered but a single repo is registered", "", remember("gone/repo", true), []string{"Acme/Web"}, "Acme/Web", false},
		{"remembered with ok=false is ignored", "", remember("Acme/Web", false), two, "", true},
		{"single registered repo", "", nil, []string{" Acme/Web "}, "Acme/Web", false},
		{"two registered, nothing remembered", "", nil, two, "", true},
		{"none registered", "", nil, nil, "", true},
		{"none registered with remembered", "", remember("Acme/Web", true), nil, "", true},
		{"blank registered entries do not count", "", nil, []string{"", "  ", "Acme/Web"}, "Acme/Web", false},
		{"explicit with none registered", "acme/web", nil, nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clarify := ResolveRepo(c.explicit, c.remembered, c.registered)
			if got != c.want || clarify != c.clarify {
				t.Fatalf("got %q,%v want %q,%v", got, clarify, c.want, c.clarify)
			}
		})
	}
}
