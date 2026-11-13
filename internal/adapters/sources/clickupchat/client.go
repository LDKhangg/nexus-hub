// Package clickupchat is a small stdlib client for the ClickUp Chat API v3:
// it lists channels with new messages, reads their messages, and lists and
// adds message reactions. The caller passes the ClickUp token on every
// call; the client never stores it. Tokens, headers, request and response
// bodies and message content never appear in errors or logs: an error
// carries only a safe code.
package clickupchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
)

const (
	apiBase = "https://api.clickup.com"
	// maxResponseBytes caps every response body the client reads.
	maxResponseBytes = 4 << 20
	// pageLimit is the page size sent as the limit query parameter. The
	// v3 docs do not state a maximum for these endpoints; 50 is their
	// documented default, so it is always accepted.
	pageLimit = 50
	// maxCursorLen caps an opaque paging cursor passed back to ClickUp.
	maxCursorLen = 2048
)

// Channel types accepted by DMChannelsSince.
const (
	ChannelDM      = "DM"
	ChannelGroupDM = "GROUP_DM"
	ChannelChannel = "CHANNEL"
)

// Error codes. Every error the client returns is an *Error with one of
// these codes, or the context's error when the context ended.
const (
	// CodeUnauthorized: ClickUp answered 401 or 403.
	CodeUnauthorized = "chat_unauthorized"
	// CodeRateLimited: ClickUp answered 429; RetryAfter may be set.
	CodeRateLimited = "chat_rate_limited"
	// CodeUnavailable: network failure, egress denial, redirect, 5xx,
	// oversized or undecodable response.
	CodeUnavailable = "chat_unavailable"
	// CodeNotFound: the channel (or its messages) does not exist or is
	// not visible to the token.
	CodeNotFound = "not_found"
	// CodeMessageNotFound: the message of a reaction call does not exist.
	CodeMessageNotFound = "message_not_found"
	// CodeReactionExists: ClickUp refused the reaction because the token's
	// user already added it. Callers treat it as success.
	CodeReactionExists = "reaction_exists"
	// CodeReactionUnsupported: ClickUp refused the reaction for any other
	// 400 reason (an unknown emoji name, for example).
	CodeReactionUnsupported = "reaction_unsupported"
	// CodeBadRequest: any other 4xx answer.
	CodeBadRequest = "chat_bad_request"
	// CodeInvalidInput: an argument failed validation; no request was sent.
	CodeInvalidInput = "invalid_input"
	// CodeMissingCredentials: the token is empty; no request was sent.
	CodeMissingCredentials = "missing_credentials"
)

// Error is a safe, classified ClickUp Chat failure. It carries no token,
// header, body or message content.
type Error struct {
	Code string
	// Retryable is true for rate limits and transient failures.
	Retryable bool
	// RetryAfter is ClickUp's Retry-After for CodeRateLimited, 0 if absent.
	RetryAfter time.Duration
	// Denied is true when the egress guard refused the connection.
	Denied bool
}

func (e *Error) Error() string { return "clickupchat: " + e.Code }

