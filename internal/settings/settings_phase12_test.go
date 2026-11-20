package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TC-20 (REQ-4): shutdown_timeout defaults to 25s, takes 1s..120s, and
// rejects zero, negative, too large, wrong type and unit-less input.
func TestPhase12ShutdownTimeoutDefaultAndBounds(t *testing.T) {
	if d := Defaults().ShutdownTimeout; d != 25*time.Second {
		t.Fatalf("default = %v, want 25s", d)
	}
	s, err := Load(context.Background(), mapStore{})
	if err != nil || s.ShutdownTimeout != 25*time.Second {
		t.Fatalf("unset: %v %v", s.ShutdownTimeout, err)
	}
	for _, v := range []string{`"1s"`, `"25s"`, `"80s"`, `"1m"`, `"1500ms"`, `"1m20s"`} {
		if err := Validate("shutdown_timeout", json.RawMessage(v)); err != nil {
			t.Errorf("rejected %s: %v", v, err)
		}
	}
	for _, v := range []string{`"0s"`, `"999ms"`, `"81s"`, `"121s"`, `"-5s"`, `"2m"`, `"3m"`, `"abc"`, `""`, `"25"`, `25`, `null`, `true`, `["25s"]`} {
		if err := Validate("shutdown_timeout", json.RawMessage(v)); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	s, err = Load(context.Background(), mapStore{"shutdown_timeout": json.RawMessage(`"40s"`)})
	if err != nil || s.ShutdownTimeout != 40*time.Second {
		t.Fatalf("override: %v %v", s.ShutdownTimeout, err)
	}
	if _, err := Load(context.Background(), mapStore{"shutdown_timeout": json.RawMessage(`"0s"`)}); err == nil {
		t.Fatal("Load accepted shutdown_timeout 0s")
	}
	if _, err := ParseCLIValue("shutdown_timeout", "30s"); err != nil {
		t.Errorf("CLI 30s: %v", err)
	}
	for _, text := range []string{"0", "30", "81s", "9999s", "", "mot phut"} {
		if _, err := ParseCLIValue("shutdown_timeout", text); err == nil {
			t.Errorf("CLI accepted %q", text)
		}
	}
}

// TC-34 (REQ-7): the quota settings have the documented defaults and bounds;
// bad numbers never load.
func TestPhase12QuotaSettingsDefaultsAndBounds(t *testing.T) {
	d := Defaults()
	if d.QueueMaxPending != 500 || d.WorkspaceMaxConcurrent != 4 || d.LLMDailyBudgetUSD != 0 {
		t.Fatalf("defaults = %d %d %v", d.QueueMaxPending, d.WorkspaceMaxConcurrent, d.LLMDailyBudgetUSD)
	}
	s, err := Load(context.Background(), mapStore{})
	if err != nil || s.QueueMaxPending != 500 || s.WorkspaceMaxConcurrent != 4 {
		t.Fatalf("unset: %+v %v", s, err)
	}
	s, err = Load(context.Background(), mapStore{
		"queue_max_pending":                   json.RawMessage(`10`),
		"workspace_max_concurrent_executions": json.RawMessage(`2`),
		"llm_daily_budget_usd":                json.RawMessage(`12.5`),
	})
	if err != nil || s.QueueMaxPending != 10 || s.WorkspaceMaxConcurrent != 2 || s.LLMDailyBudgetUSD != 12.5 {
		t.Fatalf("override: %+v %v", s, err)
	}
	cases := map[string]struct{ good, bad []string }{
		"queue_max_pending": {
			good: []string{`1`, `500`, `1000000`},
			bad:  []string{`0`, `-1`, `1000001`, `1.5`, `"500"`, `null`, `true`, `1e999`, `NaN`},
		},
		"workspace_max_concurrent_executions": {
			good: []string{`0`, `1`, `4`, `64`},
			bad:  []string{`-1`, `-3`, `65`, `2.5`, `"4"`, `null`, `[]`},
		},
		"llm_daily_budget_usd": {
			good: []string{`0`, `0.01`, `12.5`, `1000000`},
			bad:  []string{`-0.01`, `-1`, `1000001`, `"5"`, `null`, `1e999`, `NaN`, `Infinity`, `true`},
		},
	}
	for key, c := range cases {
		for _, v := range c.good {
			if err := Validate(key, json.RawMessage(v)); err != nil {
				t.Errorf("%s rejected %s: %v", key, v, err)
			}
		}
		for _, v := range c.bad {
			if err := Validate(key, json.RawMessage(v)); err == nil {
				t.Errorf("%s accepted %s", key, v)
			}
			if _, err := Load(context.Background(), mapStore{key: json.RawMessage(v)}); err == nil {
				t.Errorf("Load accepted %s=%s", key, v)
			}
		}
	}
	for _, text := range []string{"NaN", "Inf", "-Inf", "abc", "", "1,5"} {
		if _, err := ParseCLIValue("llm_daily_budget_usd", text); err == nil {
			t.Errorf("CLI accepted budget %q", text)
		}
	}
	for _, text := range []string{"0", "-1", "x", "", "2.5"} {
		if _, err := ParseCLIValue("queue_max_pending", text); err == nil {
			t.Errorf("CLI accepted queue_max_pending %q", text)
		}
	}
}

// R-A-5 (REQ-4): shutdown_timeout tops out at 80s so that it, the stop grace
// and the lease release stay below TimeoutStopSec=100.
func TestPhase12ShutdownTimeoutMaxIs80s(t *testing.T) {
	if err := Validate("shutdown_timeout", json.RawMessage(`"80s"`)); err != nil {
		t.Fatalf("80s rejected: %v", err)
	}
	for _, v := range []string{`"81s"`, `"80.1s"`, `"100s"`, `"120s"`} {
		if err := Validate("shutdown_timeout", json.RawMessage(v)); err == nil {
			t.Errorf("%s accepted", v)
		}
	}
	if _, err := ParseCLIValue("shutdown_timeout", "81s"); err == nil {
		t.Error("CLI accepted 81s")
	}
}

// R-A-4 (REQ-7): workspace_max_concurrent_executions = 0 means "off" and is
// accepted (also from the CLI and from Load); negative is still refused.
func TestPhase12WorkspaceCapZeroMeansOff(t *testing.T) {
	if err := Validate("workspace_max_concurrent_executions", json.RawMessage(`0`)); err != nil {
		t.Fatalf("0 rejected: %v", err)
	}
	if _, err := ParseCLIValue("workspace_max_concurrent_executions", "0"); err != nil {
		t.Fatalf("CLI 0 rejected: %v", err)
	}
	s, err := Load(context.Background(), mapStore{"workspace_max_concurrent_executions": json.RawMessage(`0`)})
	if err != nil || s.WorkspaceMaxConcurrent != 0 {
		t.Fatalf("Load 0: %d %v", s.WorkspaceMaxConcurrent, err)
	}
	if err := Validate("workspace_max_concurrent_executions", json.RawMessage(`-1`)); err == nil {
		t.Fatal("-1 accepted")
	}
}

// D-A-2 (REQ-7): JSON null (also padded) is refused for every int, float and
// duration setting instead of silently becoming zero.
func TestPhase12NullIsRejectedForNumbersAndDurations(t *testing.T) {
	keys := []string{"queue_max_pending", "workspace_max_concurrent_executions", "llm_daily_budget_usd", "shutdown_timeout", "tool_timeout", "behavior_poll_interval", "debounce_window", "orchestrator_min_confidence", "message_link_retention_days"}
	for _, k := range keys {
		for _, v := range []string{`null`, ` null`, "null\n", ` null `} {
			if err := Validate(k, json.RawMessage(v)); err == nil {
				t.Errorf("%s accepted %q", k, v)
			}
			if _, err := Load(context.Background(), mapStore{k: json.RawMessage(v)}); err == nil {
				t.Errorf("Load accepted %s=%q", k, v)
			}
		}
	}
	// A real zero or a normal value is still fine.
	if err := Validate("llm_daily_budget_usd", json.RawMessage(`0`)); err != nil {
		t.Fatal(err)
	}
}
