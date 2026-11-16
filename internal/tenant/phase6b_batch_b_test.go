package tenant

import "testing"

// TC-B1-R3 (review R3): the thread id belongs to chat_id; it is dropped when
// the owner chat differs, kept when it is the same chat or no owner chat is set.
func TestPhase6bB1_OwnerTargetConfigThread(t *testing.T) {
	const tok = "TOKEN"
	for _, tc := range []struct {
		name       string
		m          map[string]any
		wantChat   string
		wantThread bool
	}{
		{"owner differs: thread dropped", map[string]any{"chat_id": "-100", "owner_chat_id": float64(-200), "message_thread_id": float64(9)}, "-200", false},
		{"owner only, no chat_id: thread dropped", map[string]any{"owner_chat_id": "-200", "message_thread_id": float64(9)}, "-200", false},
		{"owner equals chat_id: thread kept", map[string]any{"chat_id": "-200", "owner_chat_id": float64(-200), "message_thread_id": float64(9)}, "-200", true},
		{"owner unset: thread kept", map[string]any{"chat_id": "-100", "message_thread_id": float64(9)}, "-100", true},
		{"owner invalid falls to chat_id: thread kept", map[string]any{"chat_id": "-100", "owner_chat_id": "abc", "message_thread_id": float64(9)}, "-100", true},
		{"owner differs, no thread", map[string]any{"chat_id": "-100", "owner_chat_id": float64(-200)}, "-200", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.m["bot_token_env"] = tok
			in := map[string]any{}
			for k, v := range tc.m {
				in[k] = v
			}
			got := OwnerTargetConfig(tc.m)
			cfg, err := ParseTelegramTargetConfig(got)
			if err != nil || cfg.ChatID != tc.wantChat || (cfg.MessageThreadID != nil) != tc.wantThread {
				t.Fatalf("cfg=%+v err=%v", cfg, err)
			}
			for k, v := range in { // the stored config is never changed
				if tc.m[k] != v {
					t.Fatalf("input changed at %q", k)
				}
			}
			if len(tc.m) != len(in) {
				t.Fatal("input gained or lost keys")
			}
		})
	}
}
