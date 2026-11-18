package orchestrator

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Phase 9 Fix B regressions: D-B-1 (Plan.Task returns a copy) and the
// evidence handed to a github.merge task (R-B-3, Proofs).

// TC-FB-D1 (D-B-1): changing what Plan.Task returned never changes the plan.
func TestP9FixB_PlanTaskReturnsACopy(t *testing.T) {
	p := Plan{Tasks: []PlanTask{{
		Key: "land", Capabilities: []string{CapGitHubMerge}, DependsOn: []string{"fix"},
		Merge: &MergeTarget{Repo: "acme/api", Number: 12, HeadSHA: p9Head},
	}}}
	got, ok := p.Task("land")
	if !ok {
		t.Fatal("task not found")
	}
	got.Capabilities[0] = "repo.read"
	got.DependsOn[0] = "other"
	got.Merge.Number = 99
	got.Merge.HeadSHA = p9Head2
	got.Merge.Repo = "evil/repo"
	again, _ := p.Task("land")
	if again.Capabilities[0] != CapGitHubMerge || again.DependsOn[0] != "fix" {
		t.Fatalf("slices changed: %+v", again)
	}
	if *again.Merge != (MergeTarget{Repo: "acme/api", Number: 12, HeadSHA: p9Head}) {
		t.Fatalf("merge target changed: %+v", *again.Merge)
	}
	if p.Tasks[0].Merge.Number != 12 || p.Tasks[0].Capabilities[0] != CapGitHubMerge {
		t.Fatal("the plan itself changed")
	}
	// A task without a target stays without one.
	q := Plan{Tasks: []PlanTask{{Key: "fix"}}}
	if got, _ := q.Task("fix"); got.Merge != nil {
		t.Fatalf("a target appeared: %+v", got.Merge)
	}
}

// TC-FB-R3 (R-B-3): the proofs of a task are the other succeeded tasks' evidence refs and stored result fields, one entry per task.
func TestP9FixB_TaskProofs(t *testing.T) {
	res := func(v map[string]string) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	r := &stepRun{tasks: []PlanTaskRecord{
		{Key: "fix", State: TaskSucceeded, EvidenceRefs: []string{"github:pr:acme/api#12"}, Result: res(map[string]string{"branch": "nexus/fix", "pr_url": "https://github.com/acme/api/pull/12", "summary": "head " + p9Head, "ignored": "x"})},
		{Key: "failed", State: "FAILED", EvidenceRefs: []string{"github:pr:acme/api#77"}},
		{Key: "pending", State: "PENDING"},
		{Key: "empty", State: TaskSucceeded},
		{Key: "land", State: TaskSucceeded, EvidenceRefs: []string{"self"}},
	}}
	got := r.taskProofs(PlanTaskRecord{Key: "land"})
	if len(got) != 1 {
		t.Fatalf("want one entry (only the succeeded task with evidence, not the task itself), got %q", got)
	}
	for _, want := range []string{"github:pr:acme/api#12", "nexus/fix", "https://github.com/acme/api/pull/12", p9Head} {
		if !strings.Contains(got[0], want) {
			t.Errorf("proof lacks %q: %q", want, got[0])
		}
	}
	if strings.Contains(got[0], "ignored") || strings.Contains(got[0], "#77") || strings.Contains(got[0], "self") {
		t.Fatalf("proof carries what it must not: %q", got[0])
	}
	// Malformed stored results do not panic and still give the refs.
	bad := &stepRun{tasks: []PlanTaskRecord{{Key: "x", State: TaskSucceeded, EvidenceRefs: []string{"ref-1"}, Result: json.RawMessage(`[1,2]`)}}}
	if got := bad.taskProofs(PlanTaskRecord{Key: "land"}); !slices.Equal(got, []string{"ref-1"}) {
		t.Fatalf("%q", got)
	}
}
