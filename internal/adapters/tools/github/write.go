package githubtools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// BotPrefix marks every comment the agent posts.
const BotPrefix = "🤖 "

type commentTool struct {
	base
	san tools.Sanitizer
}

// NewWriteTools returns github_pr_comment.
func NewWriteTools(api API, s tools.Sanitizer) []tools.Tool {
	return []tools.Tool{&commentTool{base: base{api: api, spec: tools.ToolSpec{
		Name:        "github_pr_comment",
		Description: "Post a comment on a pull request or issue. Mentions are neutralized and the comment is marked as written by the bot.",
		InputSchema: schema(`,"body":{"type":"string","maxLength":4000}`, "body"),
		Tier:        tenant.TierWrite,
	}}, san: s}}
}

// botBody sanitizes and prefixes outbound comment text.
func botBody(s tools.Sanitizer, text string) string {
	text = strings.TrimSpace(text)
	if s != nil {
		text = s.CleanOutbound(text, true)
	}
	return BotPrefix + text
}

func validText(s string, max int) bool {
	s = strings.TrimSpace(s)
	return s != "" && utf8.RuneCountInString(s) <= max
}

func (t *commentTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	in, err := t.prepare(c, "body")
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if !validText(in.Body, maxCommentRunes) {
		return tools.ToolOutput{}, toolErr("invalid_input")
	}
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := t.do(ctx, c, in, permsComment, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", in.Repo, in.num()), map[string]string{"body": botBody(t.san, in.Body)}, &out); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: "comment posted: " + out.HTMLURL, Summary: fmt.Sprintf("comment %d on #%d", out.ID, in.num())}, nil
}
