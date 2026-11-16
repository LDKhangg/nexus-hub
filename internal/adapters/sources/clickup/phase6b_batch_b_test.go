package clickup

import (
	"fmt"
	"strings"
	"testing"
)

// Phase 6b batch B: the status type of a status change (REQ-9).

func b1StatusBody(after string) string {
	return fmt.Sprintf(`{"webhook_id":"w1","event":"taskStatusUpdated","task_id":"t1","history_items":[{"id":"h1","field":"status","date":"1700000000000","user":{"id":7,"username":"Author"},"before":{"status":"open","type":"open"},"after":%s}]}`, after)
}

// TC-B1-60 (REQ-9): status_type is lower-cased metadata next to status_to;
// absent, blank or oversized types are dropped, never fail the event.
func TestPhase6bB1_StatusTypeMetadata(t *testing.T) {
	cases := []struct {
		name, after, want string
		hasType           bool
	}{
		{"done", `{"status":"complete","type":"done"}`, "done", true},
		{"closed", `{"status":"closed","type":"closed"}`, "closed", true},
		{"custom", `{"status":"Complete","type":"custom"}`, "custom", true},
		{"upper case and spaces", `{"status":"x","type":"  DONE "}`, "done", true},
		{"no type", `{"status":"closed"}`, "", false},
		{"blank type", `{"status":"closed","type":"  "}`, "", false},
		{"null type", `{"status":"closed","type":null}`, "", false},
		{"32 bytes kept", `{"status":"x","type":"` + strings.Repeat("a", 32) + `"}`, strings.Repeat("a", 32), true},
		{"33 bytes dropped", `{"status":"x","type":"` + strings.Repeat("a", 33) + `"}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := NewClickUpParser("", "").Parser([]byte(b1StatusBody(c.after)))
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%+v err=%v", events, err)
			}
			got, has := events[0].Metadata["status_type"]
			if has != c.hasType || got != c.want {
				t.Fatalf("status_type=(%q,%v) want (%q,%v)", got, has, c.want, c.hasType)
			}
			if events[0].Metadata["status_to"] == "" || events[0].Metadata["status_from"] != "open" {
				t.Fatalf("metadata=%v", events[0].Metadata)
			}
		})
	}
	// The existing real payload still parses without types.
	body := `{"webhook_id":"w1","event":"taskStatusUpdated","task_id":"t1","history_items":[{"id":"h2","field":"status","date":"1700000001000","before":null,"after":{"status":"closed"}}]}`
	events, err := NewClickUpParser("", "").Parser([]byte(body))
	if err != nil || len(events) != 1 || events[0].Content != "Unknown → closed" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if _, has := events[0].Metadata["status_type"]; has {
		t.Fatal("status_type set without a type")
	}
}
