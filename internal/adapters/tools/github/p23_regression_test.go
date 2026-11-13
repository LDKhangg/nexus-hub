package githubtools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TD-3 (Phase 2 REQ-5): checks must expose a real full SHA that can be
// copied verbatim into a merge; no prefix, even 39 characters, is enough.
func TestP23_ChecksFullSHAAndExactMerge(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, supplied := range []string{sha, sha[:7], sha[:10], sha[:39], sha[:39] + "8"} {
		t.Run(supplied, func(t *testing.T) {
			api := &fakeAPI{resp: map[string]any{
				"GET /repos/o/r/pulls/7": strings.Replace(prJSON, "abc1234def", sha, 1),
				"GET /repos/o/r/commits/" + sha + "/check-runs?per_page=100": greenRuns,
				"GET /repos/o/r/commits/" + sha + "/status":                  `{"state":"success","statuses":[]}`,
				"PUT /repos/o/r/pulls/7/merge":                               `{"merged":true,"sha":"merged"}`,
			}}
			out, err := run(t, find(t, NewReadTools(api), "github_pr_checks"), grant(nil), `{"repo":"o/r","number":7}`)
			if err != nil || !strings.Contains(out.Text, "head "+sha+":") {
				t.Fatal("checks did not expose the complete head SHA")
			}
			input, err := json.Marshal(map[string]any{"repo": "o/r", "number": 7, "head_sha": supplied, "merge_method": "squash"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(nil), string(input))
			if supplied != sha {
				if code(err) != "head_sha_mismatch" || api.count("PUT") != 0 {
					t.Fatal("non-exact head was not refused before merge")
				}
				return
			}
			if err != nil || api.count("PUT") != 1 {
				t.Fatal("exact full head did not merge once")
			}
			body := api.calls[len(api.calls)-1].Body.(map[string]string)
			if body["sha"] != sha {
				t.Fatal("merge request did not pin the full checked SHA")
			}
		})
	}
}
