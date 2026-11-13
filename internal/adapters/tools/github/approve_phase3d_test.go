package githubtools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

func approveCall(t *testing.T, api *fakeAPI, acc tenant.GitHubAccount) (tools.ToolOutput, error) {
	t.Helper()
	return NewApproveTool(api).Run(t.Context(), tools.Call{WorkspaceID: "ws", Account: &acc, Input: json.RawMessage(`{"account":"personal","repo":"o/r","number":7}`)})
}

func TestPhase3dApproveAdapter(t *testing.T) {
	t.Run("TC-35/new approval has no comment text", func(t *testing.T) {
		api := &fakeAPI{resp: map[string]any{
			"GET /repos/o/r/pulls/7":                             `{"state":"open","merged":false,"user":{"login":"alice"}}`,
			"GET /repos/o/r/pulls/7/reviews?page=1&per_page=100": `[]`,
			"POST /repos/o/r/pulls/7/reviews":                    `{"id":44,"state":"APPROVED","user":{"login":"owner"}}`,
		}}
		out, err := approveCall(t, api, tenant.GitHubAccount{Name: "personal", Type: "pat", Login: "owner"})
		if err != nil || !strings.Contains(out.Text, "outcome=approved") || !strings.Contains(out.Text, "review_id=44") {
			t.Fatalf("%q %v", out.Text, err)
		}
		if api.count("POST /repos/o/r/pulls/7/reviews") != 1 {
			t.Fatal("approval not posted once")
		}
		body := api.calls[len(api.calls)-1].Body.(map[string]string)
		if len(body) != 1 || body["event"] != "APPROVE" {
			t.Fatalf("approval body=%v", body)
		}
	})

	t.Run("TC-35/already approved is idempotent", func(t *testing.T) {
		api := &fakeAPI{resp: map[string]any{
			"GET /repos/o/r/pulls/7":                             `{"state":"open","user":{"login":"alice"}}`,
			"GET /repos/o/r/pulls/7/reviews?page=1&per_page=100": `[{"id":9,"state":"APPROVED","user":{"login":"OWNER"}}]`,
		}}
		out, err := approveCall(t, api, tenant.GitHubAccount{Name: "personal", Type: "pat", Login: "owner"})
		if err != nil || !strings.Contains(out.Text, "already_approved") || api.count("POST ") != 0 {
			t.Fatalf("%q %v calls=%v", out.Text, err, api.calls)
		}
	})

	for _, tc := range []struct {
		name    string
		account tenant.GitHubAccount
		pull    any
		code    string
	}{
		{"TC-36/app account", tenant.GitHubAccount{Name: "personal", Type: "app", Login: "bot"}, nil, CodeAccountNotPAT},
		{"TC-36/closed PR", tenant.GitHubAccount{Name: "personal", Type: "pat", Login: "owner"}, `{"state":"closed","user":{"login":"alice"}}`, CodePRClosed},
		{"TC-36/own PR", tenant.GitHubAccount{Name: "personal", Type: "pat", Login: "owner"}, `{"state":"open","user":{"login":"OWNER"}}`, CodeOwnPR},
		{"TC-36/missing PR", tenant.GitHubAccount{Name: "personal", Type: "pat", Login: "owner"}, &github.GitHubError{Code: "not_found"}, CodePRNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{resp: map[string]any{}}
			if tc.pull != nil {
				api.resp["GET /repos/o/r/pulls/7"] = tc.pull
			}
			_, err := approveCall(t, api, tc.account)
			var te *tools.ToolError
			if !errors.As(err, &te) || te.Code != tc.code {
				t.Fatalf("error=%#v", err)
			}
			if api.count("POST ") != 0 {
				t.Fatal("failure posted approval")
			}
		})
	}
}
