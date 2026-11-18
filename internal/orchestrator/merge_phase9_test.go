package orchestrator

import (
	"strings"
	"testing"
)

// Phase 9 Batch B: plan validation of github.merge tasks (REQ-9) and the
// capability registry's view of the new capability.

const (
	p9Head  = "0123456789abcdef0123456789abcdef01234567"
	p9Head2 = "fedcba9876543210fedcba9876543210fedcba98"
)

func p9MergeTask(key string, merge any, caps ...string) map[string]any {
	if len(caps) == 0 {
		caps = []string{CapGitHubMerge}
	}
	t := ptask(key, "Merge "+key, caps)
	if merge != nil {
		t["merge"] = merge
	}
	return t
}

func p9Target(repo string, number int, sha string) map[string]any {
	return m("repo", repo, "number", number, "head_sha", sha)
}

func p9PlanIn() PlanInput {
	return planIn(func(in *PlanInput) { in.Capabilities = append(append([]string{}, planCaps...), CapGitHubMerge) })
}

// TC-MG1 (REQ-9): ValidMergeTarget accepts one repo, a positive PR number and a full lowercase 40-hex SHA only.
func TestP9ValidMergeTarget(t *testing.T) {
	good := MergeTarget{Repo: "acme/api", Number: 12, HeadSHA: p9Head}
	if !ValidMergeTarget(&good) {
		t.Fatal("a valid target was refused")
	}
	bad := map[string]*MergeTarget{
		"nil":                nil,
		"empty repo":         {Repo: "", Number: 12, HeadSHA: p9Head},
		"no owner":           {Repo: "api", Number: 12, HeadSHA: p9Head},
		"two slashes":        {Repo: "a/b/c", Number: 12, HeadSHA: p9Head},
		"dotdot":             {Repo: "acme/..", Number: 12, HeadSHA: p9Head},
		"dotdot inside":      {Repo: "acme/a..b", Number: 12, HeadSHA: p9Head},
		"space":              {Repo: "acme/a b", Number: 12, HeadSHA: p9Head},
		"url":                {Repo: "https://github.com/acme/api", Number: 12, HeadSHA: p9Head},
		"option-like owner":  {Repo: "-x/api", Number: 12, HeadSHA: p9Head},
		"number 0":           {Repo: "acme/api", Number: 0, HeadSHA: p9Head},
		"negative number":    {Repo: "acme/api", Number: -3, HeadSHA: p9Head},
		"number over max":    {Repo: "acme/api", Number: MaxPRNumber + 1, HeadSHA: p9Head},
		"short sha":          {Repo: "acme/api", Number: 12, HeadSHA: "0123456"},
		"39 hex":             {Repo: "acme/api", Number: 12, HeadSHA: p9Head[:39]},
		"41 hex":             {Repo: "acme/api", Number: 12, HeadSHA: p9Head + "a"},
		"non hex":            {Repo: "acme/api", Number: 12, HeadSHA: strings.Repeat("g", 40)},
		"uppercase sha":      {Repo: "acme/api", Number: 12, HeadSHA: strings.ToUpper(p9Head)},
		"sha with newline":   {Repo: "acme/api", Number: 12, HeadSHA: p9Head[:39] + "\n"},
		"branch name as sha": {Repo: "acme/api", Number: 12, HeadSHA: "main"},
	}
	for name, mt := range bad {
		if ValidMergeTarget(mt) {
			t.Errorf("%s: accepted %+v", name, mt)
		}
	}
	if !ValidMergeTarget(&MergeTarget{Repo: "acme/api", Number: MaxPRNumber, HeadSHA: p9Head}) {
		t.Error("the largest PR number was refused")
	}
}

// TC-MG2 (REQ-9): a valid github.merge task passes Plan and keeps its target.
func TestP9Plan_MergeTaskAccepted(t *testing.T) {
	f := NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE",
		ptask("fix", "Fix it", []string{CapRepoWrite}),
		func() map[string]any {
			x := p9MergeTask("land", p9Target("acme/api", 12, p9Head))
			x["depends_on"] = []string{"fix"}
			return x
		}()), 10, 2))
	o := newOrch(f)
	o.MaxRepair = 0
	p, _, err := runPlan(t, o, p9PlanIn())
	if err != nil {
		t.Fatal(err)
	}
	land, ok := p.Task("land")
	if !ok || land.Merge == nil || *land.Merge != (MergeTarget{Repo: "acme/api", Number: 12, HeadSHA: p9Head}) {
		t.Fatalf("task %+v", land)
	}
	if fix, _ := p.Task("fix"); fix.Merge != nil {
		t.Fatalf("a non-merge task got a target: %+v", fix.Merge)
	}
	// The stored plan task is a copy: changing it does not change the plan.
	land.Merge.Number = 99
	if again, _ := p.Task("land"); again.Merge.Number != 12 {
		t.Fatal("Task exposes the plan's merge target")
	}
}

