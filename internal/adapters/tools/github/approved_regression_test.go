package githubtools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/github"
)

func approvedRuns(n, total int, conclusion string) string {
	runs := make([]map[string]string, n)
	for i := range runs {
		runs[i] = map[string]string{"name": fmt.Sprintf("check-%d", i), "status": "completed", "conclusion": conclusion}
	}
	b, _ := json.Marshal(map[string]any{"total_count": total, "check_runs": runs})
	return string(b)
}
func approvedStatuses(n, total int, state string) string {
	statuses := make([]map[string]string, n)
	for i := range statuses {
		statuses[i] = map[string]string{"context": fmt.Sprintf("status-%d", i), "state": state}
	}
	b, _ := json.Marshal(map[string]any{"total_count": total, "state": "success", "statuses": statuses})
	return string(b)
}

// TC-57 (REQ-F16), TC-58 (REQ-F16): every page matters; no mutation on unread checks.
func TestApproved_MergeReadsEveryCheckPage(t *testing.T) {
	for _, mode := range []string{"green", "101st_failure", "101st_pending", "31st_status_failure", "incomplete", "page_error", "cap"} {
		t.Run(mode, func(t *testing.T) {
			api := mergeAPI(prJSON, approvedRuns(100, 101, "success"), approvedStatuses(30, 31, "success"))
			run2 := "GET /repos/o/r/commits/abc1234def/check-runs?per_page=100&page=2"
			status2 := "GET /repos/o/r/commits/abc1234def/status?page=2"
			api.resp[run2] = approvedRuns(1, 101, "success")
			api.resp[status2] = approvedStatuses(1, 31, "success")
			want := ""
			switch mode {
			case "101st_failure":
				api.resp[run2] = approvedRuns(1, 101, "failure")
				want = "checks_failing"
			case "101st_pending":
				api.resp[run2] = `{"total_count":101,"check_runs":[{"name":"late","status":"queued"}]}`
				want = "checks_pending"
			case "31st_status_failure":
				api.resp[status2] = approvedStatuses(1, 31, "failure")
				want = "checks_failing"
			case "incomplete":
				api.resp[run2] = approvedRuns(0, 101, "success")
				want = "checks_unverified"
			case "page_error":
				api.resp[run2] = &github.GitHubError{Code: "server_error", Retryable: true}
				want = "server_error"
			case "cap":
				api.resp["GET /repos/o/r/commits/abc1234def/check-runs?per_page=100"] = approvedRuns(100, 1001, "success")
				for p := 2; p <= 10; p++ {
					api.resp[fmt.Sprintf("GET /repos/o/r/commits/abc1234def/check-runs?per_page=100&page=%d", p)] = approvedRuns(100, 1001, "success")
				}
				want = "checks_unverified"
			}
			_, err := run(t, find(t, NewDangerousTools(api, san{}), "github_pr_merge"), grant(nil), mergeIn)
			if got := code(err); got != want {
				t.Errorf("got %q want %q", got, want)
			}
			if mode == "green" {
				if api.count("PUT") != 1 || api.count(run2) != 1 || api.count(status2) != 1 {
					t.Fatal("green merge did not read both second pages and merge once")
				}
			} else if api.count("PUT") != 0 {
				t.Error("unsafe merge executed")
			}
		})
	}
}

// TC-43 (REQ-F6), TC-44 (REQ-F6): arbitrary passwords, including rename patches.
func TestApproved_PRSecretPathsWithholdNonPatternPasswords(t *testing.T) {
	for _, path := range []string{".env", "config/private.key", "credentials", "secrets/password.txt", "id_ed25519"} {
		for _, renamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/renamed=%v", path, renamed), func(t *testing.T) {
				f := map[string]any{"filename": path, "patch": "+password=ordinary-private-password-123", "status": "modified"}
				if renamed {
					f["filename"] = "public.txt"
					f["previous_filename"] = path
					f["status"] = "renamed"
				}
				b, _ := json.Marshal([]any{f, map[string]any{"filename": "main.go", "patch": "+fmt.Println(42)", "status": "modified"}})
				api := &fakeAPI{resp: map[string]any{"GET /repos/o/r/pulls/7/files?per_page=100": string(b)}}
				out, err := run(t, find(t, NewReadTools(api), "github_pr_files"), grant(nil), `{"repo":"o/r","number":7}`)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out.Text, "ordinary-private-password") || !strings.Contains(out.Text, "patch withheld") || !strings.Contains(out.Text, "fmt.Println(42)") {
					t.Fatal("secret patch leaked or normal patch lost")
				}
			})
		}
	}
}
