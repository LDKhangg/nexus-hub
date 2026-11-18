package orchestrator

import (
	"context"
	"strings"
	"testing"
)

func evalPlan() Plan {
	return Plan{Tasks: []PlanTask{
		{Key: "a", Title: "First", Capabilities: []string{CapRepoRead}},
		{Key: "b", Title: "Second", Capabilities: []string{CapRepoRead}, DependsOn: []string{"a"}},
	}, Order: []string{"a", "b"}}
}

func evalIn() EvaluateInput {
	return EvaluateInput{Plan: evalPlan(), TaskKey: "a", Result: TaskResult{
		Outcome: "succeeded", ReasonCode: "ok", Summary: "done",
		Evidence: []EvidenceRef{{Kind: EvidenceStepOutput, ID: "s1"}, {Kind: EvidenceArtifact, ID: "pr-7"}},
	}}
}

func evalOut(decision string, refs ...string) map[string]any {
	d := m("decision", decision, "reason_code", "because")
	if refs != nil {
		d["evidence_refs"] = refs
	}
	return d
}

// TC-E1 (REQ-6): all 7 decisions are accepted.
func TestEvaluate_AllDecisionsAccepted(t *testing.T) {
	for _, d := range Decisions {
		f := NewFakeRuntime().Script("evaluate", reply(t, evalOut(d, "s1"), 12, 3))
		dec, u, err := newOrch(f).Evaluate(context.Background(), evalIn())
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if dec.Decision != d || dec.ReasonCode != "because" || len(dec.EvidenceRefs) != 1 || u.Tokens.Input != 12 {
			t.Fatalf("%s: %+v %+v", d, dec, u)
		}
	}
	if len(Decisions) != 7 {
		t.Fatalf("decision set has %d entries", len(Decisions))
	}
}

// TC-E2 (REQ-6, abuse): evidence refs must be a subset of the input refs.
func TestEvaluate_EvidenceSubset(t *testing.T) {
	f := NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "s1", "pr-7"), 1, 1))
	dec, _, err := newOrch(f).Evaluate(context.Background(), evalIn())
	if err != nil || len(dec.EvidenceRefs) != 2 {
		t.Fatalf("%+v %v", dec, err)
	}
	// An invented ref is rejected with bound 0 ...
	f = NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "s1", "invented-9"), 9, 2))
	o := newOrch(f)
	o.MaxRepair = 0
	_, u, err := o.Evaluate(context.Background(), evalIn())
	mustErrIs(t, err, ErrInvalidOutput)
	if u.Tokens.Input != 9 {
		t.Fatalf("usage on error %+v", u)
	}
	// ... and repaired with bound 1, without echoing the invented id.
	f = NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "invented-9"), 1, 1), reply(t, evalOut("COMPLETE", "s1"), 1, 1))
	o = newOrch(f)
	o.MaxRepair = 1
	dec, u, err = o.Evaluate(context.Background(), evalIn())
	if err != nil || u.Attempts != 2 || dec.EvidenceRefs[0] != "s1" {
		t.Fatalf("%+v %+v %v", dec, u, err)
	}
	fb := f.Requests()[1].Prompt
	if !strings.Contains(fb, "$.evidence_refs[0]") {
		t.Fatalf("feedback %s", fb)
	}
	requireNoLeak(t, fb, "invented-9")
}

// TC-E3 (REQ-6): a decision with no refs when none exist, and refs when the result has none.
func TestEvaluate_NoEvidenceInput(t *testing.T) {
	in := evalIn()
	in.Result.Evidence = nil
	f := NewFakeRuntime().Script("evaluate", reply(t, evalOut("FAIL"), 1, 1))
	dec, _, err := newOrch(f).Evaluate(context.Background(), in)
	if err != nil || len(dec.EvidenceRefs) != 0 {
		t.Fatalf("%+v %v", dec, err)
	}
	f = NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "s1"), 1, 1))
	o := newOrch(f)
	o.MaxRepair = 0
	_, _, err = o.Evaluate(context.Background(), in)
	mustErrIs(t, err, ErrInvalidOutput)
}

