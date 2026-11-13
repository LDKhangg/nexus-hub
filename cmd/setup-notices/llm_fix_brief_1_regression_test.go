package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixBrief1LLMCheckPrintsCleanupNoteWithoutContent(t *testing.T) {
	// TC-R5 (REQ-C5, REQ-C6, REQ-C11).
	t.Run("TC-R5/nh llm check prints safe cleanup note", func(t *testing.T) {
		ctx, pool := setupDB(t)
		path := filepath.Join(t.TempDir(), "fake-opencode")
		body := `#!/bin/sh
set -eu
if [ "${1-}" = session ]; then
  printf 'PRIVATE CLEANUP OUTPUT\n' >&2
  exit 1
fi
cat >/dev/null
printf '%s\n' '{"type":"text","sessionID":"ses_check","part":{"id":"p1","text":"{\"ok\":true}"}}' '{"type":"step_finish","part":{"reason":"stop","tokens":{"input":9,"output":2}}}'
`
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		var settingsOut bytes.Buffer
		if err := settingsCommand(ctx, pool, []string{"set",
			"llm_provider", "opencode_cli", "opencode_cli_path", path,
			"triage_model", "openai/test", "actor_model", "openai/test", "summary_model", "openai/test",
		}, &settingsOut); err != nil {
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
		if row["ok"] != true || row["note"] != "cli_session_cleanup_failed" || row["input_tokens"] != float64(9) {
			t.Fatalf("row=%v", row)
		}
		if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), `{"ok":true}`) {
			t.Fatalf("content leaked: %s", out.String())
		}
	})
}
