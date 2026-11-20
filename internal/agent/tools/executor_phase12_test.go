package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// restart builds a new executor for the same job over the same store, like a
// hub that crashed and resumed the job.
func restart(e env) *Executor {
	return NewExecutor(e.exec.reg, policy(), DefaultLimits(), e.store, upperSan{}, 42, "ws-1", execOpts(e.gate)...)
}

// TC-9 (REQ-3): a finished post (write tier) and a finished merge (dangerous
// tier, allowed) are not repeated when the job replays after a crash; the
// replay answers from the stored result, also when the input is reordered or
// padded with spaces.
func TestPhase12ReplayDoesNotRepeatFinishedWriteOrMerge(t *testing.T) {
	e := newEnv(t, policy(), true, Limits{})
	post := json.RawMessage(`{"repo":"o/r","task":"86abc","text":"hello"}`)
	merge := json.RawMessage(`{"repo":"o/r","pr":7}`)
	if r := e.exec.Call(context.Background(), 1, "write_a", post); r.IsError || r.Status != "ok" {
		t.Fatalf("post: %+v", r)
	}
	if r := e.exec.Call(context.Background(), 2, "danger_a", merge); r.IsError || r.Status != "ok" {
		t.Fatalf("merge: %+v", r)
	}
	again := restart(e)
	if r := again.Call(context.Background(), 1, "write_a", json.RawMessage(`{ "text": "hello", "task": "86abc", "repo": "o/r" }`)); r.IsError || r.Status != "ok" {
		t.Fatalf("replayed post: %+v", r)
	}
	if r := again.Call(context.Background(), 2, "danger_a", json.RawMessage(`{"pr":7,"repo":"o/r"}`)); r.IsError || r.Status != "ok" {
		t.Fatalf("replayed merge: %+v", r)
	}
	if n := e.tools["write_a"].count(); n != 1 {
		t.Fatalf("post ran %d times, want 1", n)
	}
	if n := e.tools["danger_a"].count(); n != 1 {
		t.Fatalf("merge ran %d times, want 1", n)
	}
}

// TC-9 (REQ-3): a post or merge that started but never recorded its end (the
// crash was inside the call) is not run again: the replay reports an unknown
// outcome and a human decides.
func TestPhase12ReplayAfterCrashInsideCallDoesNotRunAgain(t *testing.T) {
	for _, name := range []string{"write_a", "danger_a"} {
		e := newEnv(t, policy(), true, Limits{})
		in := json.RawMessage(`{"repo":"o/r","pr":9}`)
		tier := e.tools[name].spec.Tier
		if _, _, err := e.store.Begin(context.Background(), RunStart{Tier: tier, Key: CanonicalKey(42, name, in)}); err != nil {
			t.Fatal(err)
		}
		r := restart(e).Call(context.Background(), 1, name, in)
		if !r.IsError || r.Status != "unknown_outcome" {
			t.Fatalf("%s: replay %+v, want unknown_outcome", name, r)
		}
		if n := e.tools[name].count(); n != 0 {
			t.Fatalf("%s ran %d times after a crash inside the call", name, n)
		}
	}
}

// TC-9 (REQ-3, human): a different PR or a different post is a different
// action and still runs; the idempotency key is not a blanket block.
func TestPhase12DifferentInputStillRuns(t *testing.T) {
	e := newEnv(t, policy(), true, Limits{})
	if r := e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(`{"repo":"o/r","pr":7}`)); r.IsError {
		t.Fatalf("%+v", r)
	}
	if r := restart(e).Call(context.Background(), 1, "danger_a", json.RawMessage(`{"repo":"o/r","pr":8}`)); r.IsError || r.Status != "ok" {
		t.Fatalf("second PR: %+v", r)
	}
	if n := e.tools["danger_a"].count(); n != 2 {
		t.Fatalf("merges ran %d times, want 2", n)
	}
}
