package orchestrator

import (
	"context"
	"math"
	"strings"
	"testing"
)

// TC-C1 (REQ-3): valid output is returned with the threshold of the call.
func TestClassify_ValidOutput(t *testing.T) {
	f := NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "bug_fix", 0.92), 100, 20))
	o := newOrch(f)
	c, u, err := o.Classify(context.Background(), ClassifyInput{Request: "sửa lỗi đăng nhập", Source: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Intent != "task" || c.TaskType != "bug_fix" || c.Priority != "normal" || c.Complexity != "m" || c.Confidence != 0.92 {
		t.Fatalf("classification %+v", c)
	}
	if c.Threshold != DefaultMinConfidence || c.NeedsClarification() {
		t.Fatalf("threshold %v", c.Threshold)
	}
	if u.Attempts != 1 || u.Tokens.Input != 100 || u.Tokens.Output != 20 {
		t.Fatalf("usage %+v", u)
	}
	r := f.Requests()[0]
	if r.Model != "test-model" || r.SchemaName != "classify" || r.System != classifySystem {
		t.Fatalf("request %+v", r)
	}
}

// TC-C2 (REQ-3): labels outside the vocabulary are invalid output, repaired within the bound.
func TestClassify_VocabularyEnforced(t *testing.T) {
	f := NewFakeRuntime().Script("classify",
		reply(t, classifyJSON("banana", "bug_fix", 0.9), 10, 1),
		reply(t, classifyJSON("task", "bug_fix", 0.9), 10, 1))
	o := newOrch(f)
	c, u, err := o.Classify(context.Background(), ClassifyInput{Request: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Intent != "task" || u.Attempts != 2 || u.Tokens.Input != 20 {
		t.Fatalf("c=%+v u=%+v", c, u)
	}
	if !strings.Contains(f.Requests()[1].Prompt, "$.intent: must be one of: question, task") {
		t.Fatalf("repair feedback: %q", f.Requests()[1].Prompt)
	}
	// Bound 0: no repair, usage still returned.
	f = NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "whatever", 0.9), 7, 3))
	o = newOrch(f)
	o.MaxRepair = 0
	_, u, err = o.Classify(context.Background(), ClassifyInput{Request: "x"})
	mustErrIs(t, err, ErrInvalidOutput)
	if u.Tokens.Input != 7 || u.Attempts != 1 {
		t.Fatalf("usage on error %+v", u)
	}
}

// TC-C3 (REQ-3): configured vocabularies replace the defaults; bad labels are dropped.
func TestClassify_ConfiguredVocabulary(t *testing.T) {
	f := NewFakeRuntime().Script("classify", reply(t, classifyJSON("deploy", "ops", 0.9), 1, 1))
	o := newOrch(f)
	o.Intents = []string{"Bad Label", "deploy", "deploy", " "}
	o.TaskTypes = []string{"ops"}
	c, _, err := o.Classify(context.Background(), ClassifyInput{Request: "x"})
	if err != nil || c.Intent != "deploy" {
		t.Fatalf("c=%+v err=%v", c, err)
	}
	if strings.Contains(f.Requests()[0].Prompt, "Bad Label") {
		t.Fatal("invalid label reached the prompt")
	}
	// "task" is no longer allowed.
	f = NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "ops", 0.9), 1, 1))
	o = newOrch(f)
	o.MaxRepair = 0
	o.Intents = []string{"deploy"}
	_, _, err = o.Classify(context.Background(), ClassifyInput{Request: "x"})
	mustErrIs(t, err, ErrInvalidOutput)
	// All labels invalid: defaults.
	f = NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "bug_fix", 0.9), 1, 1))
	o = newOrch(f)
	o.Intents = []string{"BAD", "x y"}
	if _, _, err = o.Classify(context.Background(), ClassifyInput{Request: "x"}); err != nil {
		t.Fatal(err)
	}
}

// TC-C4 (REQ-3): threshold fallback and clarification rule.
func TestClassify_ConfidenceThreshold(t *testing.T) {
	cases := []struct {
		name       string
		min        float64
		conf       float64
		wantThresh float64
		wantAsk    bool
	}{
		{"default below", 0.6, 0.59, 0.6, true},
		{"default equal", 0.6, 0.6, 0.6, false},
		{"default above", 0.6, 0.61, 0.6, false},
		{"NaN falls back", math.NaN(), 0.55, 0.6, true},
		{"NaN falls back ok", math.NaN(), 0.65, 0.6, false},
		{"zero falls back", 0, 0.5, 0.6, true},
		{"negative falls back", -1, 0.5, 0.6, true},
		{"above one falls back", 1.5, 0.7, 0.6, false},
		{"+Inf falls back", math.Inf(1), 0.7, 0.6, false},
		{"custom strict", 0.9, 0.8, 0.9, true},
		{"one is valid", 1, 0.99, 1, true},
		{"one accepts one", 1, 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "bug_fix", tc.conf), 1, 1))
			o := newOrch(f)
			o.MinConfidence = tc.min
			c, _, err := o.Classify(context.Background(), ClassifyInput{Request: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if c.Threshold != tc.wantThresh || c.NeedsClarification() != tc.wantAsk {
				t.Fatalf("threshold=%v ask=%v", c.Threshold, c.NeedsClarification())
			}
		})
	}
}

