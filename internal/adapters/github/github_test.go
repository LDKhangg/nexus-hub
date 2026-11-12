package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type fakeResolver map[string]string

func (f fakeResolver) Resolve(_ context.Context, ws, ref string) (string, error) {
	v, ok := f[ws+"|"+ref]
	if !ok {
		return "", &secrets.Error{Code: secrets.CodeMissingCredentials}
	}
	return v, nil
}

type recRedactor struct {
	mu   sync.Mutex
	vals []string
}

func (r *recRedactor) Add(v string) { r.mu.Lock(); r.vals = append(r.vals, v); r.mu.Unlock() }
func (r *recRedactor) has(v string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.vals {
		if x == v {
			return true
		}
	}
	return false
}

var pat = tenant.GitHubAccount{Name: "personal", Type: "pat", TokenRef: "gh_personal", Login: "LDKhangg"}

func appAccount() tenant.GitHubAccount {
	return tenant.GitHubAccount{Name: "workbot", Type: "app", AppID: "123", InstallationID: "456", PrivateKeyRef: "gh_app_key"}
}

func rsaKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return k, string(p)
}

type ghServer struct {
	*httptest.Server
	mu        sync.Mutex
	paths     []string
	auths     []string
	headers   []http.Header
	tokenReqs []map[string]any
	login     string
	mints     atomic.Int32
	expiresIn time.Duration
	handler   func(w http.ResponseWriter, r *http.Request) bool
}

func newGH(t *testing.T) *ghServer {
	g := &ghServer{login: "LDKhangg", expiresIn: time.Hour}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, r.Method+" "+r.URL.Path)
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		g.headers = append(g.headers, r.Header.Clone())
		g.mu.Unlock()
		if g.handler != nil && g.handler(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": g.login})
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.mu.Lock()
			g.tokenReqs = append(g.tokenReqs, body)
			g.mu.Unlock()
			n := g.mints.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installation" + strings.Repeat("x", 20) + string(rune('a'+n)), "expires_at": time.Now().Add(g.expiresIn).UTC().Format(time.RFC3339)})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *ghServer) count(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

func newEnv(t *testing.T, g *ghServer, res fakeResolver, red *recRedactor, now func() time.Time) (*Client, *credentials) {
	t.Helper()
	creds := NewCredentials(g.Client(), res, red, now).(*credentials)
	creds.baseURL = g.URL
	c := NewClient(g.Client(), creds)
	c.baseURL = g.URL
	return c, creds
}

func TestPAT_LoginVerified(t *testing.T) {
	g := newGH(t)
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_personal": "ghp_" + strings.Repeat("a", 36)}, &recRedactor{}, time.Now)
	for i := 0; i < 3; i++ {
		var out map[string]any
		if err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodGet, "/repos/o/r", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if g.count("GET /user") != 1 || g.count("GET /repos/o/r") != 3 {
		t.Fatalf("paths %v", g.paths)
	}
}

func TestPAT_AccountMismatch(t *testing.T) {
	g := newGH(t)
	g.login = "someone-else"
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_personal": "ghp_" + strings.Repeat("a", 36)}, &recRedactor{}, time.Now)
	err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodGet, "/repos/o/r", nil, nil)
	var ge *GitHubError
	if !errors.As(err, &ge) || ge.Code != "account_mismatch" || ge.Retryable {
		t.Fatalf("err %v", err)
	}
	if g.count("GET /repos") != 0 {
		t.Fatal("API call made with mismatched account")
	}
}

func TestPAT_MissingCredential(t *testing.T) {
	g := newGH(t)
	c, _ := newEnv(t, g, fakeResolver{}, &recRedactor{}, time.Now)
	err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodGet, "/repos/o/r", nil, nil)
	var ge *GitHubError
	if !errors.As(err, &ge) || ge.Code != "missing_credentials" || len(g.paths) != 0 {
		t.Fatalf("err %v paths %v", err, g.paths)
	}
}

