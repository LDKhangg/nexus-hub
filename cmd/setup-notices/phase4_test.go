package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/jackc/pgx/v5/pgxpool"
)

type phase4CLI struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	ws   string
}

// newPhase4CLI creates the workspace with `init`, the way the runbook does
// after the migrations have run.
func newPhase4CLI(t *testing.T) *phase4CLI {
	t.Helper()
	ctx, pool := setupDB(t)
	c := &phase4CLI{t: t, ctx: ctx, pool: pool}
	out, err := c.run("init", "--workspace-name", "Personal", "--chat-id", "-10042", "--team-id", "team")
	if err != nil {
		t.Fatal(err)
	}
	var ids struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal([]byte(out), &ids); err != nil || ids.WorkspaceID == "" {
		t.Fatalf("init output=%q err=%v", out, err)
	}
	c.ws = ids.WorkspaceID
	return c
}

// ownerMember links the bootstrap owner to the workspace, as migration
// 00012 does for workspaces that exist when it runs.
func (c *phase4CLI) ownerMember() {
	c.t.Helper()
	if _, err := c.pool.Exec(c.ctx, `INSERT INTO workspace_members(user_id,workspace_id,role) VALUES($1,$2,'owner') ON CONFLICT DO NOTHING`, reporting.DefaultOwnerUserID, c.ws); err != nil {
		c.t.Fatal(err)
	}
}

func (c *phase4CLI) run(args ...string) (string, error) {
	var out bytes.Buffer
	err := runCommand(c.ctx, c.pool, args, &out, func(string) (string, bool) { return "private-value", true })
	return out.String(), err
}

func (c *phase4CLI) must(args ...string) string {
	c.t.Helper()
	out, err := c.run(args...)
	if err != nil {
		c.t.Fatalf("%v: %v", args, err)
	}
	return out
}

// should runs a command and reports a failure without stopping the test.
func (c *phase4CLI) should(args ...string) string {
	c.t.Helper()
	out, err := c.run(args...)
	if err != nil {
		c.t.Errorf("%v: %v", args, err)
	}
	return out
}

func (c *phase4CLI) sight(platform, id, name, username string) {
	c.t.Helper()
	repo, err := storage.NewPeopleRepository(c.pool)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := repo.Sighting(c.ctx, people.Sighting{WorkspaceID: c.ws, Platform: platform, ExternalID: id, PlatformName: name, Username: username, Source: "auto", SeenAt: time.Now()}); err != nil {
		c.t.Fatal(err)
	}
}

func decodeContacts(t *testing.T, out string) []people.Contact {
	t.Helper()
	var list []people.Contact
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var c people.Contact
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("output %q: %v", out, err)
		}
		list = append(list, c)
	}
	return list
}

func decodePrefs(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("prefs output %q: %v", out, err)
	}
	return m
}

