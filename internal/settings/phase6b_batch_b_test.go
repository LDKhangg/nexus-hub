package settings

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Phase 6b batch B: topic_close_statuses (REQ-9).

// TC-B1-70 (REQ-9): default, listing, override.
func TestPhase6bB1_TopicCloseStatusesDefaultAndOverride(t *testing.T) {
	s, err := Load(context.Background(), mapStore{})
	if err != nil || s.TopicCloseStatuses != "complete,closed,done" {
		t.Fatalf("default=%q err=%v", s.TopicCloseStatuses, err)
	}
	if !slices.Contains(Keys(), "topic_close_statuses") {
		t.Fatal("topic_close_statuses is not in Keys()")
	}
	if raw, err := DefaultJSON("topic_close_statuses"); err != nil || string(raw) != `"complete,closed,done"` {
		t.Fatalf("DefaultJSON=%s err=%v", raw, err)
	}
	s, err = Load(context.Background(), mapStore{"topic_close_statuses": json.RawMessage(`"shipped, Hoàn thành"`)})
	if err != nil || s.TopicCloseStatuses != "shipped, Hoàn thành" {
		t.Fatalf("override=%q err=%v", s.TopicCloseStatuses, err)
	}
}

// TC-B1-71 (REQ-9): validation, JSON and CLI.
func TestPhase6bB1_TopicCloseStatusesValidation(t *testing.T) {
	twenty := strings.TrimSuffix(strings.Repeat("s,", 20), ",")
	good := []string{`"done"`, `"a,b"`, `" done , closed "`, `"` + strings.Repeat("a", 40) + `"`, `"` + twenty + `"`, `"Hoàn thành"`, `"to do: not ready"`}
	for _, v := range good {
		if err := Validate("topic_close_statuses", json.RawMessage(v)); err != nil {
			t.Errorf("rejected %s: %v", v, err)
		}
	}
	bad := []string{
		`""`, `" "`, `","`, `"a,,b"`, `"a, ,b"`, `"done,"`, `",done"`,
		`"` + strings.Repeat("a", 41) + `"`, `"` + twenty + `,s"`,
		`"a\nb"`, `"a\u0000b"`, `"a\tb"`, `"a\u007fb"`,
		`12`, `true`, `null`, `["done"]`, `{}`,
	}
	for _, v := range bad {
		if err := Validate("topic_close_statuses", json.RawMessage(v)); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	// A bad stored value stops Load, so the server cannot start on it.
	if _, err := Load(context.Background(), mapStore{"topic_close_statuses": json.RawMessage(`"a,,b"`)}); err == nil {
		t.Error("Load accepted a bad stored list")
	}
	if raw, err := ParseCLIValue("topic_close_statuses", "complete,closed"); err != nil || string(raw) != `"complete,closed"` {
		t.Errorf("cli value: %s %v", raw, err)
	}
	for _, text := range []string{"", ",", "a,,b", "x\ny"} {
		if _, err := ParseCLIValue("topic_close_statuses", text); err == nil {
			t.Errorf("cli accepted %q", text)
		}
	}
}
