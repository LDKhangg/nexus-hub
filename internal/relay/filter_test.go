package relay

import (
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

func TestShouldDrop(t *testing.T) {
	cases := []struct {
		name       string
		event      domain.NormalizedEvent
		wantDrop   bool
		wantReason string
	}{
		{"my bot", domain.NormalizedEvent{FromOwnBot: true}, true, string(DropOwnBot)},
		{"comment thường", domain.NormalizedEvent{FromOwnBot: false}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, r := shouldDrop(tc.event)
			if d != tc.wantDrop || r != DropReason(tc.wantReason) {
				t.Errorf("shouldDrop() = (%v, %q), want (%v, %q)", d, r, tc.wantDrop, tc.wantReason)
			}
		})
	}
}
