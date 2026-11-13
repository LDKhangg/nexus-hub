package chatpoll

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
)

func TestPhase3EChatHelpers_ParseOnlyExactUserMentions(t *testing.T) {
	// TC-72, TC-73, TC-78, TC-79 (REQ-E8, REQ-E10).
	text := "[@A](#user_mention#7) [@A](#user_mention#7) [@G](#task_user_group_mention#8) " +
		"[@bad](#user_mention#abc) [@B](#user_mention#9)"
	ids := TaggedUserIDs(text)
	if len(ids) != 2 || ids[0] != "7" || ids[1] != "9" {
		t.Fatalf("ids=%v", ids)
	}
	if got := CleanChannelName("  Team\u202e\u200b\n  Channel  "); got != "Team Channel" {
		t.Fatalf("channel=%q", got)
	}
}

type directorySecrets map[string]string

func (s directorySecrets) Resolve(_ context.Context, ws, ref string) (string, error) {
	return s[ws+"/"+ref], nil
}

type teamAPIFake struct {
	mu     sync.Mutex
	calls  int
	block  chan struct{}
	called chan struct{}
}

func (f *teamAPIFake) Teams(ctx context.Context, token string) ([]clickupchat.Team, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.called != nil {
		f.called <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []clickupchat.Team{{ID: token, Members: []clickupchat.Member{{UserID: "7", Username: "Alice"}}}}, nil
}

func TestPhase3EMemberDirectory_TTLTenantIsolationAndNoHTTPUnderLock(t *testing.T) {
	// TC-80, TC-81 (REQ-E10).
	now := time.Unix(100, 0)
	api := &teamAPIFake{}
	d := &MemberDirectory{API: api, Secrets: directorySecrets{"n1/r": "team1", "n2/r": "team2"}, TTL: time.Minute, MaxTeams: 3, Now: func() time.Time { return now }}
	if name, ok := d.MemberName(context.Background(), "n1", "r", "team1", "7"); !ok || name != "Alice" {
		t.Fatalf("first=%q/%v", name, ok)
	}
	if _, ok := d.MemberName(context.Background(), "n2", "r", "team1", "7"); ok {
		t.Fatal("tenant cache leaked")
	}
	api.mu.Lock()
	calls := api.calls
	api.mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if _, ok := d.MemberName(context.Background(), "n1", "r", "team1", "7"); !ok {
		t.Fatal("cache miss")
	}
	api.mu.Lock()
	cachedCalls := api.calls
	api.mu.Unlock()
	if cachedCalls != calls {
		t.Fatal("cache did HTTP")
	}
	now = now.Add(2 * time.Minute)
	d.MemberName(context.Background(), "n1", "r", "team1", "7")
	api.mu.Lock()
	expiredCalls := api.calls
	api.mu.Unlock()
	if expiredCalls != calls+1 {
		t.Fatalf("TTL did not expire: %d", expiredCalls)
	}

	blocked := &teamAPIFake{block: make(chan struct{}), called: make(chan struct{}, 2)}
	d2 := &MemberDirectory{API: blocked, Secrets: directorySecrets{"a/r": "a", "b/r": "b"}}
	done := make(chan struct{})
	go func() { d2.MemberName(context.Background(), "a", "r", "a", "7"); close(done) }()
	<-blocked.called
	second := make(chan struct{})
	go func() { d2.MemberName(context.Background(), "b", "r", "b", "7"); close(second) }()
	select {
	case <-blocked.called:
	case <-time.After(time.Second):
		t.Fatal("directory held lock over HTTP")
	}
	close(blocked.block)
	<-done
	<-second
}

type channelAPIFake struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *channelAPIFake) ChannelMembers(_ context.Context, ws, token, channel, cursor string, limit int) (clickupchat.Page[clickupchat.ChannelMember], error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[ws+"/"+channel]++
	f.mu.Unlock()
	if cursor == "" {
		return clickupchat.Page[clickupchat.ChannelMember]{Items: []clickupchat.ChannelMember{{ID: "1", Name: "One"}}, NextCursor: "next"}, nil
	}
	return clickupchat.Page[clickupchat.ChannelMember]{Items: []clickupchat.ChannelMember{{ID: "2", Username: "Two"}}}, nil
}

func TestPhase3EChannelMemberDirectory_PagesAndTenantKeys(t *testing.T) {
	// TC-80, TC-81 (REQ-E10).
	api := &channelAPIFake{}
	d := &ChannelMemberDirectory{API: api, Secrets: directorySecrets{"n1/r": "tok1", "n2/r": "tok2"}, Pages: 5, MaxChannels: 2}
	if name, ok := d.ChannelMemberName(context.Background(), "n1", "r", "100", "c", "2"); !ok || name != "Two" {
		t.Fatalf("paged=%q/%v", name, ok)
	}
	if _, ok := d.ChannelMemberName(context.Background(), "n2", "r", "100", "c", "missing"); ok {
		t.Fatal("unexpected name")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.calls["100/c"] < 4 {
		t.Fatalf("tenant lookup reused another tenant's cache: %v", api.calls)
	}
}
