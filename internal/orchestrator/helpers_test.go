package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// reply scripts one JSON answer (marshalled from v) with in/out tokens.
func reply(t *testing.T, v any, in, out int64) FakeReply {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return FakeReply{JSON: b, Usage: FakeUsage{Input: in, Output: out}}
}

// rawReply scripts a raw (possibly invalid) JSON text.
func rawReply(s string, in, out int64) FakeReply {
	return FakeReply{JSON: json.RawMessage(s), Usage: FakeUsage{Input: in, Output: out}}
}

func m(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func classifyJSON(intent, taskType string, confidence float64) map[string]any {
	return m("intent", intent, "task_type", taskType, "priority", "normal", "complexity", "m", "confidence", confidence)
}

func ptask(key, title string, caps []string, deps ...string) map[string]any {
	if deps == nil {
		deps = []string{}
	}
	return m("key", key, "title", title, "capabilities", caps, "depends_on", deps, "priority", "normal", "complexity", "s")
}

func planJSON(decision string, tasks ...map[string]any) map[string]any {
	return m("tasks", tasks, "decision", decision)
}

func mustErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("want error %v, got %v", target, err)
	}
}

func lastPrompt(t *testing.T, f *FakeRuntime) string {
	t.Helper()
	r := f.Requests()
	if len(r) == 0 {
		t.Fatal("no request recorded")
	}
	return r[len(r)-1].Prompt
}

func requireNoLeak(t *testing.T, s string, secrets ...string) {
	t.Helper()
	for _, x := range secrets {
		if strings.Contains(s, x) {
			t.Fatalf("text leaks %q: %s", x, s)
		}
	}
}

var planCaps = []string{CapRepoRead, CapRepoWrite, CapClickUpRead}

func newOrch(f *FakeRuntime) *Orchestrator { return NewOrchestrator(f, "test-model") }
