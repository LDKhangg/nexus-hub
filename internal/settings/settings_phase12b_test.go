package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TC-B-1 (REQ-8): metrics_addr accepts loopback host:port and "" (off).
func TestPhase12bMetricsAddrLoopbackOnly(t *testing.T) {
	if got := Defaults().MetricsAddr; got != "127.0.0.1:9090" {
		t.Fatalf("default = %q", got)
	}
	for _, v := range []string{`"127.0.0.1:9090"`, `"[::1]:9090"`, `"localhost:1"`, `"127.0.0.1:65535"`, `"127.5.5.5:8080"`, `""`, `null`} {
		if err := Validate("metrics_addr", json.RawMessage(v)); err != nil {
			t.Errorf("rejected %s: %v", v, err)
		}
	}
	for _, v := range []string{
		`"0.0.0.0:9090"`, `":9090"`, `"[::]:9090"`, `"8.8.8.8:9090"`, `"10.0.0.5:9090"`, `"example.com:9090"`,
		`"127.0.0.1:0"`, `"127.0.0.1:70000"`, `"127.0.0.1:-1"`, `"127.0.0.1:"`, `"127.0.0.1"`, `"localhost"`,
		`"127.0.0.1:9090/metrics"`, `" 127.0.0.1:9090"`, `"127.0.0.1:abc"`, `"public"`, `9090`, `true`,
	} {
		if err := Validate("metrics_addr", json.RawMessage(v)); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	if _, err := ParseCLIValue("metrics_addr", "0.0.0.0:9090"); err == nil {
		t.Error("CLI accepted a wildcard address")
	}
}

// TC-B-2 (REQ-8, abuse): a bad stored metrics_addr never loads, so the
// listener cannot be bound to a public interface through the store.
func TestPhase12bBadStoredMetricsAddrDoesNotLoad(t *testing.T) {
	for _, v := range []string{`"0.0.0.0:9090"`, `":9090"`, `"203.0.113.9:9090"`} {
		if _, err := Load(context.Background(), mapStore{"metrics_addr": json.RawMessage(v)}); err == nil {
			t.Errorf("Load accepted %s", v)
		}
	}
	s, err := Load(context.Background(), mapStore{"metrics_addr": json.RawMessage(`""`)})
	if err != nil || s.MetricsAddr != "" {
		t.Fatalf("off: %q %v", s.MetricsAddr, err)
	}
	// JSON null reads as "" (metrics off), the safe side.
	s, err = Load(context.Background(), mapStore{"metrics_addr": json.RawMessage(`null`)})
	if err != nil || s.MetricsAddr != "" {
		t.Fatalf("null: %q %v", s.MetricsAddr, err)
	}
	s, err = Load(context.Background(), mapStore{"metrics_addr": json.RawMessage(`"127.0.0.1:19090"`)})
	if err != nil || s.MetricsAddr != "127.0.0.1:19090" {
		t.Fatalf("override: %q %v", s.MetricsAddr, err)
	}
}

// TC-B-26 (REQ-7): llm_daily_budget_usd bounds.
func TestPhase12bLLMDailyBudgetBounds(t *testing.T) {
	for _, v := range []string{`2`, `0.5`, `0`, `1000000`} {
		if err := Validate("llm_daily_budget_usd", json.RawMessage(v)); err != nil {
			t.Errorf("rejected %s: %v", v, err)
		}
	}
	for _, v := range []string{`-1`, `-0.01`, `1000001`, `"abc"`, `"2"`, `null`, `true`, `[2]`, `"NaN"`} {
		if err := Validate("llm_daily_budget_usd", json.RawMessage(v)); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	s, err := Load(context.Background(), mapStore{"llm_daily_budget_usd": json.RawMessage(`2`)})
	if err != nil || s.LLMDailyBudgetUSD != 2 {
		t.Fatalf("budget = %v %v", s.LLMDailyBudgetUSD, err)
	}
	if Defaults().LLMDailyBudgetUSD != 0 {
		t.Fatal("budget must default to off")
	}
	if _, err := ParseCLIValue("llm_daily_budget_usd", "abc"); err == nil {
		t.Error("CLI accepted abc")
	}
	if raw, err := ParseCLIValue("llm_daily_budget_usd", "2"); err != nil || strings.TrimSpace(string(raw)) != "2" {
		t.Errorf("CLI 2: %s %v", raw, err)
	}
}

// TC-B-36 (REQ-10): alert settings defaults (15/30 min, on) and bounds.
func TestPhase12bAlertSettingsDefaultsAndBounds(t *testing.T) {
	d := Defaults()
	if !d.AlertsEnabled || d.AlertStuckRunningMinutes != 15 || d.AlertStuckQueuedMinutes != 30 {
		t.Fatalf("defaults = %v %d %d", d.AlertsEnabled, d.AlertStuckRunningMinutes, d.AlertStuckQueuedMinutes)
	}
	for _, key := range []string{"alert_stuck_running_minutes", "alert_stuck_queued_minutes"} {
		for _, v := range []string{`1`, `20`, `1440`} {
			if err := Validate(key, json.RawMessage(v)); err != nil {
				t.Errorf("%s rejected %s: %v", key, v, err)
			}
		}
		for _, v := range []string{`0`, `-1`, `1441`, `1.5`, `"15"`, `"abc"`, `null`, `true`} {
			if err := Validate(key, json.RawMessage(v)); err == nil {
				t.Errorf("%s accepted %s", key, v)
			}
		}
	}
	s, err := Load(context.Background(), mapStore{
		"alerts_enabled":              json.RawMessage(`false`),
		"alert_stuck_running_minutes": json.RawMessage(`20`),
		"alert_stuck_queued_minutes":  json.RawMessage(`40`),
	})
	if err != nil || s.AlertsEnabled || s.AlertStuckRunningMinutes != 20 || s.AlertStuckQueuedMinutes != 40 {
		t.Fatalf("override = %+v %v", s, err)
	}
	if err := Validate("alerts_enabled", json.RawMessage(`"yes"`)); err == nil {
		t.Error("alerts_enabled accepted a string")
	}
}
