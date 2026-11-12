package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type mapStore map[string]json.RawMessage

func (m mapStore) All(context.Context) (map[string]json.RawMessage, error) { return m, nil }

func TestSettings_DefaultsAndValidation(t *testing.T) {
	s, err := Load(context.Background(), mapStore{})
	if err != nil {
		t.Fatal(err)
	}
	if s.AllowDangerous || s.TriageModel != "claude-haiku-5-5" || s.ActorModel != "claude-sonnet-5-5" || s.SummaryModel != "claude-haiku-5-5" {
		t.Fatalf("defaults %+v", s)
	}
	if s.MaxInputTokens != 60000 || s.MaxOutputTokens != 6000 || s.DebounceWindow != 8*time.Second || s.DebounceMaxWait != 30*time.Second {
		t.Fatalf("defaults %+v", s)
	}
	if s.ReposDir != "/var/lib/nexus-hub/repos" || s.TmpDir != "/var/lib/nexus-hub/tmp" || s.MaxCloneBytes != 2<<30 || s.AnthropicCredential != "anthropic" {
		t.Fatalf("defaults %+v", s)
	}
	if s.AgentPollInterval != time.Second || s.ToolTimeout != 30*time.Second || s.GitNetworkTimeout != 120*time.Second || s.NoticePollInterval != time.Second || s.NoticeRequestTimeout != 10*time.Second || s.ListenAddr != ":8080" {
		t.Fatalf("defaults %+v", s)
	}

	s, err = Load(context.Background(), mapStore{"allow_dangerous": json.RawMessage(`true`), "max_input_tokens": json.RawMessage(`90000`), "debounce_window": json.RawMessage(`"5s"`), "git_path": json.RawMessage(`"/usr/bin/git"`)})
	if err != nil || !s.AllowDangerous || s.MaxInputTokens != 90000 || s.DebounceWindow != 5*time.Second || s.GitPath != "/usr/bin/git" {
		t.Fatalf("%+v %v", s, err)
	}

	bad := map[string]string{
		"max_input_tokens":     `-1`,
		"max_output_tokens":    `"6000"`,
		"allow_dangerous":      `"yes"`,
		"debounce_window":      `"forever"`,
		"tool_timeout":         `"0s"`,
		"repos_dir":            `"relative/path"`,
		"git_path":             `"/usr/bin/sh"`,
		"triage_model":         `""`,
		"anthropic_credential": `"Bad Ref"`,
		"max_clone_bytes":      `1.5`,
		"listen_addr":          `"no-port"`,
	}
	for k, v := range bad {
		if err := Validate(k, json.RawMessage(v)); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	if err := Validate("unknown_key", json.RawMessage(`1`)); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := Load(context.Background(), mapStore{"unknown_key": json.RawMessage(`1`)}); err == nil {
		t.Error("unknown stored key accepted")
	}
	if _, err := Load(context.Background(), mapStore{"debounce_window": json.RawMessage(`"40s"`)}); err == nil {
		t.Error("window above max wait accepted")
	}
}

func TestSettings_ParseCLIValue(t *testing.T) {
	cases := map[string][2]string{
		"max_input_tokens": {"90000", "90000"},
		"allow_dangerous":  {"true", "true"},
		"debounce_window":  {"5s", `"5s"`},
		"triage_model":     {"claude-haiku-5-5", `"claude-haiku-5-5"`},
	}
	for key, c := range cases {
		raw, err := ParseCLIValue(key, c[0])
		if err != nil || string(raw) != c[1] {
			t.Errorf("%s: %s %v", key, raw, err)
		}
	}
	if _, err := ParseCLIValue("max_input_tokens", "lots"); err == nil {
		t.Error("bad int accepted")
	}
}