func TestApp_JWTClaimsAndSignature(t *testing.T) {
	key, _ := rsaKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := signAppJWT("123", key, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if !strings.Contains(string(hdr), `"RS256"`) {
		t.Fatalf("header %s", hdr)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["iss"] != "123" || int64(claims["iat"].(float64)) != now.Unix()-60 || int64(claims["exp"].(float64)) != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims %v", claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
}

func TestApp_TokenDownScoped(t *testing.T) {
	g := newGH(t)
	_, keyPEM := rsaKey(t)
	red := &recRedactor{}
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_app_key": keyPEM}, red, time.Now)
	if err := c.Do(context.Background(), "ws", appAccount(), "acme/app", map[string]string{"contents": "read"}, http.MethodGet, "/repos/acme/app", nil, nil); err != nil {
		t.Fatal(err)
	}
	req := g.tokenReqs[0]
	if repos := req["repositories"].([]any); len(repos) != 1 || repos[0] != "app" {
		t.Fatalf("repositories %v", req)
	}
	if perms := req["permissions"].(map[string]any); perms["contents"] != "read" || len(perms) != 1 {
		t.Fatalf("permissions %v", req)
	}
	if g.paths[0] != "POST /app/installations/456/access_tokens" || !strings.HasPrefix(g.auths[0], "Bearer ey") {
		t.Fatalf("mint %v %v", g.paths, g.auths[0][:10])
	}
	if !strings.HasPrefix(g.auths[1], "Bearer ghs_installation") {
		t.Fatalf("api auth %q", g.auths[1])
	}
	if !red.has(strings.TrimPrefix(g.auths[1], "Bearer ")) {
		t.Fatal("installation token not registered for redaction")
	}
}

func TestApp_TokenCachedUntilExpiry(t *testing.T) {
	g := newGH(t)
	_, keyPEM := rsaKey(t)
	clock := time.Now()
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_app_key": keyPEM}, &recRedactor{}, func() time.Time { return clock })
	perms := map[string]string{"contents": "read"}
	do := func() {
		if err := c.Do(context.Background(), "ws", appAccount(), "acme/app", perms, http.MethodGet, "/repos/acme/app", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	do()
	do()
	if g.mints.Load() != 1 {
		t.Fatalf("mints %d", g.mints.Load())
	}
	// Different permissions are a different token.
	if err := c.Do(context.Background(), "ws", appAccount(), "acme/app", map[string]string{"contents": "write"}, http.MethodGet, "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if g.mints.Load() != 2 {
		t.Fatalf("mints %d", g.mints.Load())
	}
	clock = clock.Add(56 * time.Minute) // within 5 minutes of expiry
	do()
	if g.mints.Load() != 3 {
		t.Fatalf("token not refreshed: %d", g.mints.Load())
	}
}

func TestApp_BadKey(t *testing.T) {
	g := newGH(t)
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_app_key": "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----"}, &recRedactor{}, time.Now)
	err := c.Do(context.Background(), "ws", appAccount(), "acme/app", nil, http.MethodGet, "/x", nil, nil)
	var ge *GitHubError
	if !errors.As(err, &ge) || ge.Code != "invalid_app_key" || len(g.paths) != 0 {
		t.Fatalf("err %v", err)
	}
	if _, err := parseAppKey("garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if _, err := parseAppKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))); err != nil {
		t.Fatalf("pkcs8: %v", err)
	}
}

func TestClient_Headers(t *testing.T) {
	g := newGH(t)
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_personal": "ghp_" + strings.Repeat("a", 36)}, &recRedactor{}, time.Now)
	if err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodPost, "/repos/o/r/issues/1/comments", map[string]string{"body": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	h := g.headers[len(g.headers)-1]
	if h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") != "2022-11-28" || h.Get("User-Agent") != "nexus-hub" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", h)
	}
}

func TestClient_Errors(t *testing.T) {
	cases := []struct {
		status    int
		hdr       map[string]string
		body      string
		code      string
		retryable bool
	}{
		{401, nil, `{}`, "unauthorized", false},
		{403, nil, `{"message":"Resource not accessible"}`, "forbidden", false},
		{403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "0"}, `{"message":"API rate limit exceeded"}`, "rate_limited", true},
		{429, map[string]string{"Retry-After": "7"}, `{}`, "rate_limited", true},
		{404, nil, `{}`, "not_found", false},
		{409, nil, `{}`, "conflict", false},
		{422, nil, `{}`, "conflict", false},
		{500, nil, `{}`, "server_error", true},
		{301, map[string]string{"Location": "https://api.github.com/repositories/1"}, `{}`, "repo_moved", false},
	}
	for _, tc := range cases {
		g := newGH(t)
		g.handler = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user" {
				return false
			}
			for k, v := range tc.hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
			return true
		}
		c, _ := newEnv(t, g, fakeResolver{"ws|gh_personal": "ghp_" + strings.Repeat("a", 36)}, &recRedactor{}, time.Now)
		err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodGet, "/repos/o/r", nil, nil)
		var ge *GitHubError
		if !errors.As(err, &ge) || ge.Code != tc.code || ge.Retryable != tc.retryable {
			t.Errorf("%d: got %v", tc.status, err)
			continue
		}
		if tc.status == 429 && ge.RetryAfter != 7*time.Second {
			t.Errorf("retry after %v", ge.RetryAfter)
		}
		if strings.Contains(err.Error(), "Resource not accessible") {
			t.Error("body echoed")
		}
	}
}

func TestClient_TokenRegisteredForRedaction(t *testing.T) {
	g := newGH(t)
	red := &recRedactor{}
	tok := "ghp_" + strings.Repeat("z", 36)
	c, _ := newEnv(t, g, fakeResolver{"ws|gh_personal": tok}, red, time.Now)
	if err := c.Do(context.Background(), "ws", pat, "o/r", nil, http.MethodGet, "/repos/o/r", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !red.has(tok) {
		t.Fatal("PAT not registered")
	}
}

// The git workspace tools take the same credential source.
var _ codetools.TokenSource = Credentials(nil)
