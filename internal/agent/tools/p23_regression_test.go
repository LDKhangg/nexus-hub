package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// P23-F7 (REQ-F7): even a syntactically valid invented name is untrusted.
// The returned name is fed back to the actor, so it must be safe as well.
func TestP23_UnregisteredToolNamesNeverBecomeAuditIdentifiers(t *testing.T) {
	for _, name := range []string{"fake_private_invented_tool", "SECRET\nAuthorization: fake", strings.Repeat("x", 20000)} {
		t.Run("untrusted_name", func(t *testing.T) {
			e := newEnv(t, policy(), false, Limits{})
			r := e.exec.Call(context.Background(), 1, name, json.RawMessage(`{}`))
			expectDenied(t, e, r, "unknown_tool")
			row := e.store.last()
			if row.Start.Tool != InvalidToolName || r.Tool != InvalidToolName || row.Start.Repo != "" || row.Start.Account != "" {
				t.Fatal("unregistered tool became trusted metadata")
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), name) || strings.Contains(r.Content, name) {
				t.Fatal("unregistered name leaked into audit or actor result")
			}
			for _, tool := range e.tools {
				if tool.count() != 0 {
					t.Fatal("unknown tool executed another registered tool")
				}
			}
		})
	}
}
