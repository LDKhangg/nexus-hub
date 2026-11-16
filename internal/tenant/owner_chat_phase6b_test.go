package tenant

import (
	"math"
	"testing"
)

// TC-A3.2 (REQ-3): owner_chat_id > chat_id > none; invalid values fall
// through; validation refuses garbage.
func TestPhase6bTelegramOwnerChatID(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    map[string]any
		want int64
	}{
		{"owner_chat_id wins", map[string]any{"owner_chat_id": float64(-100), "chat_id": "-200"}, -100},
		{"string owner id", map[string]any{"owner_chat_id": " -100 "}, -100},
		{"int64", map[string]any{"owner_chat_id": int64(-5)}, -5},
		{"int", map[string]any{"owner_chat_id": -5}, -5},
		{"chat_id fallback", map[string]any{"chat_id": "-200"}, -200},
		{"invalid owner falls to chat_id", map[string]any{"owner_chat_id": "abc", "chat_id": "-200"}, -200},
		{"zero owner falls to chat_id", map[string]any{"owner_chat_id": float64(0), "chat_id": "-200"}, -200},
		{"nil owner", map[string]any{"owner_chat_id": nil, "chat_id": "-200"}, -200},
		{"fraction", map[string]any{"owner_chat_id": 1.5}, 0},
		{"overflow float", map[string]any{"owner_chat_id": float64(math.MaxInt64)}, 0},
		{"overflow string", map[string]any{"owner_chat_id": "9223372036854775808"}, 0},
		{"bool", map[string]any{"owner_chat_id": true}, 0},
		{"empty", map[string]any{}, 0},
		{"nil map", nil, 0},
	} {
		if got := TelegramOwnerChatID(tc.m); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, ok := range []map[string]any{nil, {}, {"owner_chat_id": nil}, {"owner_chat_id": "-1001"}, {"owner_chat_id": float64(-1001)}} {
		if err := ValidateOwnerChatID(ok); err != nil {
			t.Errorf("valid %v refused: %v", ok, err)
		}
	}
	for _, bad := range []map[string]any{{"owner_chat_id": "abc"}, {"owner_chat_id": float64(0)}, {"owner_chat_id": ""}, {"owner_chat_id": 1.5}, {"owner_chat_id": true}} {
		if err := ValidateOwnerChatID(bad); err == nil {
			t.Errorf("invalid %v accepted", bad)
		}
	}
}
