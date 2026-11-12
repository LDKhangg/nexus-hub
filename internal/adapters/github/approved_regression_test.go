package github

import (
	"context"
	"errors"
	"net/http"
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
