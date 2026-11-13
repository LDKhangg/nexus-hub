package clickupchat

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// This file holds the two ClickUp API v2 reads that behavior runs use next
// to Chat: a task's comments (the text of the comment that started a run)
// and the members of the token's workspaces (display names for reports).
// They share the client's request handling, so the same rules hold: the
// token is passed per call, and errors carry only a safe code.

// CodeCommentNotFound: the task (or its comments) does not exist or is not
// visible to the token.
const CodeCommentNotFound = "comment_not_found"

// v2TaskIDRe accepts ClickUp task IDs (the canonical ID, as webhooks send
// it).
var v2TaskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Comment is one task comment. Text is untrusted user text: it must never
// be logged, stored as trusted data or put in errors. Username is set by
// the user: callers clean and cap it before showing it.
type Comment struct {
	ID       string
	Text     string
	UserID   string
	Username string
	// DateMS is the comment time in Unix ms.
	DateMS int64
}

// Member is one user of a workspace. Username is set by the user: callers
// clean and cap it before showing it.
type Member struct {
	UserID   string
	Username string
}

// Team is one ClickUp workspace (team) the token can see, with its
// members.
type Team struct {
	ID      string
	Members []Member
}

// TaskComments returns one page of a task's comments, newest first (GET
// /api/v2/task/{task_id}/comment; 25 per page by ClickUp's default). The
// first page has startMS 0 and startID ""; the next page passes the date
// and ID of the last comment of the previous page.
func (c *HTTPClient) TaskComments(ctx context.Context, token, taskID string, startMS int64, startID string) ([]Comment, error) {
	if token == "" {
		return nil, &Error{Code: CodeMissingCredentials}
	}
	if !v2TaskIDRe.MatchString(taskID) || startMS < 0 || (startID != "" && !idRe.MatchString(startID)) || (startID == "") != (startMS == 0) {
		return nil, invalid()
	}
	path := "/api/v2/task/" + url.PathEscape(taskID) + "/comment"
	if startID != "" {
		q := url.Values{}
		q.Set("start", strconv.FormatInt(startMS, 10))
		q.Set("start_id", startID)
		path += "?" + q.Encode()
	}
	var resp struct {
		Comments []struct {
			ID          flexString `json:"id"`
			CommentText string     `json:"comment_text"`
			Date        flexInt    `json:"date"`
			User        struct {
				ID       flexString `json:"id"`
				Username string     `json:"username"`
			} `json:"user"`
		} `json:"comments"`
	}
	if err := c.do(ctx, token, http.MethodGet, path, nil, &resp, CodeCommentNotFound, false); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(resp.Comments))
	for _, d := range resp.Comments {
		out = append(out, Comment{
			ID: string(d.ID), Text: d.CommentText, UserID: string(d.User.ID), Username: d.User.Username, DateMS: int64(d.Date),
		})
	}
	return out, nil
}

// Teams returns the workspaces (teams) the token can see, with their
// members (GET /api/v2/team).
func (c *HTTPClient) Teams(ctx context.Context, token string) ([]Team, error) {
	if token == "" {
		return nil, &Error{Code: CodeMissingCredentials}
	}
	var resp struct {
		Teams []struct {
			ID      flexString `json:"id"`
			Members []struct {
				User struct {
					ID       flexString `json:"id"`
					Username string     `json:"username"`
				} `json:"user"`
			} `json:"members"`
		} `json:"teams"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/api/v2/team", nil, &resp, CodeNotFound, false); err != nil {
		return nil, err
	}
	out := make([]Team, 0, len(resp.Teams))
	for _, t := range resp.Teams {
		team := Team{ID: string(t.ID), Members: make([]Member, 0, len(t.Members))}
		for _, m := range t.Members {
			team.Members = append(team.Members, Member{UserID: string(m.User.ID), Username: m.User.Username})
		}
		out = append(out, team)
	}
	return out, nil
}
