// Package github is a small stdlib GitHub REST client. Tokens never appear
// in errors or logs; error text carries only a safe code.
package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const (
	apiBase          = "https://api.github.com"
	maxResponseBytes = 2 << 20
)

// GitHubError is a safe, classified API failure.
type GitHubError struct {
	Code       string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *GitHubError) Error() string { return "github: " + e.Code }

// Client calls the REST API with per-call credentials.
type Client struct {
	http    *http.Client
	creds   Credentials
	baseURL string
}

func NewClient(httpc *http.Client, creds Credentials) *Client {
	return &Client{http: noRedirect(httpc), creds: creds, baseURL: apiBase}
}

// noRedirect copies the client and refuses to follow redirects, so a 301
// for a renamed repository surfaces as repo_moved.
func noRedirect(httpc *http.Client) *http.Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	cp := *httpc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

// Do performs one API request as account, scoped to repo with perms.
func (c *Client) Do(ctx context.Context, workspaceID string, account tenant.GitHubAccount, repo string, perms map[string]string, method, path string, body, out any) error {
	if c.creds == nil {
		return &GitHubError{Code: "missing_credentials"}
	}
	tok, err := c.creds.Token(ctx, workspaceID, account, repo, perms)
	if err != nil {
		return err
	}
	return doJSON(ctx, c.http, c.baseURL, method, path, "Bearer "+tok, body, out)
}

func transportErr(err error) error {
	var de *netguard.DeniedError
	if errors.As(err, &de) {
		return &GitHubError{Code: de.Code}
	}
	return &GitHubError{Code: "network_error", Retryable: true}
}

// statusErr classifies a non-2xx response; nil for success.
func statusErr(resp *http.Response) error {
	s := resp.StatusCode
	if s >= 200 && s < 300 {
		return nil
	}
	switch {
	case s == http.StatusTooManyRequests || (s == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")):
		return &GitHubError{Code: "rate_limited", Retryable: true, RetryAfter: retryAfter(resp)}
	case s >= 300 && s < 400:
		return &GitHubError{Code: "repo_moved"}
	case s == http.StatusUnauthorized:
		return &GitHubError{Code: "unauthorized"}
	case s == http.StatusForbidden:
		return &GitHubError{Code: "forbidden"}
	case s == http.StatusNotFound:
		return &GitHubError{Code: "not_found"}
	case s == http.StatusConflict || s == http.StatusUnprocessableEntity || s == http.StatusMethodNotAllowed:
		return &GitHubError{Code: "conflict"}
	case s >= 500:
		return &GitHubError{Code: "server_error", Retryable: true}
	}
	return &GitHubError{Code: "http_" + strconv.Itoa(s)}
}

func retryAfter(resp *http.Response) time.Duration {
	if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v >= 0 {
		return time.Duration(v) * time.Second
	}
	if v, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if d := time.Until(time.Unix(v, 0)); d > 0 {
			return d
		}
	}
	return 0
}
