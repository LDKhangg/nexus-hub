package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// TC-11 (REQ-5), TC-12 (REQ-5): inside-tool policy denials retain denied/error semantics.
func TestApproved_InsideToolDenialIsAuditedDenied(t *testing.T) {
	e := newEnv(t, policy(), true, Limits{})
	e.tools["write_a"].run = func(context.Context, Call) (ToolOutput, error) {
		return ToolOutput{}, &ToolError{Code: "denied_path", Denied: true}
	}
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)), "denied_path")
	if r := e.exec.Call(context.Background(), 2, "read_repo", json.RawMessage(repoIn)); r.IsError || e.tools["read_repo"].count() != 1 {
		t.Error("configured read grant denied")
	}
	if r := e.exec.Call(context.Background(), 3, "danger_a", json.RawMessage(repoIn)); r.IsError || e.tools["danger_a"].count() != 1 {
		t.Error("fully authorized dangerous call denied")
	}
}

// TC-46 (REQ-F7): repo identifiers rejected by validation never become
// trusted audit metadata, even when their input is otherwise valid JSON.
func TestApproved_InvalidRepoNeverStoredAsAuditIdentifier(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	r := e.exec.Call(context.Background(), 1, "read_repo", json.RawMessage(`{"repo":"private-repo-SECRET/owner/extra"}`))
	if !r.IsError || r.Status != StatusDenied || e.tools["read_repo"].count() != 0 {
		t.Error("invalid repo was not denied before execution")
	}
	if e.store.last().Start.Repo != "" {
		t.Error("invalid repo stored verbatim in trusted audit metadata")
	}
}

// TC-14 (REQ-F9): a write that panics/times out after its effect is never repeated.
func TestApproved_AmbiguousWriteTimeoutPanicNeverRepeated(t *testing.T) {
	for _, mode := range []string{"panic", "timeout", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			l := DefaultLimits()
			l.ReadTimeout = 20 * time.Millisecond
			e := newEnv(t, policy(), false, l)
			e.tools["write_a"].run = func(ctx context.Context, _ Call) (ToolOutput, error) {
				switch mode {
				case "panic":
					panic("fake-private-panic")
				case "timeout":
					<-ctx.Done()
					return ToolOutput{}, ctx.Err()
				}
				return ToolOutput{}, &ToolError{Code: "invalid_response", Ambiguous: true}
			}
			a := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
			fresh := NewExecutor(e.exec.reg, policy(), l, false, e.store, upperSan{}, 42, "ws-1")
			b := fresh.Call(context.Background(), 2, "write_a", json.RawMessage(`{ "repo":"o/r" }`))
			if !a.IsError || a.Status != StatusUnknownOutcome || !b.IsError || b.Status != StatusUnknownOutcome || e.tools["write_a"].count() != 1 {
				t.Error("uncertain effect repeated or not classified unknown_outcome")
			}
			if e.store.last().Status != StatusUnknownOutcome {
				t.Error("unknown outcome was not durable")
			}
		})
	}
}

// TC-23 (REQ-11), TC-24 (REQ-11): dry-run is not a permission bypass;
// switching it off executes the same formerly simulated write exactly once.
func TestApproved_DryRunPreservesPolicyAndRealWrite(t *testing.T) {
	p := policy()
	p.DryRun = true
	e := newEnv(t, p, false, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn)), "dangerous_disabled")
	r := e.exec.Call(context.Background(), 2, "write_a", json.RawMessage(repoIn))
	if r.IsError || r.Status != StatusDryRun || e.tools["write_a"].count() != 0 {
		t.Error("dry-run write executed")
	}
	p.DryRun = false
	real := NewExecutor(e.exec.reg, p, DefaultLimits(), false, e.store, upperSan{}, 42, "ws-1")
	r = real.Call(context.Background(), 3, "write_a", json.RawMessage(repoIn))
	if r.IsError || r.Status != StatusOK || e.tools["write_a"].count() != 1 {
		t.Error("dry-run audit suppressed later real effect")
	}
	p.DryRun = true
	on := newEnv(t, p, true, Limits{})
	r = on.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn))
	if r.Tier != tenant.TierDangerous || r.Status != StatusDryRun || on.tools["danger_a"].count() != 0 {
		t.Error("authorized dangerous simulation performed effect")
	}
}
