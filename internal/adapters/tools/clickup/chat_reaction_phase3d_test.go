package clickuptools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

type reactionFake struct {
	reactions []clickupchat.Reaction
	addErr error
	adds int
}
func (f *reactionFake) DMChannelsSince(context.Context, string, string, []string, int64, string) (clickupchat.Page[clickupchat.Channel], error) { return clickupchat.Page[clickupchat.Channel]{}, nil }
func (f *reactionFake) Messages(context.Context, string, string, string, string) (clickupchat.Page[clickupchat.Message], error) { return clickupchat.Page[clickupchat.Message]{}, nil }
func (f *reactionFake) AddReaction(context.Context, string, string, string, string) error { f.adds++; return f.addErr }
func (f *reactionFake) RemoveReaction(context.Context, string, string, string, string) error { return nil }
func (f *reactionFake) Reactions(context.Context, string, string, string, string) (clickupchat.Page[clickupchat.Reaction], error) { return clickupchat.Page[clickupchat.Reaction]{Items: f.reactions}, nil }

func reactionRun(t *testing.T, f *reactionFake) (tools.ToolOutput, error) {
	t.Helper()
	r := &res{val: "fake-token"}
	return NewChatReactionTool(http.DefaultClient, r, f).Run(t.Context(), tools.Call{WorkspaceID: "ws", ClickUpTokenRef: "clickup", Input: json.RawMessage(`{"team_id":"123","message_id":"m1","reaction":"white_check_mark"}`)})
}

func TestPhase3dChatReactionAdapter(t *testing.T) {
	t.Run("TC-35/add reaction", func(t *testing.T) {
		f := &reactionFake{}
		out, err := reactionRun(t, f)
		if err != nil || f.adds != 1 || out.Text != "message_id=m1 reaction=white_check_mark outcome=reacted" { t.Fatalf("%q adds=%d err=%v", out.Text, f.adds, err) }
	})
	t.Run("TC-35/existing reaction is success", func(t *testing.T) {
		f := &reactionFake{addErr: &clickupchat.Error{Code: clickupchat.CodeReactionExists}}
		out, err := reactionRun(t, f)
		if err != nil || out.Text != "message_id=m1 reaction=white_check_mark outcome=already_reacted" { t.Fatalf("%q %v", out.Text, err) }
	})
	for _, tc := range []struct{name, source, want string}{
		{"TC-36/missing message", clickupchat.CodeMessageNotFound, CodeMessageNotFound},
		{"TC-36/unauthorized", clickupchat.CodeUnauthorized, CodeClickUpUnauthorized},
		{"TC-36/unsupported", clickupchat.CodeReactionUnsupported, CodeReactionUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &reactionFake{addErr: &clickupchat.Error{Code: tc.source}}
			_, err := reactionRun(t, f)
			var te *tools.ToolError
			if !errors.As(err, &te) || te.Code != tc.want { t.Fatalf("error=%#v", err) }
		})
	}
}
