package behavior

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// This file adds the triggering comment of a comment-triggered run to the
// run's context, and gives the display names of ClickUp users for the
// triggering message's "#meta" fragment.

// CommentTool names the triggering comment read in context notes.
const CommentTool = "clickup_comment"

// MaxDisplayNameRunes caps a display name (a user or channel name) in a
// context fragment.
const MaxDisplayNameRunes = 100

// commentIDRe accepts ClickUp comment IDs.
var commentIDRe = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// commentSource is the locator base of a comment's fragments: "#meta"
// (identifiers and the sender's display name) and "#text" (the untrusted
// comment text).
func commentSource(commentID string) string { return "clickup:comment/" + commentID }

// TaskComment is one ClickUp task comment as read at run time. Text is
// untrusted: it goes only into the run's context snapshot, as quoted
// context for the model. SenderName is set by the user.
type TaskComment struct {
	Text       string
	SenderID   string
	SenderName string
}

// CommentReader reads one comment of a ClickUp task at run time with the
// run's source credential (tokenRef). The comment is never put into
// trigger data, run-level outcomes, logs or errors. A failure is a
// *ChatReadError.
type CommentReader interface {
	TaskComment(ctx context.Context, workspaceID, tokenRef, taskID, commentID string) (TaskComment, error)
}

// MemberDirectory gives the display name of a ClickUp user in ClickUp
// workspace (team) teamID, read with the run's source credential
// (tokenRef). ok is false when the name is not known or cannot be read;
// a run then goes on without it.
type MemberDirectory interface {
	MemberName(ctx context.Context, workspaceID, tokenRef, teamID, userID string) (name string, ok bool)
}

// ChannelMemberDirectory gives the display name of a member of ClickUp
// Chat channel channelID in ClickUp workspace (team) teamID, read with the
// run's source credential (tokenRef). ok is false when the name is not
// known or cannot be read; a run then goes on without it.
type ChannelMemberDirectory interface {
	ChannelMemberName(ctx context.Context, workspaceID, tokenRef, teamID, channelID, userID string) (name string, ok bool)
}

// chatSenderName is the cleaned display name of the sender of a
// chat-triggered run's message, "" when unknown: first from the channel's
// members (the ChannelMemberDirectory), then from the workspace's member
// directory, whose listing can be empty.
func (w *RunWorker) chatSenderName(ctx context.Context, workspaceID, tokenRef string, t TriggerData) string {
	if w.d.ChannelMembers != nil && tokenRef != "" && chatTeamIDRe.MatchString(t.TeamID) && chatIDRe.MatchString(t.ChannelID) && chatTeamIDRe.MatchString(t.SenderID) {
		if name, ok := w.d.ChannelMembers.ChannelMemberName(ctx, workspaceID, tokenRef, t.TeamID, t.ChannelID, t.SenderID); ok {
			if name = CleanDisplayName(name); name != "" {
				return name
			}
		}
		if ctx.Err() != nil {
			return ""
		}
	}
	return w.memberName(ctx, workspaceID, tokenRef, t.TeamID, t.SenderID)
}

// memberName is the cleaned display name of a ClickUp user, "" when
// unknown.
func (w *RunWorker) memberName(ctx context.Context, workspaceID, tokenRef, teamID, userID string) string {
	if w.d.Members == nil || tokenRef == "" || !chatTeamIDRe.MatchString(teamID) || !chatTeamIDRe.MatchString(userID) {
		return ""
	}
	name, ok := w.d.Members.MemberName(ctx, workspaceID, tokenRef, teamID, userID)
	if !ok {
		return ""
	}
	return CleanDisplayName(name)
}

// readTriggerComment adds the triggering comment of a comment-triggered
// run to the context, before the behavior's reads: a "#meta" fragment
// with the comment and task identifiers and the sender, and a "#text"
// fragment with the untrusted comment text (capped at DefaultReadBytes,
// neutralized like every fetched text, redacted with the snapshot). The
// sender's name comes from the comment, or from the MemberDirectory when
// the comment has none. A transient failure retries the run; any other
// failure is noted for a behavior without a verdict; with a verdict it ends
// the model step without one. nil means go on.
func (w *RunWorker) readTriggerComment(ctx context.Context, col *collector, tokenRef, teamID string, run Run, t TriggerData, needsVerdict bool) *runOutcome {
	if w.d.Comments == nil || run.TriggerKind != TriggerCommentAdded || t.CommentID == "" {
		return nil
	}
	if !commentIDRe.MatchString(t.CommentID) || !taskIDRe.MatchString(t.TaskID) {
		col.note(CommentTool, "", CodeBadTarget)
		if needsVerdict {
			return runFail(CodeTriggerTextUnavailable)
		}
		return nil
	}
	c, err := w.d.Comments.TaskComment(ctx, run.WorkspaceID, tokenRef, t.TaskID, t.CommentID)
	if ctx.Err() != nil {
		return runRetry(CodeRunTimeout)
	}
	if err != nil {
		var ce *ChatReadError
		if errors.As(err, &ce) && ce.Transient {
			return runRetry(CodeReadTransient)
		}
		code := "tool_error"
		if ce != nil && codeRe.MatchString(ce.Code) {
			code = ce.Code
		}
		col.note(CommentTool, t.CommentID, code)
		if needsVerdict {
			return runFail(CodeTriggerTextUnavailable)
		}
		return nil
	}
	text, cut := capBytes(c.Text, DefaultReadBytes)
	if cut {
		col.note(CommentTool, t.CommentID, NoteTruncated)
	}
	sender := t.SenderID
	if sender == "" && chatTeamIDRe.MatchString(c.SenderID) {
		sender = c.SenderID
	}
	name := CleanDisplayName(c.SenderName)
	if name == "" {
		name = w.memberName(ctx, run.WorkspaceID, tokenRef, teamID, sender)
	}
	meta := []string{"comment_id: " + t.CommentID, "task_id: " + t.TaskID, "sender_id: " + sender}
	if name != "" {
		meta = append(meta, "sender_name: "+name)
	}
	for i := range meta {
		meta[i] = neutralizeDelimiters(meta[i])
	}
	base := commentSource(t.CommentID)
	col.add([]Fragment{
		{Source: base + "#meta", Tool: CommentTool, Lines: meta},
		{Source: base + "#text", Tool: CommentTool, Lines: lines(text)},
	}, CommentTool, t.CommentID)
	return nil
}

// CleanDisplayName makes a user-set name (a ClickUp user or channel name)
// safe for one context line or report line: control and invisible format
// characters (bidi overrides, zero-width) are dropped, runs of white space
// become one space, the result is trimmed and capped at
// MaxDisplayNameRunes runes.
func CleanDisplayName(name string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range name {
		if n >= MaxDisplayNameRunes {
			break
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if space {
			if n+1 >= MaxDisplayNameRunes {
				break
			}
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
