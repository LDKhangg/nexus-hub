package clickuptools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// ToolChatReaction is the name of the ClickUp Chat reaction tool.
const ToolChatReaction = "clickup_chat_reaction"

// Outcomes of clickup_chat_reaction, in its output ("outcome=<x>").
const (
	// OutcomeReacted: the reaction was added.
	OutcomeReacted = "reacted"
	// OutcomeAlreadyReacted: the token's user had already added this
	// reaction; nothing was added.
	OutcomeAlreadyReacted = "already_reacted"
)

// Failure codes of clickup_chat_reaction, besides invalid_input,
// missing_credentials, rate_limited and network_error.
const (
	CodeMessageNotFound     = "message_not_found"
	CodeClickUpUnauthorized = "clickup_unauthorized"
	CodeReactionUnsupported = "reaction_unsupported"
	CodeChatBadRequest      = "chat_bad_request"
	CodeChatUnavailable     = "chat_unavailable"
)

// maxReactionPages caps the reaction pages read before adding one.
const maxReactionPages = 10

var (
	teamIDRe   = regexp.MustCompile(`^[0-9]{1,20}$`)
	chatIDRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	reactionRe = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)
)

var chatReactionSpec = tools.ToolSpec{
	Name:        ToolChatReaction,
	Description: "Add a reaction (a lower-case emoji name) to a ClickUp Chat message as the ClickUp token's user, unless that user already added it.",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"team_id":{"type":"string","pattern":"^[0-9]{1,20}$"},` +
		`"message_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,128}$"},` +
		`"reaction":{"type":"string","pattern":"^[a-z0-9_+-]{1,64}$"}},` +
		`"required":["team_id","message_id","reaction"]}`),
	Tier: tenant.TierWrite,
}

// chatReactionTool is clickup_chat_reaction. It is ActionOnly: only the
// behavior engine's run-level action executor may run it.
type chatReactionTool struct {
	c    client
	chat clickupchat.Client
}

var _ tools.ActionOnly = (*chatReactionTool)(nil)

// NewChatReactionTool returns clickup_chat_reaction, authenticated with the
// call's clickup_token_ref. Input:
// {"team_id":"<ClickUp workspace id>","message_id":"<id>","reaction":"<name>"}.
// Output: "message_id=<id> reaction=<name> outcome=reacted|already_reacted".
// chat nil uses the ClickUp Chat v3 client over httpc.
func NewChatReactionTool(httpc *http.Client, resolver SecretResolver, chat clickupchat.Client) tools.Tool {
	if chat == nil {
		chat = clickupchat.New(httpc)
	}
	return &chatReactionTool{c: newClient(httpc, resolver), chat: chat}
}

func (t *chatReactionTool) Spec() tools.ToolSpec { return chatReactionSpec }
func (t *chatReactionTool) UsesNetwork() bool    { return true }
func (t *chatReactionTool) ActionOnly()          {}

type chatReactionInput struct {
	TeamID    string `json:"team_id"`
	MessageID string `json:"message_id"`
	Reaction  string `json:"reaction"`
}

func parseChatReactionInput(raw json.RawMessage) (chatReactionInput, error) {
	var in chatReactionInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	if !teamIDRe.MatchString(in.TeamID) || !chatIDRe.MatchString(in.MessageID) || !reactionRe.MatchString(in.Reaction) {
		return in, toolErr("invalid_input", false)
	}
	return in, nil
}

func (t *chatReactionTool) Run(ctx context.Context, call tools.Call) (tools.ToolOutput, error) {
	in, err := parseChatReactionInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	token, err := t.c.token(ctx, call.WorkspaceID, call.ClickUpTokenRef)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	done := func(outcome string) (tools.ToolOutput, error) {
		text := "message_id=" + in.MessageID + " reaction=" + in.Reaction + " outcome=" + outcome
		return tools.ToolOutput{Text: text, Summary: text}, nil
	}

	// Who already added this reaction (a retry after a crash finds its own).
	var reactors []string
	cursor := ""
	for page := 0; page < maxReactionPages; page++ {
		p, err := t.chat.Reactions(ctx, in.TeamID, token, in.MessageID, cursor)
		if err != nil {
			return tools.ToolOutput{}, mapChatErr(err)
		}
		for _, r := range p.Items {
			if strings.EqualFold(r.Reaction, in.Reaction) && r.UserID != "" {
				reactors = append(reactors, r.UserID)
			}
		}
		if p.NextCursor == "" || p.NextCursor == cursor {
			break
		}
		cursor = p.NextCursor
	}
	if len(reactors) > 0 {
		// Only then is the token's own user needed. A failed lookup is not
		// fatal: adding an existing reaction is answered as reaction_exists.
		if me, err := t.c.selfID(ctx, token); err == nil {
			for _, id := range reactors {
				if id == me {
					return done(OutcomeAlreadyReacted)
				}
			}
		} else if ctx.Err() != nil {
			return tools.ToolOutput{}, ctx.Err()
		}
	}

	err = t.chat.AddReaction(ctx, in.TeamID, token, in.MessageID, in.Reaction)
	if clickupchat.ErrorCode(err) == clickupchat.CodeReactionExists {
		return done(OutcomeAlreadyReacted)
	}
	if err != nil {
		return tools.ToolOutput{}, mapChatErr(err)
	}
	return done(OutcomeReacted)
}

// selfID returns the ClickUp user ID of the token (GET /api/v2/user).
func (c client) selfID(ctx context.Context, token string) (string, error) {
	data, err := c.do(ctx, token, http.MethodGet, "/api/v2/user", nil, false)
	if err != nil {
		return "", err
	}
	var resp struct {
		User struct{ ID any } `json:"user"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", toolErr("invalid_response", false)
	}
	id := idString(resp.User.ID)
	if id == "" {
		return "", toolErr("invalid_response", false)
	}
	return id, nil
}

