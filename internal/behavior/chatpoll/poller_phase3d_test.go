package chatpoll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

type pollTargets []Target
func (p pollTargets) ChatPollTargets(context.Context) ([]Target, error) { return p, nil }
type pollVersions struct{ v []behavior.Version }
func (p pollVersions) EnabledVersions(context.Context, string) ([]behavior.Version, error) { return p.v, nil }
type pollSecrets struct{}
func (pollSecrets) Resolve(context.Context, string, string) (string, error) { return "fake-token", nil }

type pollState struct { state behavior.ChatPollState; fresh bool; advances []int64; errors []string; claimed bool }
func (s *pollState) InitChatPoll(_ context.Context, ws, in, team string, now int64) (behavior.ChatPollState, bool, error) {
	if s.state.TeamID == "" { s.state = behavior.ChatPollState{WorkspaceID: ws, IntegrationID: in, TeamID: team, CursorMS: now}; f := s.fresh; s.fresh = false; return s.state, f, nil }
	return s.state, false, nil
}
func (s *pollState) GetChatPoll(context.Context, string, string) (behavior.ChatPollState, error) { return s.state, nil }
func (s *pollState) AdvanceChatCursor(_ context.Context, _, _, _ string, cursor int64) (int64, error) { s.advances = append(s.advances, cursor); if cursor > s.state.CursorMS { s.state.CursorMS = cursor }; return s.state.CursorMS, nil }
func (s *pollState) RecordChatPollError(_ context.Context, _, _, code string, now time.Time, base, max time.Duration) (behavior.ChatPollState, error) { s.errors = append(s.errors, code); s.state.ErrorStreak++; if code == CodeUnauthorized { s.state.UnauthorizedStreak++ } else { s.state.UnauthorizedStreak = 0 }; until := now.Add(base); s.state.BackoffUntil = &until; return s.state, nil }
func (s *pollState) ResetChatPollErrors(context.Context, string, string) error { s.state.ErrorStreak = 0; s.state.BackoffUntil = nil; return nil }
func (s *pollState) ClaimChatUnauthorizedNotice(_ context.Context, _, _ string, min int, _ time.Time) (bool, error) { if s.claimed || s.state.UnauthorizedStreak < min { return false, nil }; s.claimed = true; return true, nil }

type pollRuns struct { events [][]behavior.Event; err error }
func (r *pollRuns) StartChatRuns(_ context.Context, _ string, ev []behavior.Event) (int, error) { r.events = append(r.events, ev); return len(ev), r.err }
type pollChat struct { channels clickupchat.Page[clickupchat.Channel]; messages clickupchat.Page[clickupchat.Message]; err error }
func (f *pollChat) DMChannelsSince(context.Context, string, string, []string, int64, string) (clickupchat.Page[clickupchat.Channel], error) { if f.err != nil { return clickupchat.Page[clickupchat.Channel]{}, f.err }; return f.channels, nil }
func (f *pollChat) Messages(context.Context, string, string, string, string) (clickupchat.Page[clickupchat.Message], error) { if f.err != nil { return clickupchat.Page[clickupchat.Message]{}, f.err }; return f.messages, nil }
type ownerChatFake struct{}
func (ownerChatFake) OwnerChat(context.Context, string, behavior.TriggerData) (behavior.ChatRef, error) { return behavior.ChatRef{WorkspaceID: "ws", IntegrationID: "tg", ChatID: 1}, nil }
type senderFake struct{ sent []string }
func (s *senderFake) SendMessage(_ context.Context, _ behavior.ChatRef, m behavior.OutMessage) (int64, error) { s.sent = append(s.sent, m.Text); return int64(len(s.sent)), nil }

func pollVersion(t *testing.T) behavior.Version {
	t.Helper()
	d := behavior.Doc{Schema: 1, Name: "chat", Triggers: []behavior.TriggerDoc{{Type: behavior.TriggerChatMessage, Filter: behavior.FilterDoc{Channel: behavior.StringList{behavior.ChannelDM}}}}, Instructions: "summarize", Actions: map[string][]behavior.ActionDoc{string(behavior.ClassNotify): {{Type: behavior.ActionTelegramMessage}}}}
	b, err := behavior.Encode(d); if err != nil { t.Fatal(err) }
	return behavior.Version{Name: "chat", Version: 1, Document: b, Vars: json.RawMessage(`{}`)}
}

