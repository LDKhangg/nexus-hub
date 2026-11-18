package agent

import (
	"context"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 7b B2 (R-B-1, REQ-10): TokenBudget.MaxTotal caps input, output and
// cache tokens together; 0 keeps the per-side behavior.

// B2-A1: exhausted() with MaxTotal.
func TestTokenBudget_MaxTotalExhausted(t *testing.T) {
	cases := []struct {
		name string
		b    TokenBudget
		want bool
	}{
		{"under", TokenBudget{MaxTotal: 100, Used: domain.Usage{Input: 59, Output: 40}}, false},
		{"exactly at cap", TokenBudget{MaxTotal: 100, Used: domain.Usage{Input: 60, Output: 40}}, true},
		{"cache read counts", TokenBudget{MaxTotal: 100, Used: domain.Usage{Input: 10, CacheRead: 60, Output: 30}}, true},
		{"cache write counts", TokenBudget{MaxTotal: 100, Used: domain.Usage{Input: 10, CacheWrite: 60, Output: 30}}, true},
		{"output alone", TokenBudget{MaxTotal: 100, Used: domain.Usage{Output: 100}}, true},
		{"zero is unlimited", TokenBudget{Used: domain.Usage{Input: 1 << 40, Output: 1 << 40}}, false},
		{"negative is unlimited", TokenBudget{MaxTotal: -1, Used: domain.Usage{Input: 1000, Output: 1000}}, false},
		{"per-side input still applies", TokenBudget{MaxInput: 50, MaxTotal: 1000, Used: domain.Usage{Input: 50}}, true},
		{"per-side output still applies", TokenBudget{MaxOutput: 50, MaxTotal: 1000, Used: domain.Usage{Output: 50}}, true},
		{"per-side only, 0 total", TokenBudget{MaxInput: 100, MaxOutput: 100, Used: domain.Usage{Input: 99, Output: 99}}, false},
		{"fresh budget", TokenBudget{MaxInput: 100, MaxOutput: 100, MaxTotal: 100}, false},
	}
	for _, c := range cases {
		if got := c.b.exhausted(); got != c.want {
			t.Errorf("%s: exhausted=%v want %v", c.name, got, c.want)
		}
	}
}

// B2-A2: the loop stops when input+output together reach MaxTotal even though each side is under its own cap.
func TestActor_MaxTotalStopsLoop(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("s", "git_log", `{}`))}} // 15 tokens per step
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec,
		TokenBudget{MaxInput: 60000, MaxOutput: 6000, MaxTotal: 40})
	if err != nil || r.Stopped != StopTokenBudget || len(llm.reqs) != 3 {
		t.Fatalf("%+v reqs %d err %v", r, len(llm.reqs), err)
	}
	if r.Usage.Input+r.Usage.Output != 45 {
		t.Fatalf("usage %+v", r.Usage)
	}
}

// B2-A3: a budget already spent by earlier attempts stops before any model call.
func TestActor_MaxTotalPreSpentMakesNoCall(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{answer("x")}}
	r, _ := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec,
		TokenBudget{MaxTotal: 100, Used: domain.Usage{Input: 70, Output: 30}})
	if r.Stopped != StopTokenBudget || len(llm.reqs) != 0 {
		t.Fatalf("%+v reqs %d", r, len(llm.reqs))
	}
}

// B2-A4: with MaxTotal 0 the agent path is unchanged: only the step limit stops a long loop.
func TestActor_MaxTotalZeroKeepsAgentPath(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("s", "git_log", `{}`))}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil || r.Stopped != StopStepLimit || len(llm.reqs) != MaxActorSteps {
		t.Fatalf("%+v reqs %d err %v", r, len(llm.reqs), err)
	}
}
