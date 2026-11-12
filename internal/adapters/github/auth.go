package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Credentials returns an API token for an account, scoped to one repo where
// the account type supports it. Its signature matches codetools.TokenSource.
type Credentials interface {
	Token(ctx context.Context, workspaceID string, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error)
}

// SecretResolver resolves credential references (secrets.Resolver).
type SecretResolver interface {
	Resolve(ctx context.Context, workspaceID, ref string) (string, error)
}

// Redactor registers secret values for output redaction.
type Redactor interface{ Add(string) }

const (
	loginCacheTTL   = 10 * time.Minute
	tokenRefreshGap = 5 * time.Minute
	maxAuthBody     = 64 << 10
)

type expiring struct {
	value   string
	expires time.Time
}

type credentials struct {
	http     *http.Client
	resolver SecretResolver
	redactor Redactor
	now      func() time.Time
	baseURL  string

	mu     sync.Mutex
	logins map[string]time.Time // ws|ref|login|token -> verified until
	tokens map[string]expiring  // installation token cache
}

// NewCredentials builds the PAT / GitHub App credential source.
func NewCredentials(httpc *http.Client, resolver SecretResolver, redactor Redactor, now func() time.Time) Credentials {
	if now == nil {
		now = time.Now
	}
	return &credentials{http: noRedirect(httpc), resolver: resolver, redactor: redactor, now: now, baseURL: apiBase, logins: map[string]time.Time{}, tokens: map[string]expiring{}}
}

func (c *credentials) register(v string) {
	if c.redactor != nil && v != "" {
		c.redactor.Add(v)
	}
}

func (c *credentials) resolve(ctx context.Context, ws, ref string) (string, error) {
	if c.resolver == nil || ref == "" {
		return "", &GitHubError{Code: "missing_credentials"}
	}
	v, err := c.resolver.Resolve(ctx, ws, ref)
	if err != nil || v == "" {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		code := "missing_credentials"
		var se *secrets.Error
		if errors.As(err, &se) {
			code = se.Code
		}
		return "", &GitHubError{Code: code}
	}
	c.register(v)
	return v, nil
}

func (c *credentials) Token(ctx context.Context, ws string, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error) {
	switch account.Type {
	case "pat":
		return c.patToken(ctx, ws, account)
	case "app":
		return c.appToken(ctx, ws, account, repo, perms)
	}
	return "", &GitHubError{Code: "missing_credentials"}
}

// patToken verifies (cached 10 minutes) that the PAT belongs to the
// configured login, so a swapped secret cannot act as someone else.
func (c *credentials) patToken(ctx context.Context, ws string, account tenant.GitHubAccount) (string, error) {
	tok, err := c.resolve(ctx, ws, account.TokenRef)
	if err != nil {
		return "", err
	}
	// The expected login is part of the key: a verification for one login
	// never vouches for the same token configured under another login.
	key := ws + "|" + account.TokenRef + "|" + strings.ToLower(account.Login) + "|" + tok
	c.mu.Lock()
	until, ok := c.logins[key]
	c.mu.Unlock()
	if ok && c.now().Before(until) {
		return tok, nil
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := c.call(ctx, http.MethodGet, "/user", "Bearer "+tok, nil, &user); err != nil {
		return "", err
	}
	if account.Login == "" || !strings.EqualFold(user.Login, account.Login) {
		return "", &GitHubError{Code: "account_mismatch"}
	}
	c.mu.Lock()
	c.logins[key] = c.now().Add(loginCacheTTL)
	c.mu.Unlock()
	return tok, nil
}

// appToken mints an installation token limited to one repository and the
// requested permissions, cached until five minutes before it expires.
func (c *credentials) appToken(ctx context.Context, ws string, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error) {
	_, name, ok := strings.Cut(repo, "/")
	if !tenant.ValidRepo(repo) || !ok {
		return "", &GitHubError{Code: "invalid_repo"}
	}
	permKeys := make([]string, 0, len(perms))
	for k, v := range perms {
		permKeys = append(permKeys, k+"="+v)
	}
	sort.Strings(permKeys)
	cacheKey := strings.Join([]string{ws, account.AppID, account.InstallationID, repo, strings.Join(permKeys, ",")}, "|")
	c.mu.Lock()
	if t, ok := c.tokens[cacheKey]; ok && c.now().Before(t.expires.Add(-tokenRefreshGap)) {
		c.mu.Unlock()
		return t.value, nil
	}
	c.mu.Unlock()
	pemText, err := c.resolve(ctx, ws, account.PrivateKeyRef)
	if err != nil {
		return "", err
	}
	key, err := parseAppKey(pemText)
	if err != nil {
		return "", &GitHubError{Code: "invalid_app_key"}
	}
	jwt, err := signAppJWT(account.AppID, key, c.now())
	if err != nil {
		return "", &GitHubError{Code: "invalid_app_key"}
	}
	body := map[string]any{"repositories": []string{name}}
	if len(perms) > 0 {
		body["permissions"] = perms
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.call(ctx, http.MethodPost, "/app/installations/"+account.InstallationID+"/access_tokens", "Bearer "+jwt, body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", &GitHubError{Code: "server_error", Retryable: true}
	}
	c.register(out.Token)
	c.mu.Lock()
	c.tokens[cacheKey] = expiring{value: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// call is a minimal JSON request used by the credential flows.
func (c *credentials) call(ctx context.Context, method, path, auth string, body, out any) error {
	return doJSON(ctx, c.http, c.baseURL, method, path, auth, body, out)
}

func doJSON(ctx context.Context, hc *http.Client, base, method, path, auth string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &GitHubError{Code: "invalid_request"}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return &GitHubError{Code: "invalid_request"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "nexus-hub")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return transportErr(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if gerr := statusErr(resp); gerr != nil {
		return gerr
	}
	if out != nil && len(data) > 0 && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(data, out); err != nil {
			return &GitHubError{Code: "invalid_response"}
		}
	}
	return nil
}

// NewCredentialsAt is NewCredentials against another API base.
func NewCredentialsAt(baseURL string, httpc *http.Client, resolver SecretResolver, redactor Redactor, now func() time.Time) Credentials {
	c := NewCredentials(httpc, resolver, redactor, now).(*credentials)
	c.baseURL = baseURL
	return c
}
