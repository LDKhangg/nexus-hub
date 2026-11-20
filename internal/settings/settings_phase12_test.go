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
	for _, v := range []string{`"1s"`, `"25s"`, `"120s"`, `"1m"`, `"2m"`, `"1500ms"`} {
		if err := Validate("shutdown_timeout", json.RawMessage(v)); err != nil {
			t.Errorf("rejected %s: %v", v, err)
		}
	}
	for _, v := range []string{`"0s"`, `"999ms"`, `"121s"`, `"-5s"`, `"3m"`, `"abc"`, `""`, `"25"`, `25`, `null`, `true`, `["25s"]`} {
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
	for _, text := range []string{"0", "30", "9999s", "", "mot phut"} {
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
			good: []string{`1`, `4`, `64`},
			bad:  []string{`0`, `-3`, `65`, `2.5`, `"4"`, `null`, `[]`},
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
