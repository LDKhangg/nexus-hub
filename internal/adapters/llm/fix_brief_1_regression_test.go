package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm/clirun"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

func fixBriefOpencode(t *testing.T, answer, cleanupMode string) (*OpencodeCLI, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "cleanup-attempts")
	path := filepath.Join(dir, "fake-opencode")
	textEvent := `{"type":"text","sessionID":"ses_fix","part":{"id":"p1","text":` + strconvQuote(answer) + `}}`
	finishEvent := `{"type":"step_finish","part":{"reason":"stop","tokens":{"input":7,"output":3}}}`
	body := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1-}" = "session" ]; then
  printf x >> %q
  if [ %q = fail ] || [ ! -f %q ]; then
    touch %q
    printf 'PRIVATE CLEANUP OUTPUT\n' >&2
    exit 1
  fi
  exit 0
fi
cat >/dev/null
printf '%%s\n' '%s' '%s'
`, logPath, cleanupMode, filepath.Join(dir, "failed-once"), filepath.Join(dir, "failed-once"), textEvent, finishEvent)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := clirun.New(clirun.Config{Path: path, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewOpencodeCLI(runner, "openai/test")
	if err != nil {
		t.Fatal(err)
	}
	return provider, logPath
}

func cleanupAttempts(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

func TestFixBrief1CLILoginPhrasesAreUnconfigured(t *testing.T) {
	// TC-R4 (REQ-C3).
	for _, phrase := range []string{"please login", "please log in", "login required", "not signed in"} {
		phrase := phrase
		t.Run("TC-R4/"+strings.ReplaceAll(phrase, " ", "_"), func(t *testing.T) {
			err := ClassifyCLIFailure(1, "PRIVATE "+strings.ToUpper(phrase)+" TOKEN")
			if err.Code != domain.LLMCodeUnconfigured || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(strings.ToLower(err.Error()), "login") {
				t.Fatalf("unsafe classification: %#v / %v", err, err)
			}
		})
	}
}

func TestFixBrief1OpencodeCleanupFailureKeepsResponsesAndSafeNote(t *testing.T) {
	// TC-R5 (REQ-C5, REQ-C6, REQ-C10, REQ-C11).
	t.Run("TC-R5/plain response", func(t *testing.T) {
		provider, attempts := fixBriefOpencode(t, "kept answer", "fail")
		res, err := provider.Generate(context.Background(), &domain.LLMRequest{Messages: []domain.Message{{Role: domain.UserRole, Text: "question"}}})
		if err != nil || res.Text != "kept answer" || res.Note != NoteCLISessionCleanupFailed || res.Usage.Input != 7 {
			t.Fatalf("response=%#v err=%v", res, err)
		}
		if got := cleanupAttempts(t, attempts); got != 2 {
			t.Fatalf("cleanup attempts=%d", got)
		}
	})

	t.Run("TC-R5/structured response and check", func(t *testing.T) {
		provider, attempts := fixBriefOpencode(t, `{"ok":true}`, "fail")
		check := Check(context.Background(), provider, "openai/test")
		if !check.OK || check.Code != "" || check.Note != NoteCLISessionCleanupFailed || check.InputTokens != 7 {
			t.Fatalf("check=%#v", check)
		}
		if got := cleanupAttempts(t, attempts); got != 2 {
			t.Fatalf("cleanup attempts=%d", got)
		}
	})

	t.Run("TC-R5/tool response", func(t *testing.T) {
		provider, attempts := fixBriefOpencode(t, `{"text":"done","calls":[],"stop":"end_turn"}`, "fail")
		res, err := provider.GenerateWithTools(context.Background(), &domain.ToolRequest{Turns: []domain.Turn{{Role: domain.UserRole, Text: "question"}}})
		if err != nil || res.Text != "done" || res.Note != NoteCLISessionCleanupFailed || res.Usage.Input != 7 {
			t.Fatalf("response=%#v err=%v", res, err)
		}
		if got := cleanupAttempts(t, attempts); got != 2 {
			t.Fatalf("cleanup attempts=%d", got)
		}
	})

	t.Run("TC-R5/second cleanup succeeds without note", func(t *testing.T) {
		provider, attempts := fixBriefOpencode(t, `{"ok":true}`, "once")
		res, err := provider.GenerateStructured(context.Background(), &StructuredRequest{
			Prompt: "question",
			Schema: json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`),
		})
		if err != nil || res.Note != "" || string(res.JSON) != `{"ok":true}` {
			t.Fatalf("response=%#v err=%v", res, err)
		}
		if got := cleanupAttempts(t, attempts); got != 2 {
			t.Fatalf("cleanup attempts=%d", got)
		}
	})
}