// ErrorCode returns the code of an *Error in err's chain, or "" if none.
func ErrorCode(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// Page is one page of a cursor-paged list. NextCursor is empty on the last
// page.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// Channel is a chat channel as listed by DMChannelsSince.
type Channel struct {
	ID string
	// Type is "CHANNEL", "DM" or "GROUP_DM".
	Type string
	// Name is the channel's name as ClickUp lists it. It is set by users:
	// callers clean and cap it before showing it.
	Name string
	// LatestCommentAt is the newest message time in Unix ms, 0 if absent.
	LatestCommentAt int64
	Archived        bool
}

// Message is one chat message. Content is untrusted user text in markdown:
// it must never be logged, stored as trusted data or put in errors.
type Message struct {
	ID     string
	UserID string
	// Content is the message text (text/md).
	Content string
	// DateMS is the message time in Unix ms.
	DateMS        int64
	ChannelID     string
	ParentMessage string
	// Type is "message" or "post".
	Type string
}

// Reaction is one reaction on a message.
type Reaction struct {
	Reaction string
	UserID   string
	DateMS   int64
}

// Client is the ClickUp Chat API used by the poller and the reaction
// action. ws is the ClickUp workspace (team) ID and token the ClickUp
// personal token, passed per call.
type Client interface {
	// DMChannelsSince lists channels of the given types (default DM) that
	// have messages since sinceMS (Unix ms; 0 means no filter), one page
	// at a time, starting at cursor ("" for the first page). Closed
	// channels are included. When types holds ChannelChannel no type
	// filter is sent to ClickUp and every type comes back: callers always
	// filter the items by Type.
	DMChannelsSince(ctx context.Context, ws, token string, types []string, sinceMS int64, cursor string) (Page[Channel], error)
	// Messages returns one page of a channel's messages in the order
	// ClickUp sends them, which is not documented: callers sort.
	Messages(ctx context.Context, ws, token, channelID, cursor string) (Page[Message], error)
	// AddReaction adds reaction (a lower-case emoji name) to a message as
	// the token's user. An existing reaction yields CodeReactionExists.
	AddReaction(ctx context.Context, ws, token, messageID, reaction string) error
	// RemoveReaction removes reaction from a message as the token's user.
	// A 404 (the reaction or the message is already gone) is success.
	RemoveReaction(ctx context.Context, ws, token, messageID, reaction string) error
	// Reactions returns one page of a message's reactions.
	Reactions(ctx context.Context, ws, token, messageID, cursor string) (Page[Reaction], error)
}

// HTTPClient implements Client over HTTP. It follows no redirect.
type HTTPClient struct {
	http    *http.Client
	baseURL string
}

var _ Client = (*HTTPClient)(nil)

// New returns a client for api.clickup.com. httpc should be the server's
// egress-guarded client; nil means a plain client with a 30s timeout.
func New(httpc *http.Client) *HTTPClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	cp := *httpc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPClient{http: &cp, baseURL: apiBase}
}

// NewAt is New against another API base, such as a local test server.
func NewAt(baseURL string, httpc *http.Client) *HTTPClient {
	c := New(httpc)
	c.baseURL = strings.TrimRight(baseURL, "/")
	return c
}

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	reactionRe = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)
)

func invalid() error { return &Error{Code: CodeInvalidInput} }

func validCursor(c string) bool {
	if len(c) > maxCursorLen {
		return false
	}
	for _, r := range c {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' {
			return false
		}
	}
	return true
}

func checkCommon(ws, token, cursor string) error {
	if token == "" {
		return &Error{Code: CodeMissingCredentials}
	}
	if !idRe.MatchString(ws) || !validCursor(cursor) {
		return invalid()
	}
	return nil
}

func wsPath(ws string) string {
	return "/api/v3/workspaces/" + url.PathEscape(ws) + "/chat"
}

