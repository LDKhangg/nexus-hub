package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TC-59 (REQ-F17), TC-60 (REQ-F17): verification is bound to login, token and workspace.
func TestApproved_PATCacheIdentityIsolation(t *testing.T) {
	g := newGH(t)
	g.login = "Alice"
	clock := time.Now()
	res := fakeResolver{"ws|gh_personal": "fake-pat-alice-123", "other|gh_personal": "fake-pat-other-456"}
	c, _ := newEnv(t, g, res, &recRedactor{}, func() time.Time { return clock })
	a := pat
	a.Login = "ALICE"
	do := func(ws string) error {
		return c.Do(context.Background(), ws, a, "o/r", nil, http.MethodGet, "/repos/o/r", nil, nil)
	}
	if err := do("ws"); err != nil {
		t.Fatal(err)
	}
	if err := do("ws"); err != nil {
		t.Fatal(err)
	}
	if g.count("GET /user") != 1 {
		t.Fatal("casefolded identity not cached")
	}
	a.Login = "Bob"
	err := do("ws")
	var ge *GitHubError
	if !errors.As(err, &ge) || ge.Code != "account_mismatch" {
		t.Fatal("cached Alice accepted as Bob")
	}
	if g.count("GET /repos/o/r") != 2 {
		t.Fatal("API effect with wrong expected login")
	}
	a.Login = "alice"
	if err := do("other"); err != nil {
		t.Fatal(err)
	}
	if g.count("GET /user") != 3 {
		t.Fatal("workspace shared verified cache")
	}
	res["ws|gh_personal"] = "fake-pat-rotated-789"
	if err := do("ws"); err != nil {
		t.Fatal(err)
	}
	if g.count("GET /user") != 4 {
		t.Fatal("rotated token not reverified")
	}
	clock = clock.Add(11 * time.Minute)
	if err := do("ws"); err != nil {
		t.Fatal(err)
	}
	if g.count("GET /user") != 5 {
		t.Fatal("expired PAT verification reused")
	}
}

// TC-20 (REQ-9): when the App cache expires, failed re-minting must not
// reuse the stale token, switch to a PAT, or reach the repository API.
func TestApproved_ExpiredAppRefreshFailureNeverFallsBack(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g := newGH(t)
			_, key := rsaKey(t)
			clock := time.Now()
			c, _ := newEnv(t, g, fakeResolver{"ws|gh_app_key": key, "ws|gh_personal": "fake-fallback-pat"}, &recRedactor{}, func() time.Time { return clock })
			do := func() error {
				return c.Do(context.Background(), "ws", appAccount(), "o/r", map[string]string{"contents": "read"}, "GET", "/repos/o/r", nil, nil)
			}
			if err := do(); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(61 * time.Minute)
			g.handler = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/app/installations/") {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(status)
					io.WriteString(w, `{"message":"fake-private-upstream-error"}`)
					return true
				}
				return false
			}
			err := do()
			want := map[int]string{401: "unauthorized", 403: "rate_limited", 429: "rate_limited", 500: "server_error"}[status]
			var ge *GitHubError
			if !errors.As(err, &ge) || ge.Code != want {
				t.Errorf("refresh error=%v want=%s", err, want)
			}
			if g.count("GET /repos/o/r") != 1 || g.count("GET /user") != 0 || g.count("POST /app/installations/") != 2 {
				t.Error("expired App refresh reused token or fell back to PAT")
			}
			if err != nil && strings.Contains(err.Error(), "fake-private-upstream-error") {
				t.Error("refresh error leaked provider body")
			}
		})
	}
}
