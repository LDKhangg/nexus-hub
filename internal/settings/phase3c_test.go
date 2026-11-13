package settings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type phase3Store map[string]json.RawMessage
func (s phase3Store) All(context.Context) (map[string]json.RawMessage, error) { return s, nil }

func TestPhase3CProviderModelShapesAndMergedLoad(t *testing.T) {
	// TC-41, TC-42 (REQ-C1, REQ-C8, REQ-C12).
	exe := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { t.Fatal(err) }
	store := phase3Store{
		"llm_provider": json.RawMessage(`"opencode_cli"`),
		"opencode_cli_path": json.RawMessage(strconvJSON(exe)),
		"triage_model": json.RawMessage(`"openai/gpt-6-luna"`),
		"actor_model": json.RawMessage(`"openai/gpt-6-luna#high"`),
		"summary_model": json.RawMessage(`"anthropic/claude-test"`),
	}
	s, err := Load(context.Background(), store)
	if err != nil || s.LLMProvider != LLMProviderOpencodeCLI { t.Fatalf("merged settings: %#v %v", s, err) }
	if !ModelFitsProvider(LLMProviderOpencodeCLI, "openai/gpt-6-luna#high") || ModelFitsProvider(LLMProviderOpencodeCLI, "claude-test") { t.Fatal("opencode model shape") }
	if !ModelFitsProvider(LLMProviderClaudeCLI, "claude-test") || ModelFitsProvider(LLMProviderClaudeCLI, "openai/gpt") { t.Fatal("claude model shape") }
}

func TestPhase3CProviderValidationNamesAllMismatchedModels(t *testing.T) {
	// TC-42, TC-55 (REQ-C8, REQ-C12).
	exe := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { t.Fatal(err) }
	_, err := Load(context.Background(), phase3Store{
		"llm_provider": json.RawMessage(`"opencode_cli"`), "opencode_cli_path": json.RawMessage(strconvJSON(exe)),
		"triage_model": json.RawMessage(`"claude-a"`), "actor_model": json.RawMessage(`"claude-b"`), "summary_model": json.RawMessage(`"claude-c"`),
	})
	if err == nil { t.Fatal("accepted mismatched models") }
	for _, key := range []string{"triage_model", "actor_model", "summary_model"} { if !strings.Contains(err.Error(), key) { t.Errorf("missing %s: %v", key, err) } }
	for _, bad := range []string{"relative", "/missing/provider", "/tmp/../tmp/provider"} {
		if Validate("opencode_cli_path", json.RawMessage(strconvJSON(bad))) == nil { t.Errorf("accepted path %q", bad) }
	}
}

func strconvJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