// TC-MG3 (REQ-9, abuse): every malformed merge task is invalid output (repair), never accepted.
func TestP9Plan_MergeTaskValidation(t *testing.T) {
	cases := []struct {
		name string
		bad  map[string]any
	}{
		{"bad repo (path trick)", p9MergeTask("a", p9Target("acme/../etc", 12, p9Head))},
		{"bad repo (url)", p9MergeTask("a", p9Target("https://x.io/a/b", 12, p9Head))},
		{"number 0", p9MergeTask("a", p9Target("acme/api", 0, p9Head))},
		{"negative number", p9MergeTask("a", p9Target("acme/api", -1, p9Head))},
		{"number over max", p9MergeTask("a", p9Target("acme/api", MaxPRNumber+1, p9Head))},
		{"short sha", p9MergeTask("a", p9Target("acme/api", 12, "abc1234"))},
		{"non-hex sha", p9MergeTask("a", p9Target("acme/api", 12, strings.Repeat("z", 40)))},
		{"uppercase sha", p9MergeTask("a", p9Target("acme/api", 12, strings.ToUpper(p9Head)))},
		{"missing merge field", p9MergeTask("a", nil)},
		{"extra capability", p9MergeTask("a", p9Target("acme/api", 12, p9Head), CapGitHubMerge, CapRepoRead)},
		{"merge field without the capability", func() map[string]any {
			x := ptask("a", "Read only", []string{CapRepoRead})
			x["merge"] = p9Target("acme/api", 12, p9Head)
			return x
		}()},
		{"unknown field in merge", p9MergeTask("a", m("repo", "acme/api", "number", 12, "head_sha", p9Head, "force", true))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE", tc.bad), 30, 4))
			o := newOrch(f)
			o.MaxRepair = 0
			_, u, err := runPlan(t, o, p9PlanIn())
			mustErrIs(t, err, ErrInvalidOutput)
			if u.Attempts != 1 {
				t.Fatalf("usage %+v", u)
			}
			// With one repair the model gets feedback naming the merge rule, not the offending value.
			good := p9MergeTask("a", p9Target("acme/api", 12, p9Head))
			f = NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE", tc.bad), 30, 4), reply(t, planJSON("EXECUTE", good), 40, 5))
			o = newOrch(f)
			o.MaxRepair = 1
			p, _, err := runPlan(t, o, p9PlanIn())
			if err != nil {
				t.Fatal(err)
			}
			if tk, _ := p.Task("a"); tk.Merge == nil || tk.Merge.HeadSHA != p9Head {
				t.Fatalf("repaired plan %+v", p)
			}
			fb := f.Requests()[1].Prompt
			if !strings.Contains(fb, "merge") && !strings.Contains(fb, "capabilit") {
				t.Fatalf("feedback does not name the rule: %s", fb)
			}
			requireNoLeak(t, fb, "acme/../etc", "https://x.io/a/b", strings.Repeat("z", 40))
		})
	}
}

// TC-MG4 (REQ-9): a plan may use github.merge only when it is among the offered capabilities.
func TestP9Plan_MergeNotOfferedIsUnknownCapability(t *testing.T) {
	f := NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE", p9MergeTask("a", p9Target("acme/api", 12, p9Head))), 5, 1))
	o := newOrch(f)
	o.MaxRepair = 0
	_, _, err := runPlan(t, o, planIn(nil)) // planCaps has no github.merge
	mustErrIs(t, err, ErrInvalidOutput)
}

// TC-MG5 (REQ-9): the registry knows github.merge, matches it to behavior-actions only, and never mixes it with other capabilities.
func TestP9Registry_MergeCapability(t *testing.T) {
	r := NewCapabilityRegistry()
	if name, ok := r.Match([]string{CapGitHubMerge}, ""); !ok || name != ExecutorBehaviorActions {
		t.Fatalf("match %q %v", name, ok)
	}
	// A recommendation of a more privileged executor is not honoured.
	if name, _ := r.Match([]string{CapGitHubMerge}, ExecutorLocalWorker); name != ExecutorBehaviorActions {
		t.Fatalf("recommendation raised the executor to %q", name)
	}
	for _, mix := range [][]string{{CapGitHubMerge, CapRepoRead}, {CapGitHubMerge, CapRepoWrite}, {CapGitHubMerge, CapGitHubRead}} {
		if name, ok := r.Match(mix, ""); ok {
			t.Errorf("%v matched %q: a merge must not share an executor with other capabilities", mix, name)
		}
	}
	// An executor profile that is not wired hides the capability.
	caps := r.CapabilitiesFor(func(p ExecutorProfile) bool { return p.Name != ExecutorBehaviorActions })
	for _, c := range caps {
		if c == CapGitHubMerge {
			t.Fatalf("offered without an executor: %v", caps)
		}
	}
}
