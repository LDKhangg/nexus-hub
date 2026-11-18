package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// R-A-4 (REQ-2, budget guard): BeforeAttempt runs only before repair attempts and its error stops the call.
func TestBeforeAttempt_StopsRepairWithUsageSoFar(t *testing.T) {
	stop := errors.New("wall time spent")
	f := NewFakeRuntime().Script("classify", rawReply(`{}`, 10, 1), rawReply(`{}`, 20, 2), goodClassify(t))
	var seen []Usage
	v, err := generateChecked(context.Background(), f, StructuredRequest{Prompt: "p"}, SchemaClassify, 2, nil, func(u Usage) error {
		seen = append(seen, u)
		return stop
	})
	mustErrIs(t, err, stop)
	if len(f.Requests()) != 1 || v.Attempts != 1 {
		t.Fatalf("requests=%d attempts=%d; guard must stop the repair", len(f.Requests()), v.Attempts)
	}
	if len(seen) != 1 || seen[0].Tokens.Input != 10 || seen[0].Tokens.Output != 1 {
		t.Fatalf("hook saw %+v, want usage of the first attempt", seen)
	}
	if v.Usage.Input != 10 || v.Usage.Output != 1 {
		t.Fatalf("usage so far lost: %+v", v.Usage)
	}
}

func TestBeforeAttempt_AllowsSecondAttemptThenStops(t *testing.T) {
	stop := errors.New("budget")
	f := NewFakeRuntime().Script("classify", rawReply(`{}`, 10, 1), rawReply(`{}`, 20, 2), goodClassify(t))
	calls := 0
	v, err := generateChecked(context.Background(), f, StructuredRequest{Prompt: "p"}, SchemaClassify, 3, nil, func(u Usage) error {
		calls++
		if calls == 2 {
			return stop
		}
		return nil
	})
	mustErrIs(t, err, stop)
	if len(f.Requests()) != 2 || calls != 2 {
		t.Fatalf("requests=%d hook calls=%d", len(f.Requests()), calls)
	}
	if v.Usage.Input != 30 || v.Usage.Output != 3 {
		t.Fatalf("usage %+v", v.Usage)
	}
}

func TestBeforeAttempt_NotCalledOnFirstAttempt(t *testing.T) {
	f := NewFakeRuntime().Script("classify", goodClassify(t))
	called := false
	v, err := generateChecked(context.Background(), f, StructuredRequest{Prompt: "p"}, SchemaClassify, 2, nil, func(Usage) error {
		called = true
		return errors.New("must not run")
	})
	if err != nil || called || v.Attempts != 1 {
		t.Fatalf("err=%v called=%v attempts=%d", err, called, v.Attempts)
	}
}

func TestBeforeAttempt_NilHookUnchanged(t *testing.T) {
	f := NewFakeRuntime().Script("classify", rawReply(`{}`, 10, 5), goodClassify(t))
	v, err := generateChecked(context.Background(), f, StructuredRequest{Prompt: "p"}, SchemaClassify, 1, nil, nil)
	if err != nil || v.Attempts != 2 || v.Usage.Input != 20 {
		t.Fatalf("err=%v %+v", err, v)
	}
}

// R-A-4 through Plan: the Orchestrator field reaches the repair loop and the usage is returned.
func TestPlan_BeforeAttemptStopsRepair(t *testing.T) {
	stop := errors.New("budget spent")
	f := NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE", okTask("a", "ghost")), 7, 3), reply(t, planJSON("EXECUTE", okTask("a")), 1, 1))
	o := newOrch(f)
	o.MaxRepair = 2
	var got Usage
	o.BeforeAttempt = func(u Usage) error { got = u; return stop }
	_, u, err := runPlan(t, o, planIn(nil))
	mustErrIs(t, err, stop)
	if len(f.Requests()) != 1 {
		t.Fatalf("requests=%d", len(f.Requests()))
	}
	if got.Tokens.Input != 7 || u.Tokens.Input != 7 {
		t.Fatalf("hook usage %+v returned %+v", got, u)
	}
}

// FeedbackBounded (REQ-4): whole-value messages are kept, then at most 10 item messages, then one count line.
func TestCheckErr_WholeValueKeptItemsBounded(t *testing.T) {
	var errs []string
	for i := 0; i < 25; i++ {
		errs = append(errs, "$.tasks["+itoa(i)+"].capabilities[0]: unknown capability; use only the known capabilities")
	}
	errs = append(errs, "$.tasks: allows at most 5 items")
	err := checkErr(errs)
	if err == nil {
		t.Fatal("nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "$.tasks: allows at most 5 items") {
		t.Fatalf("whole-value message dropped: %s", msg)
	}
	if n := strings.Count(msg, "unknown capability"); n != 10 {
		t.Fatalf("%d item messages, want 10", n)
	}
	if !strings.Contains(msg, "$: 15 more item violations not listed") {
		t.Fatalf("overflow line missing: %s", msg)
	}
}

func TestCheckErr_NoOverflowLineAtLimit(t *testing.T) {
	var errs []string
	for i := 0; i < 10; i++ {
		errs = append(errs, "$.tasks["+itoa(i)+"].title: must not be empty")
	}
	msg := checkErr(errs).Error()
	if strings.Contains(msg, "more item violations") || strings.Count(msg, "must not be empty") != 10 {
		t.Fatalf("%s", msg)
	}
	if checkErr(nil) != nil {
		t.Fatal("empty list must be nil")
	}
}

// Through Plan: too many tasks plus many bad items; the repair prompt still names the whole-value rule.
func TestPlan_FeedbackKeepsWholeValueRule(t *testing.T) {
	var tasks []map[string]any
	for i := 0; i < 20; i++ {
		tasks = append(tasks, ptask("k"+string(rune('a'+i)), "T", []string{"nope.cap"}))
	}
	f := NewFakeRuntime().Script("plan", reply(t, planJSON("EXECUTE", tasks...), 1, 1), reply(t, planJSON("EXECUTE", okTask("a")), 1, 1))
	o := newOrch(f)
	o.MaxRepair = 1
	if _, _, err := runPlan(t, o, planIn(nil)); err != nil {
		t.Fatal(err)
	}
	fb := f.Requests()[1].Prompt
	if !strings.Contains(fb, "allows at most 5 items") {
		t.Fatalf("whole-value rule missing from feedback")
	}
	if !strings.Contains(fb, "more item violations not listed") {
		t.Fatalf("overflow line missing from feedback")
	}
	if strings.Contains(fb, "nope.cap") {
		t.Fatal("feedback echoed a model value")
	}
}