func TestPollerPhase3dCursorRestartBackoffAndPrivacy(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	target := Target{WorkspaceID: "ws", IntegrationID: "in", TeamID: "123", TokenRef: "ref"}
	t.Run("TC-27 and TC-28/first poll initializes, restart dedupes, bad items drop", func(t *testing.T) {
		state := &pollState{fresh: true}
		runs := &pollRuns{}
		chat := &pollChat{channels: clickupchat.Page[clickupchat.Channel]{Items: []clickupchat.Channel{{ID: "c", Type: "DM", LatestCommentAt: now.Add(time.Second).UnixMilli()}}}, messages: clickupchat.Page[clickupchat.Message]{Items: []clickupchat.Message{
			{ID: "m1", UserID: "2", ChannelID: "c", Type: "message", DateMS: now.Add(time.Second).UnixMilli(), Content: "https://github.com/o/r/pull/1"},
			{ID: "post", UserID: "2", ChannelID: "c", Type: "post", DateMS: now.Add(2*time.Second).UnixMilli()},
			{ID: "own", UserID: "1", ChannelID: "c", Type: "message", DateMS: now.Add(3*time.Second).UnixMilli()},
		}}}
		p := New(Deps{Targets: pollTargets{target}, Versions: pollVersions{[]behavior.Version{pollVersion(t)}}, State: state, Runs: runs, Chat: chat, Secrets: pollSecrets{}}, Settings{Now: func() time.Time { return now }, OwnerClickUpUserID: "1", CursorLag: time.Nanosecond})
		p.PollOnce(t.Context())
		if len(runs.events) != 0 || len(state.advances) != 0 { t.Fatal("fresh poll processed history") }
		p.PollOnce(t.Context())
		if len(runs.events) != 1 || len(runs.events[0]) != 1 || runs.events[0][0].MessageID != "m1" { t.Fatalf("events=%#v", runs.events) }
		cursor := state.state.CursorMS
		p.PollOnce(t.Context())
		if state.state.CursorMS < cursor { t.Fatal("cursor moved backwards") }
	})

	t.Run("TC-31 and TC-32/safe error code backs off without content log", func(t *testing.T) {
		state := &pollState{state: behavior.ChatPollState{WorkspaceID: "ws", IntegrationID: "in", TeamID: "123", CursorMS: now.Add(-time.Minute).UnixMilli()}}
		private := "PRIVATE MESSAGE TOKEN"
		chat := &pollChat{err: errors.New(private)}
		var logs bytes.Buffer
		old := log.Writer(); log.SetOutput(&logs); defer log.SetOutput(old)
		p := New(Deps{Targets: pollTargets{target}, Versions: pollVersions{[]behavior.Version{pollVersion(t)}}, State: state, Runs: &pollRuns{}, Chat: chat, Secrets: pollSecrets{}}, Settings{Now: func() time.Time { return now }})
		p.PollOnce(t.Context())
		if len(state.errors) != 1 || state.errors[0] != CodeUnavailable || state.state.BackoffUntil == nil { t.Fatalf("state=%#v", state) }
		if strings.Contains(logs.String(), private) { t.Fatalf("private error logged: %s", logs.String()) }
		if got := errorCode(&clickupchat.Error{Code: clickupchat.CodeUnauthorized}); got != CodeUnauthorized { t.Fatalf("unauthorized=%q", got) }
	})

	t.Run("TC-31/three unauthorized failures send one notice", func(t *testing.T) {
		state := &pollState{state: behavior.ChatPollState{WorkspaceID: "ws", IntegrationID: "in", TeamID: "123"}}
		sender := &senderFake{}
		p := New(Deps{State: state, Chats: ownerChatFake{}, Messenger: sender}, Settings{Now: func() time.Time { return now }, Interval: 10 * time.Second})
		p.fail(context.Background(), target, CodeUnauthorized)
		p.fail(context.Background(), target, CodeUnauthorized)
		if len(sender.sent) != 0 { t.Fatalf("early notices=%v", sender.sent) }
		p.fail(context.Background(), target, CodeUnauthorized)
		p.fail(context.Background(), target, CodeUnauthorized)
		if len(sender.sent) != 1 || !strings.Contains(sender.sent[0], "3 lần") { t.Fatalf("notices=%v", sender.sent) }
	})

	t.Run("TC-28/run storage failure never advances cursor", func(t *testing.T) {
		state := &pollState{state: behavior.ChatPollState{WorkspaceID: "ws", IntegrationID: "in", TeamID: "123", CursorMS: now.Add(-time.Minute).UnixMilli()}}
		chat := &pollChat{channels: clickupchat.Page[clickupchat.Channel]{Items: []clickupchat.Channel{{ID: "c", Type: "DM", LatestCommentAt: now.UnixMilli()}}}, messages: clickupchat.Page[clickupchat.Message]{Items: []clickupchat.Message{{ID: "m", UserID: "2", ChannelID: "c", Type: "message", DateMS: now.UnixMilli()}}}}
		p := New(Deps{Targets: pollTargets{target}, Versions: pollVersions{[]behavior.Version{pollVersion(t)}}, State: state, Runs: &pollRuns{err: errors.New("storage down")}, Chat: chat, Secrets: pollSecrets{}}, Settings{Now: func() time.Time { return now }, CursorLag: time.Nanosecond})
		p.PollOnce(t.Context())
		if len(state.advances) != 0 { t.Fatalf("advanced=%v", state.advances) }
	})
}