// mapChatErr maps a ClickUp Chat client error to a tool error. No failure
// is ambiguous: a retry reads the reactions first and an existing reaction
// counts as success, so repeating the call can never add a second one.
func mapChatErr(err error) error {
	var ce *clickupchat.Error
	if !errors.As(err, &ce) {
		return err // the context ended; the executor classifies it
	}
	switch ce.Code {
	case clickupchat.CodeUnauthorized:
		return toolErr(CodeClickUpUnauthorized, false)
	case clickupchat.CodeRateLimited:
		return &tools.ToolError{Code: "rate_limited", Retryable: true}
	case clickupchat.CodeUnavailable:
		switch {
		case ce.Denied:
			return &tools.ToolError{Code: CodeChatUnavailable, Denied: true}
		case ce.Retryable:
			return &tools.ToolError{Code: "network_error", Retryable: true}
		}
		return toolErr(CodeChatUnavailable, false)
	case clickupchat.CodeMessageNotFound, clickupchat.CodeNotFound:
		return toolErr(CodeMessageNotFound, false)
	case clickupchat.CodeReactionUnsupported:
		return toolErr(CodeReactionUnsupported, false)
	case clickupchat.CodeInvalidInput:
		return toolErr("invalid_input", false)
	case clickupchat.CodeMissingCredentials:
		return toolErr("missing_credentials", false)
	}
	return toolErr(CodeChatBadRequest, false)
}

// ToolChatReactionRemove is the name of the tool that removes a ClickUp
// Chat reaction (undo of clickup_chat_reaction).
const ToolChatReactionRemove = "clickup_chat_reaction_remove"

// OutcomeRemoved: the reaction is no longer on the message (removed now,
// or already gone). The only outcome of clickup_chat_reaction_remove.
const OutcomeRemoved = "removed"

var chatReactionRemoveSpec = tools.ToolSpec{
	Name:        ToolChatReactionRemove,
	Description: "Remove a reaction (a lower-case emoji name) that the ClickUp token's user added to a ClickUp Chat message.",
	InputSchema: chatReactionSpec.InputSchema,
	Tier:        tenant.TierWrite,
}

// chatReactionRemoveTool is clickup_chat_reaction_remove. It is
// ActionOnly: only the behavior engine (undo of a run-level reaction) may
// run it.
type chatReactionRemoveTool struct {
	c    client
	chat clickupchat.Client
}

var _ tools.ActionOnly = (*chatReactionRemoveTool)(nil)

// NewChatReactionRemoveTool returns clickup_chat_reaction_remove,
// authenticated with the call's clickup_token_ref. Input as
// clickup_chat_reaction. Output: "message_id=<id> reaction=<name>
// outcome=removed". Removing a reaction that is already gone is success,
// so a retried undo is idempotent. chat nil uses the ClickUp Chat v3
// client over httpc.
func NewChatReactionRemoveTool(httpc *http.Client, resolver SecretResolver, chat clickupchat.Client) tools.Tool {
	if chat == nil {
		chat = clickupchat.New(httpc)
	}
	return &chatReactionRemoveTool{c: newClient(httpc, resolver), chat: chat}
}

func (t *chatReactionRemoveTool) Spec() tools.ToolSpec { return chatReactionRemoveSpec }
func (t *chatReactionRemoveTool) UsesNetwork() bool    { return true }
func (t *chatReactionRemoveTool) ActionOnly()          {}

func (t *chatReactionRemoveTool) Run(ctx context.Context, call tools.Call) (tools.ToolOutput, error) {
	in, err := parseChatReactionInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	token, err := t.c.token(ctx, call.WorkspaceID, call.ClickUpTokenRef)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if err := t.chat.RemoveReaction(ctx, in.TeamID, token, in.MessageID, in.Reaction); err != nil {
		return tools.ToolOutput{}, mapChatErr(err)
	}
	text := "message_id=" + in.MessageID + " reaction=" + in.Reaction + " outcome=" + OutcomeRemoved
	return tools.ToolOutput{Text: text, Summary: text}, nil
}
