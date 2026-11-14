package chatpoll

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/people"
)

type sightingRecorder struct {
	mu   sync.Mutex
	seen []people.Sighting
}

func (r *sightingRecorder) DisplayName(context.Context, string, string, string) (string, bool) {
	return "", false
}

func (r *sightingRecorder) Sighting(_ context.Context, s people.Sighting) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
	return nil
}

type commentAPIFake struct{ list []clickupchat.Comment }

func (f commentAPIFake) TaskComments(context.Context, string, string, int64, string) ([]clickupchat.Comment, error) {
	return f.list, nil
}

// TC-11 (REQ-2, REQ-6): the comment reader and the channel member lookup
// feed the people directory from data they already read, with ids and
// names only (never the comment text).
func TestPhase4LookupsFeedTheDirectory(t *testing.T) {
	t.Run("TC-11 comment sender is sighted", func(t *testing.T) {
		rec := &sightingRecorder{}
		r := &CommentReader{API: commentAPIFake{list: []clickupchat.Comment{{ID: "c1", Text: "PRIVATE COMMENT TEXT", UserID: "106", Username: "dinh"}}}, Secrets: directorySecrets{"n1/r": "tok"}, People: rec}
		c, err := r.TaskComment(context.Background(), "n1", "r", "task1", "c1")
		if err != nil || c.SenderID != "106" {
			t.Fatalf("comment=%+v err=%v", c, err)
		}
		if len(rec.seen) != 1 {
			t.Fatalf("sightings=%+v", rec.seen)
		}
		s := rec.seen[0]
		if s.WorkspaceID != "n1" || s.Platform != "clickup" || s.ExternalID != "106" || s.Username != "dinh" || s.Source != "auto" || s.IsSelf {
			t.Fatalf("sighting=%+v", s)
		}
		if strings.Contains(s.PlatformName+s.Username, "PRIVATE") {
			t.Fatalf("comment text in sighting: %+v", s)
		}
	})

	t.Run("TC-11 channel members are sighted", func(t *testing.T) {
		rec := &sightingRecorder{}
		d := &ChannelMemberDirectory{API: &channelAPIFake{}, Secrets: directorySecrets{"n1/r": "tok"}, People: rec, Pages: 5}
		if name, ok := d.ChannelMemberName(context.Background(), "n1", "r", "100", "c", "2"); !ok || name != "Two" {
			t.Fatalf("name=%q ok=%v", name, ok)
		}
		got := map[string]people.Sighting{}
		for _, s := range rec.seen {
			got[s.ExternalID] = s
		}
		if got["1"].PlatformName != "One" || got["2"].Username != "Two" || got["1"].WorkspaceID != "n1" || got["2"].Platform != "clickup" {
			t.Fatalf("sightings=%+v", rec.seen)
		}
	})
}
