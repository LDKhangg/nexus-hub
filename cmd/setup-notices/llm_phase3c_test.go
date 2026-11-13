package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeClaudeCheck(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude-fake")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLLMCheckPhase3C(t *testing.T) {
	ctx, pool := setupDB(t)
	t.Run("TC-52 and TC-54/check prints metadata but no content", func(t *testing.T) {
		path := fakeClaudeCheck(t, `cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","structured_output":{"ok":true},"result":"PRIVATE ANSWER","usage":{"input_tokens":11,"output_tokens":2}}'`)
		var settingsOut bytes.Buffer
		err := settingsCommand(ctx, pool, []string{"set",
			"llm_provider", "claude_cli", "claude_cli_path", path,
			"triage_model", "claude-test", "actor_model", "claude-test", "summary_model", "claude-test",
		}, &settingsOut)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := llmCommand(ctx, pool, []string{"check"}, &out, os.LookupEnv); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["provider"] != "claude_cli" || row["model"] != "claude-test" || row["ok"] != true || row["input_tokens"] != float64(11) {
			t.Fatalf("row=%v", row)
		}
		if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "answer") {
			t.Fatalf("content leaked: %s", out.String())
		}
	})

	t.Run("TC-53/failure prints only safe code", func(t *testing.T) {
		// Use a documented ClassifyCLIFailure login marker, not an invented
		// approximation; the suffix verifies that stderr content is hidden.
		path := fakeClaudeCheck(t, `cat >/dev/null; echo 'not logged in PRIVATE TOKEN' >&2; exit 1`)
		var discard bytes.Buffer
		if err := settingsCommand(ctx, pool, []string{"set", "claude_cli_path", path}, &discard); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := llmCommand(ctx, pool, []string{"check"}, &out, os.LookupEnv); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"code":"llm_unconfigured"`) || strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "login") {
			t.Fatalf("unsafe output: %s", out.String())
		}
	})

	t.Run("TC-55/runbook mistakes fail before provider", func(t *testing.T) {
		var out bytes.Buffer
		if err := llmCommand(ctx, pool, []string{"check", "extra"}, &out, os.LookupEnv); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("bad args: %v", err)
		}
		if err := settingsCommand(ctx, pool, []string{"set", "llm_provider", "opencode_cli"}, &out); err == nil || !strings.Contains(err.Error(), "opencode_cli_path") {
			t.Fatalf("provider-only switch: %v", err)
		}
	})
}