// TC-E4 (REQ-6, human): refs match exactly after trimming; blank, duplicate and over-long ids are not offered.
func TestEvaluate_EvidenceInputNormalised(t *testing.T) {
	in := evalIn()
	in.Result.Evidence = []EvidenceRef{{ID: "  s1  "}, {ID: "s1"}, {ID: ""}, {ID: "   "}, {ID: strings.Repeat("x", 129)}}
	f := NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "s1", "s1"), 1, 1))
	dec, _, err := newOrch(f).Evaluate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.EvidenceRefs) != 1 {
		t.Fatalf("refs not de-duplicated: %v", dec.EvidenceRefs)
	}
	if strings.Contains(lastPrompt(t, f), strings.Repeat("x", 129)) {
		t.Fatal("over-long evidence id reached the prompt")
	}
	// Case matters.
	f = NewFakeRuntime().Script("evaluate", reply(t, evalOut("COMPLETE", "S1"), 1, 1))
	o := newOrch(f)
	o.MaxRepair = 0
	_, _, err = o.Evaluate(context.Background(), in)
	mustErrIs(t, err, ErrInvalidOutput)
}

// TC-E5 (REQ-6): unknown task key in input is an invalid input; in output it is invalid output.
func TestEvaluate_TaskKeys(t *testing.T) {
	for _, key := range []string{"zzz", "", "A"} {
		f := NewFakeRuntime()
		in := evalIn()
		in.TaskKey = key
		_, u, err := newOrch(f).Evaluate(context.Background(), in)
		mustErrIs(t, err, ErrInvalidInput)
		if len(f.Requests()) != 0 || u.Attempts != 0 {
			t.Fatalf("model called for key %q", key)
		}
	}
	out := evalOut("EXECUTE", "s1")
	out["task_key"] = "b"
	f := NewFakeRuntime().Script("evaluate", reply(t, out, 1, 1))
	dec, _, err := newOrch(f).Evaluate(context.Background(), evalIn())
	if err != nil || dec.TaskKey != "b" {
		t.Fatalf("%+v %v", dec, err)
	}
	out["task_key"] = "ghost"
	f = NewFakeRuntime().Script("evaluate", reply(t, out, 1, 1))
	o := newOrch(f)
	o.MaxRepair = 0
	_, _, err = o.Evaluate(context.Background(), evalIn())
	mustErrIs(t, err, ErrInvalidOutput)
}

// TC-E6 (REQ-6): reason code and next action rules.
func TestEvaluate_ReasonAndNextAction(t *testing.T) {
	for _, bad := range []string{`"Done"`, `"has space"`, `""`, `"1x"`} {
		f := NewFakeRuntime().Script("evaluate", rawReply(`{"decision":"FAIL","reason_code":`+bad+`}`, 1, 1))
		o := newOrch(f)
		o.MaxRepair = 0
		_, _, err := o.Evaluate(context.Background(), evalIn())
		mustErrIs(t, err, ErrInvalidOutput)
	}
	out := evalOut("EXECUTE")
	out["next_action"] = "  run\n\nthe   tests\t now "
	f := NewFakeRuntime().Script("evaluate", reply(t, out, 1, 1))
	dec, _, err := newOrch(f).Evaluate(context.Background(), evalIn())
	if err != nil || dec.NextAction != "run the tests now" {
		t.Fatalf("%q %v", dec.NextAction, err)
	}
	out["next_action"] = strings.Repeat("x", 301)
	f = NewFakeRuntime().Script("evaluate", reply(t, out, 1, 1))
	o := newOrch(f)
	o.MaxRepair = 0
	_, _, err = o.Evaluate(context.Background(), evalIn())
	mustErrIs(t, err, ErrInvalidOutput)
}

func TestEvaluate_RuntimeErrorAndNilRuntime(t *testing.T) {
	f := NewFakeRuntime().Script("evaluate", FakeReply{Err: errBoom})
	_, _, err := newOrch(f).Evaluate(context.Background(), evalIn())
	mustErrIs(t, err, errBoom)
	if _, _, err := (&Orchestrator{}).Evaluate(context.Background(), evalIn()); err == nil {
		t.Fatal("missing runtime accepted")
	}
}