func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func render(t *testing.T, c *phase4CLI, behavior string, kind reporting.Kind) string {
	t.Helper()
	repo, err := storage.NewPeopleRepository(c.pool)
	if err != nil {
		t.Fatal(err)
	}
	store := reporting.NewPostgresStore(c.pool)
	owner, err := store.OwnerUserID(c.ctx, c.ws)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reporting.NewRenderer(store, repo).Render(c.ctx, reporting.Report{
		Kind: kind, UserID: owner, WorkspaceID: c.ws, Behavior: behavior, Language: "vi", Platform: "clickup", ExternalID: "106",
		Values:   reporting.Values{PRNumber: "1601", Verdict: "approve_request", Confidence: "0.94", AISummary: "looks fine", Link: "https://github.com/org/repo/pull/1601", Reason: "điểm thấp"},
		Reaction: "✅ Đã thả reaction white_check_mark", TraceHint: "/trace 7",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-7 (REQ-4): the contact CLI through a whole lifecycle.
func TestPhase4ContactCLI(t *testing.T) {
	c := newPhase4CLI(t)
	c.sight("clickup", "106", "Dinh Nguyen", "dinh")
	c.sight("github", "dinh-gh", "", "dinh-gh")

	t.Run("TC-7 list, show, alias, merge, split and forget", func(t *testing.T) {
		all := decodeContacts(t, c.must("contact", "list", "--workspace-id", c.ws))
		if len(all) != 2 {
			t.Fatalf("list=%+v", all)
		}
		gh := decodeContacts(t, c.must("contact", "list", "--workspace-id", c.ws, "--platform", "github"))
		if len(gh) != 1 || gh[0].Accounts[0].ExternalID != "dinh-gh" {
			t.Fatalf("platform filter=%+v", gh)
		}
		cu := decodeContacts(t, c.must("contact", "list", "--workspace-id", c.ws, "--search", "nguyen"))
		if len(cu) != 1 || cu[0].Name != "Dinh Nguyen" {
			t.Fatalf("search=%+v", cu)
		}
		shown := decodeContacts(t, c.must("contact", "show", "--workspace-id", c.ws, cu[0].ID))
		if len(shown) != 1 || shown[0].ID != cu[0].ID || shown[0].Accounts[0].Platform != "clickup" || shown[0].Accounts[0].Username != "dinh" {
			t.Fatalf("show=%+v", shown)
		}
		named := decodeContacts(t, c.must("contact", "alias", "--workspace-id", c.ws, "clickup:106", "anh Định"))
		if len(named) != 1 || named[0].Name != "anh Định" || named[0].Alias != "anh Định" {
			t.Fatalf("alias=%+v", named)
		}
		merged := decodeContacts(t, c.must("contact", "merge", "--workspace-id", c.ws, gh[0].ID, cu[0].ID))
		if len(merged) != 1 || merged[0].ID != cu[0].ID || len(merged[0].Accounts) != 2 || merged[0].Name != "anh Định" {
			t.Fatalf("merge=%+v", merged)
		}
		var ghAccount string
		for _, a := range merged[0].Accounts {
			if a.Platform == "github" {
				ghAccount = a.ID
			}
		}
		split := decodeContacts(t, c.must("contact", "split", "--workspace-id", c.ws, ghAccount))
		if len(split) != 1 || split[0].ID == cu[0].ID || len(split[0].Accounts) != 1 || split[0].Alias != "" {
			t.Fatalf("split=%+v", split)
		}
		if _, err := c.run("contact", "forget", "--workspace-id", c.ws, cu[0].ID); err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("forget without --yes err=%v", err)
		}
		out := c.must("contact", "forget", "--workspace-id", c.ws, "--yes", cu[0].ID)
		if !strings.Contains(out, `"forgotten":true`) {
			t.Fatalf("forget output=%q", out)
		}
		var accounts int
		if err := c.pool.QueryRow(c.ctx, `SELECT count(*) FROM contact_accounts WHERE workspace_id=$1 AND platform='clickup'`, c.ws).Scan(&accounts); err != nil || accounts != 0 {
			t.Fatalf("forget left %d clickup accounts (err=%v)", accounts, err)
		}
		if _, err := c.run("contact", "show", "--workspace-id", c.ws, cu[0].ID); err == nil || err.Error() != "contact not found" {
			t.Fatalf("show after forget err=%v", err)
		}
	})

	t.Run("TC-7 malformed commands are refused without effect", func(t *testing.T) {
		for _, args := range [][]string{
			{"contact"},
			{"contact", "list"},
			{"contact", "list", "--workspace-id", "not-a-uuid"},
			{"contact", "show", "--workspace-id", c.ws},
			{"contact", "show", "--workspace-id", c.ws, "--platform", "github", "x"},
			{"contact", "alias", "--workspace-id", c.ws, "clickup:106"},
			{"contact", "alias", "--workspace-id", c.ws, "clickup:106", "a", "b"},
			{"contact", "list", "--workspace-id", c.ws, "--yes"},
			{"contact", "purge", "--workspace-id", c.ws},
		} {
			if _, err := c.run(args...); err == nil {
				t.Fatalf("%v accepted", args)
			}
		}
		if _, err := c.run("contact", "alias", "--workspace-id", c.ws, "clickup:404", "Ai đó"); err == nil || strings.Contains(err.Error(), "404") {
			t.Fatalf("unknown account alias err=%v", err)
		}
		if _, err := c.run("contact", "alias", "--workspace-id", c.ws, "clickup:dinh-x", "\u202e\n"); err == nil {
			t.Fatal("empty cleaned alias accepted")
		}
	})
}

// TC-17/TC-18 (REQ-9): report-prefs show/set, behavior overrides, refused
// input, and the next report follows the change.
func TestPhase4ReportPrefsCLI(t *testing.T) {
	c := newPhase4CLI(t)
	c.ownerMember()
	c.sight("clickup", "106", "Dinh Nguyen", "")

	t.Run("TC-17 show, set and the next report", func(t *testing.T) {
		p := decodePrefs(t, c.must("report-prefs", "show", "--workspace", c.ws))
		if p["style"] != "short" || p["show_reaction"] != false || p["show_ai_summary"] != true || p["show_links"] != true || p["language"] != "" {
			t.Fatalf("defaults=%v", p)
		}
		if out := render(t, c, "review", reporting.KindPRApprove); out != "✅ Approved PR #1601 cho Dinh Nguyen\nAI: approve_request (0.94)\nhttps://github.com/org/repo/pull/1601" {
			t.Fatalf("default report=%q", out)
		}
		p = decodePrefs(t, c.must("report-prefs", "set", "--workspace", c.ws, "style=detailed", "show_reaction=true", "language=en"))
		if p["style"] != "detailed" || p["show_reaction"] != true || p["language"] != "en" || p["show_links"] != true {
			t.Fatalf("set=%v", p)
		}
		out := render(t, c, "review", reporting.KindPRApprove)
		if !strings.HasPrefix(out, "✅ Approved PR #1601 for Dinh Nguyen") || !strings.Contains(out, "white_check_mark") || !strings.Contains(out, "/trace 7") {
			t.Fatalf("report after set=%q", out)
		}
		p = decodePrefs(t, c.must("report-prefs", "set", "--workspace", c.ws, "--behavior", "review", "style=short", "show_links=false"))
		if p["style"] != "short" || p["show_links"] != false || p["show_reaction"] != true {
			t.Fatalf("override set=%v", p)
		}
		if p := decodePrefs(t, c.must("report-prefs", "show", "--workspace", c.ws, "--behavior", "review")); p["style"] != "short" {
			t.Fatalf("override show=%v", p)
		}
		if p := decodePrefs(t, c.must("report-prefs", "show", "--workspace", c.ws, "--behavior", "other")); p["style"] != "detailed" {
			t.Fatalf("other behavior should use the workspace row: %v", p)
		}
		if out := render(t, c, "review", reporting.KindPRApprove); strings.Contains(out, "https://") || strings.Count(out, "\n") > 3 {
			t.Fatalf("behavior override not applied: %q", out)
		}
		if out := render(t, c, "other", reporting.KindPRApprove); !strings.Contains(out, "https://github.com/org/repo/pull/1601") {
			t.Fatalf("other behavior lost the workspace prefs: %q", out)
		}
	})

	t.Run("TC-18 invalid keys and values are refused and change nothing", func(t *testing.T) {
		before := c.must("report-prefs", "show", "--workspace", c.ws)
		for _, args := range [][]string{
			{"colour=red"},
			{"style=huge"},
			{"style=SHORT"},
			{"show_links=yes"},
			{"show_links=1"},
			{"language=fr"},
			{"style=short", "style=normal"},
			{"style"},
			{"=short"},
		} {
			_, err := c.run(append([]string{"report-prefs", "set", "--workspace", c.ws}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "style=short|normal|detailed") || !strings.Contains(err.Error(), "language=vi|en") {
				t.Fatalf("%v err=%v", args, err)
			}
		}
		for _, args := range [][]string{
			{"report-prefs", "set", "--workspace", c.ws},
			{"report-prefs", "show", "--workspace", c.ws, "style=short"},
			{"report-prefs", "get", "--workspace", c.ws},
			{"report-prefs", "show"},
			{"report-prefs", "show", "--workspace", c.ws, "--behavior", ""},
			{"report-prefs", "show", "--workspace", c.ws, "--colour", "red"},
		} {
			if _, err := c.run(args...); err == nil {
				t.Fatalf("%v accepted", args)
			}
		}
		if after := c.must("report-prefs", "show", "--workspace", c.ws); after != before {
			t.Fatalf("refused input changed prefs: %s -> %s", before, after)
		}
	})
}

// TC-17 (REQ-9, REQ-10): on a fresh install the workspace is created by
// `init` after the migrations; the owner must still be able to save report
// preferences and templates without a manual step.
func TestPhase4FreshInstallOwnerCanSaveReportSettings(t *testing.T) {
	c := newPhase4CLI(t)
	if _, err := c.run("report-prefs", "set", "--workspace", c.ws, "style=normal"); err != nil {
		t.Fatalf("report-prefs set on a workspace created by init: %v", err)
	}
	path := writeTemplate(t, "{outcome}")
	if _, err := c.run("report-template", "set", "pr_approve", "--workspace", c.ws, "--file", path); err != nil {
		t.Fatalf("report-template set on a workspace created by init: %v", err)
	}
}

// TC-23 (REQ-9a): report-template set/show/preview/reset with scope
// precedence; invalid templates are refused with their reason and nothing
// is saved.
func TestPhase4ReportTemplateCLI(t *testing.T) {
	c := newPhase4CLI(t)
	c.ownerMember()
	c.sight("clickup", "106", "Dinh Nguyen", "")

	t.Run("TC-23 set, show, preview, reset and precedence", func(t *testing.T) {
		out := c.must("report-template", "show", "pr_approve", "--workspace", c.ws)
		if !strings.Contains(out, `"default":true`) {
			t.Fatalf("show default=%q", out)
		}
		preview := c.must("report-template", "preview", "pr_approve", "--workspace", c.ws)
		if !strings.Contains(preview, "Sample Contact") || !strings.Contains(preview, "https://github.com/org/repo/pull/1601") || strings.Contains(preview, "Dinh") {
			t.Fatalf("default preview=%q", preview)
		}
		c.must("report-template", "set", "pr_approve", "--workspace", c.ws, "--file", writeTemplate(t, "Duyệt #{pr_number} cho {contact}\n?{link}{link}\n"))
		c.must("report-template", "set", "pr_approve", "--workspace", c.ws, "--behavior", "review", "--file", writeTemplate(t, "Review: {contact}"))
		out = c.must("report-template", "show", "pr_approve", "--workspace", c.ws)
		if !strings.Contains(out, `"template":"Duyệt #{pr_number} cho {contact}\n?{link}{link}"`) || !strings.Contains(out, `"default":false`) {
			t.Fatalf("show global=%q", out)
		}
		if out := c.must("report-template", "show", "pr_approve", "--workspace", c.ws, "--behavior", "review"); !strings.Contains(out, `"template":"Review: {contact}"`) {
			t.Fatalf("show override=%q", out)
		}
		if preview := c.must("report-template", "preview", "pr_approve", "--workspace", c.ws); strings.TrimSpace(preview) != "Duyệt #1601 cho Sample Contact\nhttps://github.com/org/repo/pull/1601" {
			t.Fatalf("custom preview=%q", preview)
		}
		if out := render(t, c, "review", reporting.KindPRApprove); out != "Review: Dinh Nguyen" {
			t.Fatalf("override render=%q", out)
		}
		if out := render(t, c, "other", reporting.KindPRApprove); out != "Duyệt #1601 cho Dinh Nguyen\nhttps://github.com/org/repo/pull/1601" {
			t.Fatalf("global render=%q", out)
		}
		// Standard input, as `--file -` in the runbook.
		stdin := writeTemplate(t, "Bỏ qua {contact}")
		f, err := os.Open(stdin)
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = f
		_, err = c.run("report-template", "set", "pr_skipped", "--workspace", c.ws, "--file", "-")
		os.Stdin = old
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if out := render(t, c, "other", reporting.KindPRSkipped); out != "Bỏ qua Dinh Nguyen\nLý do: điểm thấp" {
			t.Fatalf("stdin template with appended reason=%q", out)
		}
		if out := c.must("report-template", "reset", "pr_approve", "--workspace", c.ws, "--behavior", "review"); !strings.Contains(out, `"reset":true`) {
			t.Fatalf("reset override=%q", out)
		}
		if out := render(t, c, "review", reporting.KindPRApprove); !strings.HasPrefix(out, "Duyệt #1601") {
			t.Fatalf("after override reset=%q", out)
		}
		c.must("report-template", "reset", "pr_approve", "--workspace", c.ws)
		if out := render(t, c, "review", reporting.KindPRApprove); out != "✅ Approved PR #1601 cho Dinh Nguyen\nAI: approve_request (0.94)\nhttps://github.com/org/repo/pull/1601" {
			t.Fatalf("after full reset the style renderer should apply: %q", out)
		}
		if out := c.must("report-template", "reset", "pr_approve", "--workspace", c.ws); !strings.Contains(out, `"reset":false`) {
			t.Fatalf("second reset=%q", out)
		}
	})

	t.Run("TC-20 TC-23 invalid templates are refused and not saved", func(t *testing.T) {
		c.must("report-template", "set", "notify", "--workspace", c.ws, "--file", writeTemplate(t, "Giữ nguyên"))
		cases := map[string]string{
			"{token}":                       "unknown placeholder",
			"{dm_text}":                     "unknown placeholder",
			strings.Repeat("x", 1001):       "more than 1,000 characters",
			strings.Repeat("dòng\n", 13):    "more than 12 lines",
			"tab\there":                     "control characters",
			"bidi \u202e text {contact}":    "control characters",
			"{contact} and {{outcome}}":     "unknown placeholder",
			"?{secret}line\n{outcome}":      "unknown placeholder",
			"windows\r\nline {outcome}\r\n": "",
		}
		for body, reason := range cases {
			_, err := c.run("report-template", "set", "notify", "--workspace", c.ws, "--file", writeTemplate(t, body))
			if reason == "" {
				if err != nil {
					t.Fatalf("CRLF template refused: %v", err)
				}
				c.must("report-template", "set", "notify", "--workspace", c.ws, "--file", writeTemplate(t, "Giữ nguyên"))
				continue
			}
			if err == nil || err.Error() != reason {
				t.Fatalf("body %q err=%v, want %q", body[:min(len(body), 20)], err, reason)
			}
			if out := c.must("report-template", "show", "notify", "--workspace", c.ws); !strings.Contains(out, `"template":"Giữ nguyên"`) {
				t.Fatalf("refused template changed the saved one: %q", out)
			}
		}
		for _, args := range [][]string{
			{"report-template", "set", "pr_secret", "--workspace", c.ws, "--file", writeTemplate(t, "x")},
			{"report-template", "set", "notify", "--workspace", c.ws},
			{"report-template", "show", "notify", "--workspace", c.ws, "--file", "x"},
			{"report-template", "set", "notify", "--workspace", c.ws, "--file", filepath.Join(t.TempDir(), "missing.txt")},
		} {
			if _, err := c.run(args...); err == nil {
				t.Fatalf("%v accepted", args)
			}
		}
		if _, err := c.run("report-template", "show", "pr_secret", "--workspace", c.ws); err == nil || !strings.Contains(err.Error(), "valid kinds: pr_approve") {
			t.Fatalf("unknown kind err=%v", err)
		}
	})
}

// TC-27 (REQ-11): follow the runbook section literally with fakes: every
// documented command exists in the runbook, runs, and gives the stated
// result.
func TestPhase4RunbookPeopleAndReportStyle(t *testing.T) {
	data, err := os.ReadFile("../../docs/runbooks/self-host-home.md")
	if err != nil {
		t.Fatal(err)
	}
	runbook := string(data)
	for _, line := range []string{
		"nh contact list --workspace-id $WS [--platform clickup] [--search '<text>']",
		"nh contact show --workspace-id $WS <contact-id>",
		"nh contact alias --workspace-id $WS clickup:<user-id> 'anh Định'",
		"# Telegram: /alias clickup:<user-id> anh Định",
		"nh contact merge  --workspace-id $WS <source-contact-id> <target-contact-id>",
		"nh contact split  --workspace-id $WS <account-id>",
		"nh contact forget --workspace-id $WS --yes <contact-id>",
		"nh report-prefs set --workspace $WS style=short show_reaction=false",
		"nh report-prefs set --workspace $WS show_ai_summary=true show_links=true \\\n  show_pr_titles=true show_trace_hint=true language=vi",
		"nh report-template set     pr_approve --workspace $WS --file report.txt",
		"nh report-template preview pr_approve --workspace $WS",
		"nh report-template reset   pr_approve --workspace $WS",
		"✅ Approved PR #1601 cho anh Định\nAI: approve_request (0.94)\nhttps://github.com/org/repo/pull/1601",
		"nh settings set telegram_assistant false",
	} {
		if !strings.Contains(runbook, line) {
			t.Fatalf("runbook no longer documents %q", line)
		}
	}

	c := newPhase4CLI(t) // the runbook's order: migrations, then init
	c.sight("clickup", "106", "Dinh Nguyen", "dinh")
	c.sight("telegram", "5001", "Dinh", "dinh_tg")

	// Name your contacts.
	list := decodeContacts(t, c.must("contact", "list", "--workspace-id", c.ws, "--platform", "clickup", "--search", "dinh"))
	if len(list) != 1 {
		t.Fatalf("documented list=%+v", list)
	}
	c.must("contact", "show", "--workspace-id", c.ws, list[0].ID)
	if named := decodeContacts(t, c.must("contact", "alias", "--workspace-id", c.ws, "clickup:106", "anh Định")); named[0].Name != "anh Định" {
		t.Fatalf("documented alias=%+v", named)
	}

	// Merge accounts of one person, then split them again.
	all := decodeContacts(t, c.must("contact", "list", "--workspace-id", c.ws))
	var tg string
	for _, contact := range all {
		if contact.ID != list[0].ID {
			tg = contact.ID
		}
	}
	merged := decodeContacts(t, c.must("contact", "merge", "--workspace-id", c.ws, tg, list[0].ID))
	if len(merged[0].Accounts) != 2 {
		t.Fatalf("documented merge=%+v", merged)
	}
	var tgAccount string
	for _, a := range merged[0].Accounts {
		if a.Platform == "telegram" {
			tgAccount = a.ID
		}
	}
	c.must("contact", "split", "--workspace-id", c.ws, tgAccount)

	// Choose your report style; the documented example is what the owner
	// then gets. A failure here is reported without stopping the walk.
	c.should("report-prefs", "set", "--workspace", c.ws, "style=short", "show_reaction=false")
	c.should("report-prefs", "set", "--workspace", c.ws, "show_ai_summary=true", "show_links=true", "show_pr_titles=true", "show_trace_hint=true", "language=vi")
	if out := render(t, c, "dm-approve", reporting.KindPRApprove); out != "✅ Approved PR #1601 cho anh Định\nAI: approve_request (0.94)\nhttps://github.com/org/repo/pull/1601" {
		t.Errorf("documented example differs from the real report: %q", out)
	}
	report := writeTemplate(t, "?{link}{link}\n{outcome}")
	c.should("report-template", "set", "pr_approve", "--workspace", c.ws, "--file", report)
	if preview := c.should("report-template", "preview", "pr_approve", "--workspace", c.ws); !strings.HasPrefix(preview, "https://github.com/org/repo/pull/1601\n") {
		t.Errorf("documented preview=%q", preview)
	}
	c.should("report-template", "reset", "pr_approve", "--workspace", c.ws)

	// Forget the contact.
	c.must("contact", "forget", "--workspace-id", c.ws, "--yes", list[0].ID)
	if out := render(t, c, "dm-approve", reporting.KindPRApprove); !strings.HasPrefix(out, "✅ Approved PR #1601 cho clickup 106") {
		t.Fatalf("after forget the report should fall back to the id: %q", out)
	}
	// "A later platform sighting can add that person again."
	c.sight("clickup", "106", "Dinh Again", "")
	if out := render(t, c, "dm-approve", reporting.KindPRApprove); !strings.HasPrefix(out, "✅ Approved PR #1601 cho Dinh Again") {
		t.Fatalf("re-sighting after forget: %q", out)
	}

	// Turning the assistant off is a valid documented setting.
	if _, err := c.run("settings", "set", "telegram_assistant", "false"); err != nil {
		t.Fatalf("documented settings command: %v", err)
	}
}