// TC-C5 (REQ-3, abuse): model confidence outside 0..1 is invalid output, not trusted.
func TestClassify_ConfidenceOutOfRangeRejected(t *testing.T) {
	for _, conf := range []string{"1.5", "-0.2", `"high"`, "null"} {
		doc := `{"intent":"task","task_type":"bug_fix","priority":"low","complexity":"m","confidence":` + conf + `}`
		f := NewFakeRuntime().Script("classify", rawReply(doc, 1, 1))
		o := newOrch(f)
		o.MaxRepair = 0
		_, _, err := o.Classify(context.Background(), ClassifyInput{Request: "x"})
		mustErrIs(t, err, ErrInvalidOutput)
	}
}

// TC-C6 (REQ-3, human): empty input makes no model call.
func TestClassify_EmptyRequestNoModelCall(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		f := NewFakeRuntime()
		_, u, err := newOrch(f).Classify(context.Background(), ClassifyInput{Request: in})
		mustErrIs(t, err, ErrInvalidInput)
		if len(f.Requests()) != 0 || u.Attempts != 0 {
			t.Fatal("model called for empty request")
		}
	}
	var o *Orchestrator
	if _, _, err := o.Classify(context.Background(), ClassifyInput{Request: "x"}); err == nil {
		t.Fatal("nil orchestrator accepted")
	}
	if _, _, err := (&Orchestrator{}).Classify(context.Background(), ClassifyInput{Request: "x"}); err == nil {
		t.Fatal("missing runtime accepted")
	}
}

// TC-C7 (REQ-3): runtime error is returned without repair.
func TestClassify_RuntimeErrorNotRepaired(t *testing.T) {
	f := NewFakeRuntime().Script("classify", FakeReply{Err: errBoom}, reply(t, classifyJSON("task", "bug_fix", 0.9), 1, 1))
	_, _, err := newOrch(f).Classify(context.Background(), ClassifyInput{Request: "x"})
	mustErrIs(t, err, errBoom)
	if len(f.Requests()) != 1 {
		t.Fatal("runtime error was retried")
	}
}

// TC-C8 (REQ-3, human): long Vietnamese text with emoji is bounded; bad source label dropped; negative MaxRepair is 0.
func TestClassify_PromptBounds(t *testing.T) {
	f := NewFakeRuntime().Script("classify", reply(t, classifyJSON("task", "bug_fix", 0.9), 1, 1))
	long := strings.Repeat("Sửa lỗi 🐛 ", 5000)
	_, _, err := newOrch(f).Classify(context.Background(), ClassifyInput{Request: long, Source: "tele gram; DROP"})
	if err != nil {
		t.Fatal(err)
	}
	p := lastPrompt(t, f)
	if len([]rune(p)) > 8000*2+2000 {
		t.Fatalf("prompt not bounded: %d runes", len([]rune(p)))
	}
	if strings.Contains(p, "DROP") {
		t.Fatal("untrusted source label reached prompt")
	}
	if !strings.Contains(p, "Sửa lỗi") {
		t.Fatal("Vietnamese text mangled")
	}
	f = NewFakeRuntime().Script("classify", rawReply(`{}`, 1, 1), reply(t, classifyJSON("task", "bug_fix", 0.9), 1, 1))
	o := newOrch(f)
	o.MaxRepair = -3
	_, _, err = o.Classify(context.Background(), ClassifyInput{Request: "x"})
	mustErrIs(t, err, ErrInvalidOutput)
	if len(f.Requests()) != 1 {
		t.Fatal("negative MaxRepair must mean no repair")
	}
}

func TestNewOrchestrator_Defaults(t *testing.T) {
	o := NewOrchestrator(NewFakeRuntime(), "m1")
	if o.MaxRepair != DefaultMaxRepair || o.MinConfidence != DefaultMinConfidence ||
		o.ClassifyModel != "m1" || o.PlanModel != "m1" || o.EvaluateModel != "m1" {
		t.Fatalf("%+v", o)
	}
}

// Per-operation model names reach the runtime.
func TestOrchestrator_ModelPerOperation(t *testing.T) {
	f := NewFakeRuntime().
		Script("classify", reply(t, classifyJSON("task", "bug_fix", 0.9), 1, 1)).
		Script("plan", reply(t, planJSON("EXECUTE", ptask("a", "T", []string{CapRepoRead})), 1, 1)).
		Script("evaluate", reply(t, m("decision", "COMPLETE", "reason_code", "ok"), 1, 1))
	o := newOrch(f)
	o.ClassifyModel, o.PlanModel, o.EvaluateModel = "mc", "mp", "me"
	ctx := context.Background()
	if _, _, err := o.Classify(ctx, ClassifyInput{Request: "x"}); err != nil {
		t.Fatal(err)
	}
	p, _, err := o.Plan(ctx, PlanInput{Request: "x", Capabilities: planCaps, Caps: BudgetRequest{1, 1, 1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Evaluate(ctx, EvaluateInput{Plan: p, TaskKey: "a"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range f.Requests() {
		got = append(got, r.Model)
	}
	if strings.Join(got, ",") != "mc,mp,me" {
		t.Fatalf("models %v", got)
	}
}
