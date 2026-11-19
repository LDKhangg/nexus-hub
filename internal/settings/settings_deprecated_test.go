package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Phase 9 Batch A (P5): allow_dangerous is retired but stays a known key.

// TC-S1 (REQ-8): allow_dangerous is deprecated, other keys are not.
func TestDeprecated_OnlyAllowDangerous(t *testing.T) {
	note, ok := Deprecated("allow_dangerous")
	if !ok || !strings.Contains(note, "deprecated") || !strings.Contains(note, "no effect") || !strings.Contains(note, "nh policy") {
		t.Fatalf("%q %v", note, ok)
	}
	for _, k := range Keys() {
		if k == "allow_dangerous" {
			continue
		}
		if n, dep := Deprecated(k); dep {
			t.Fatalf("%s is deprecated: %q", k, n)
		}
	}
	if _, dep := Deprecated("no_such_key"); dep {
		t.Fatal("unknown key reported as deprecated")
	}
}

// TC-S2 (REQ-8): the key is still accepted and validated, so an existing DB value keeps loading.
func TestAllowDangerous_StillKnownAndValidated(t *testing.T) {
	found := false
	for _, k := range Keys() {
		found = found || k == "allow_dangerous"
	}
	if !found {
		t.Fatal("allow_dangerous is no longer a known key")
	}
	for _, v := range []string{`true`, `false`} {
		if _, err := Load(context.Background(), mapStore{"allow_dangerous": json.RawMessage(v)}); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if _, err := Load(context.Background(), mapStore{"allow_dangerous": json.RawMessage(`"yes"`)}); err == nil {
		t.Fatal("a non-boolean allow_dangerous was accepted")
	}
	if def, err := DefaultJSON("allow_dangerous"); err != nil || string(def) != "false" {
		t.Fatalf("%s %v", def, err)
	}
}