// DMChannelsSince implements Client.
func (c *HTTPClient) DMChannelsSince(ctx context.Context, ws, token string, types []string, sinceMS int64, cursor string) (Page[Channel], error) {
	if err := checkCommon(ws, token, cursor); err != nil {
		return Page[Channel]{}, err
	}
	if sinceMS < 0 {
		return Page[Channel]{}, invalid()
	}
	if len(types) == 0 {
		types = []string{ChannelDM}
	}
	seen := map[string]bool{}
	var list []string
	for _, t := range types {
		switch t {
		case ChannelDM, ChannelGroupDM, ChannelChannel:
		default:
			return Page[Channel]{}, invalid()
		}
		if !seen[t] {
			seen[t] = true
			list = append(list, t)
		}
	}
	q := url.Values{}
	if !seen[ChannelChannel] {
		// A probe of channel_types=CHANNEL did not return the documented
		// shape, while a listing without the filter returns every type.
		// So the filter is sent only for DM types, where it is known to
		// work; the caller filters by Type in every case.
		for _, t := range list {
			q.Add("channel_types", t)
		}
	}
	if sinceMS > 0 {
		q.Set("with_message_since", strconv.FormatInt(sinceMS, 10))
	}
	q.Set("include_closed", "true")
	q.Set("limit", strconv.Itoa(pageLimit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var resp struct {
		NextCursor string `json:"next_cursor"`
		Data       []struct {
			ID              flexString `json:"id"`
			Type            string     `json:"type"`
			Name            string     `json:"name"`
			LatestCommentAt flexInt    `json:"latest_comment_at"`
			Archived        bool       `json:"archived"`
		} `json:"data"`
	}
	if err := c.do(ctx, token, http.MethodGet, wsPath(ws)+"/channels?"+q.Encode(), nil, &resp, CodeNotFound, false); err != nil {
		return Page[Channel]{}, err
	}
	p := Page[Channel]{NextCursor: resp.NextCursor, Items: make([]Channel, 0, len(resp.Data))}
	for _, d := range resp.Data {
		p.Items = append(p.Items, Channel{ID: string(d.ID), Type: d.Type, Name: d.Name, LatestCommentAt: int64(d.LatestCommentAt), Archived: d.Archived})
	}
	return p, nil
}

// Messages implements Client.
func (c *HTTPClient) Messages(ctx context.Context, ws, token, channelID, cursor string) (Page[Message], error) {
	if err := checkCommon(ws, token, cursor); err != nil {
		return Page[Message]{}, err
	}
	if !idRe.MatchString(channelID) {
		return Page[Message]{}, invalid()
	}
	q := url.Values{}
	q.Set("content_format", "text/md")
	q.Set("limit", strconv.Itoa(pageLimit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var resp struct {
		NextCursor string `json:"next_cursor"`
		Data       []struct {
			ID            flexString `json:"id"`
			UserID        flexString `json:"user_id"`
			Content       string     `json:"content"`
			Date          flexInt    `json:"date"`
			ParentChannel flexString `json:"parent_channel"`
			ParentMessage flexString `json:"parent_message"`
			Type          string     `json:"type"`
		} `json:"data"`
	}
	path := wsPath(ws) + "/channels/" + url.PathEscape(channelID) + "/messages?" + q.Encode()
	if err := c.do(ctx, token, http.MethodGet, path, nil, &resp, CodeNotFound, false); err != nil {
		return Page[Message]{}, err
	}
	p := Page[Message]{NextCursor: resp.NextCursor, Items: make([]Message, 0, len(resp.Data))}
	for _, d := range resp.Data {
		p.Items = append(p.Items, Message{
			ID: string(d.ID), UserID: string(d.UserID), Content: d.Content, DateMS: int64(d.Date),
			ChannelID: string(d.ParentChannel), ParentMessage: string(d.ParentMessage), Type: d.Type,
		})
	}
	return p, nil
}

// AddReaction implements Client.
func (c *HTTPClient) AddReaction(ctx context.Context, ws, token, messageID, reaction string) error {
	if err := checkCommon(ws, token, ""); err != nil {
		return err
	}
	if !idRe.MatchString(messageID) || !reactionRe.MatchString(reaction) {
		return invalid()
	}
	path := wsPath(ws) + "/messages/" + url.PathEscape(messageID) + "/reactions"
	return c.do(ctx, token, http.MethodPost, path, map[string]string{"reaction": reaction}, nil, CodeMessageNotFound, true)
}

// RemoveReaction implements Client:
// DELETE /api/v3/workspaces/{ws}/chat/messages/{message_id}/reactions/{reaction}
// answers 204; a 404 means the reaction is already gone.
func (c *HTTPClient) RemoveReaction(ctx context.Context, ws, token, messageID, reaction string) error {
	if err := checkCommon(ws, token, ""); err != nil {
		return err
	}
	if !idRe.MatchString(messageID) || !reactionRe.MatchString(reaction) {
		return invalid()
	}
	path := wsPath(ws) + "/messages/" + url.PathEscape(messageID) + "/reactions/" + url.PathEscape(reaction)
	err := c.do(ctx, token, http.MethodDelete, path, nil, nil, CodeMessageNotFound, false)
	if ErrorCode(err) == CodeMessageNotFound {
		return nil
	}
	return err
}

// Reactions implements Client.
func (c *HTTPClient) Reactions(ctx context.Context, ws, token, messageID, cursor string) (Page[Reaction], error) {
	if err := checkCommon(ws, token, cursor); err != nil {
		return Page[Reaction]{}, err
	}
	if !idRe.MatchString(messageID) {
		return Page[Reaction]{}, invalid()
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(pageLimit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var resp struct {
		NextCursor string `json:"next_cursor"`
		Data       []struct {
			Reaction string     `json:"reaction"`
			UserID   flexString `json:"user_id"`
			Date     flexInt    `json:"date"`
		} `json:"data"`
	}
	path := wsPath(ws) + "/messages/" + url.PathEscape(messageID) + "/reactions?" + q.Encode()
	if err := c.do(ctx, token, http.MethodGet, path, nil, &resp, CodeMessageNotFound, false); err != nil {
		return Page[Reaction]{}, err
	}
	p := Page[Reaction]{NextCursor: resp.NextCursor, Items: make([]Reaction, 0, len(resp.Data))}
	for _, d := range resp.Data {
		p.Items = append(p.Items, Reaction{Reaction: d.Reaction, UserID: string(d.UserID), DateMS: int64(d.Date)})
	}
	return p, nil
}

// do sends one request and decodes a 2xx JSON answer into out (when not
// nil). notFound is the code for 404; reaction marks the add-reaction
// call, whose 400 answers are classified by the error message.
func (c *HTTPClient) do(ctx context.Context, token, method, path string, body, out any, notFound string, reaction bool) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return invalid()
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return invalid()
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var de *netguard.DeniedError
		if errors.As(err, &de) {
			return &Error{Code: CodeUnavailable, Denied: true}
		}
		return &Error{Code: CodeUnavailable, Retryable: true}
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	s := resp.StatusCode
	switch {
	case s >= 200 && s < 300:
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return &Error{Code: CodeUnauthorized}
	case s == http.StatusTooManyRequests:
		return &Error{Code: CodeRateLimited, Retryable: true, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case s == http.StatusNotFound:
		return &Error{Code: notFound}
	case s == http.StatusBadRequest && reaction:
		if alreadyExists(data) {
			return &Error{Code: CodeReactionExists}
		}
		return &Error{Code: CodeReactionUnsupported}
	case s >= 500:
		return &Error{Code: CodeUnavailable, Retryable: true}
	case s >= 300 && s < 400:
		return &Error{Code: CodeUnavailable}
	default:
		return &Error{Code: CodeBadRequest}
	}
	if rerr != nil {
		return &Error{Code: CodeUnavailable, Retryable: true}
	}
	if len(data) > maxResponseBytes {
		return &Error{Code: CodeUnavailable}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Code: CodeUnavailable}
		}
	}
	return nil
}

// alreadyExists reports whether a 400 body says the reaction already
// exists. It reads only the error text fields and matches conservatively:
// the text must mention both "already" and "exist". The text itself is
// never kept.
func alreadyExists(data []byte) bool {
	var e struct {
		Message string `json:"message"`
		Err     string `json:"err"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return false
	}
	for _, m := range []string{e.Message, e.Err, e.Error} {
		m = strings.ToLower(m)
		if strings.Contains(m, "already") && strings.Contains(m, "exist") {
			return true
		}
	}
	return false
}

// retryAfter parses a Retry-After value in seconds or as an HTTP date; 0
// when absent or invalid. It is capped at one hour.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		if n > 3600 {
			n = 3600
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d < 0 {
		return 0
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// flexString accepts a JSON string or number; any other value reads as "".
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = ""
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		*f = flexString(n.String())
	}
	return nil
}

// flexInt accepts a JSON integer or a numeric string; any other value
// reads as 0.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	*f = 0
	var n json.Number
	if json.Unmarshal(b, &n) != nil {
		var s string
		if json.Unmarshal(b, &s) != nil {
			return nil
		}
		n = json.Number(strings.TrimSpace(s))
	}
	if v, err := n.Int64(); err == nil {
		*f = flexInt(v)
	}
	return nil
}
