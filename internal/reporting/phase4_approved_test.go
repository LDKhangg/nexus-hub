package reporting

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/people"
)

// phase4Store is a scope-aware fake Store. Keys are
// "<user>/<workspace>/<behavior>"; an empty behavior is the workspace
// default. It resolves a behavior override before the workspace default,
// like the PostgreSQL store.
type phase4Store struct {
	prefs     map[string]Prefs
	templates map[string]string // key + "/" + kind
	calls     []string
}

func newPhase4Store() *phase4Store {
	return &phase4Store{prefs: map[string]Prefs{}, templates: map[string]string{}}
}

func scopeKey(user, workspace, behavior string) string {
	return user + "/" + workspace + "/" + behavior
}

func (s *phase4Store) Prefs(_ context.Context, user, workspace, behavior string) (Prefs, error) {
	s.calls = append(s.calls, "prefs "+scopeKey(user, workspace, behavior))
	if p, ok := s.prefs[scopeKey(user, workspace, behavior)]; ok && behavior != "" {
		return p, nil
	}
	if p, ok := s.prefs[scopeKey(user, workspace, "")]; ok {
		return p, nil
	}
	return DefaultOwnerPrefs(), nil
}

func (s *phase4Store) Template(_ context.Context, user, workspace, behavior string, kind Kind) (string, bool, error) {
	if body, ok := s.templates[scopeKey(user, workspace, behavior)+"/"+string(kind)]; ok && behavior != "" {
		return body, true, nil
	}
	body, ok := s.templates[scopeKey(user, workspace, "")+"/"+string(kind)]
	return body, ok, nil
}

// phase4Resolver is a fake people.Resolver keyed by
// "<workspace>/<platform>/<external id>".
type phase4Resolver struct{ names map[string]string }

func (r *phase4Resolver) DisplayName(_ context.Context, workspace, platform, id string) (string, bool) {
	name, ok := r.names[workspace+"/"+platform+"/"+id]
	return name, ok
}

func (*phase4Resolver) Sighting(context.Context, people.Sighting) error { return nil }

var _ people.Resolver = (*phase4Resolver)(nil)

func fullPrefs(style Style) Prefs {
	return Prefs{Style: style, ShowReaction: true, ShowAISummary: true, ShowLinks: true, ShowPRTitles: true, ShowTraceHint: true, Language: "en"}
}

func phase4Report(kind Kind) Report {
	return Report{Kind: kind, UserID: "owner", WorkspaceID: "workspace-a", Behavior: "review", Language: "vi", Platform: "clickup", ExternalID: "106",
		Reaction: "reacted", TraceHint: "trace 42", Values: Values{PRNumber: "1601", PRRepo: "org/repo", PRTitle: "Title", PRs: "org/repo#1601 Title", Verdict: "approve_request", Confidence: "0.94", AISummary: "summary", Reason: "policy denied", Link: "https://example.test/pr/1601", RunID: "42"}}
}

func lineCount(s string) int { return len(strings.Split(s, "\n")) }

func TestPhase4RendererApprovedCases(t *testing.T) {
	ctx := context.Background()
	newNames := func() *phase4Resolver {
		return &phase4Resolver{names: map[string]string{"workspace-a/clickup/106": "anh Định"}}
	}

	// TC-5 (REQ-3): the DM report shows the alias, then the documented
	// "<platform> <id>" fallback when the directory has no name.
	t.Run("TC-5 alias resolution and safe fallback", func(t *testing.T) {
		names := newNames()
		r := NewRenderer(newPhase4Store(), names)
		out, err := r.Render(ctx, phase4Report(KindPRApprove))
		if err != nil || !strings.HasPrefix(out, "✅ Approved PR #1601 cho anh Định") || strings.Contains(out, "106") {
			t.Fatalf("alias not used: output=%q err=%v", out, err)
		}
		delete(names.names, "workspace-a/clickup/106")
		out, err = r.Render(ctx, phase4Report(KindPRApprove))
		if err != nil || !strings.HasPrefix(out, "✅ Approved PR #1601 cho clickup 106") {
			t.Fatalf("documented fallback missing: output=%q err=%v", out, err)
		}
		names.names["workspace-a/clickup/106"] = "   "
		out, err = r.Render(ctx, phase4Report(KindPRApprove))
		if err != nil || !strings.Contains(out, "cho clickup 106") {
			t.Fatalf("blank directory name not replaced by the fallback: output=%q err=%v", out, err)
		}
	})

	// TC-10 (REQ-5): a name stored for another workspace never leaks.
	t.Run("TC-10 another workspace's name is not used", func(t *testing.T) {
		names := &phase4Resolver{names: map[string]string{"workspace-b/clickup/106": "Người khác"}}
		out, err := NewRenderer(nil, names).Render(ctx, phase4Report(KindPRApprove))
		if err != nil || strings.Contains(out, "Người khác") || !strings.Contains(out, "clickup 106") {
			t.Fatalf("cross-workspace name leaked: output=%q err=%v", out, err)
		}
	})

	// TC-13 (REQ-7): without any stored row the owner gets a short report
	// of at most four lines and no reaction line.
	t.Run("TC-13 owner default is short without reaction", func(t *testing.T) {
		for _, r := range []*Renderer{NewRenderer(nil, newNames()), NewRenderer(newPhase4Store(), newNames())} {
			out, err := r.Render(ctx, phase4Report(KindPRApprove))
			want := "✅ Approved PR #1601 cho anh Định\nAI: approve_request (0.94)\nhttps://example.test/pr/1601"
			if err != nil || out != want {
				t.Fatalf("default report=%q err=%v, want %q", out, err, want)
			}
		}
		for _, kind := range []Kind{KindPRApprove, KindPRSkipped, KindVerdictUnavailable, KindMention, KindNotify, KindFYI, KindDigest, KindFailure} {
			report := phase4Report(kind)
			report.Values.Items = "• one\n• two\n• three"
			out, err := NewRenderer(nil, newNames()).Render(ctx, report)
			if err != nil || lineCount(out) > 4 || strings.Contains(out, "reacted") || strings.Contains(out, "trace 42") {
				t.Fatalf("kind=%s default report=%q err=%v", kind, out, err)
			}
		}
	})

	// TC-14 (REQ-7): the most specific scope applies; another user or
	// workspace keeps its own settings; the preference language wins over
	// the profile language, and an empty one falls back to it.
	t.Run("TC-14 scoped preferences and language fallback", func(t *testing.T) {
		store := newPhase4Store()
		store.prefs[scopeKey("owner", "workspace-a", "")] = Prefs{Style: StyleNormal, Language: ""}
		store.prefs[scopeKey("owner", "workspace-a", "review")] = fullPrefs(StyleDetailed)
		store.prefs[scopeKey("other", "workspace-a", "")] = Prefs{Style: StyleDetailed, ShowReaction: true, Language: "en"}
		store.prefs[scopeKey("owner", "workspace-b", "")] = Prefs{Style: StyleDetailed, ShowReaction: true, Language: "en"}
		r := NewRenderer(store, newNames())

		out, err := r.Render(ctx, phase4Report(KindPRApprove))
		if err != nil || !strings.Contains(out, "Approved PR #1601 for anh Định") || !strings.Contains(out, "reacted") || !strings.Contains(out, "Behavior: review · run 42") || !strings.Contains(out, "trace 42") {
			t.Fatalf("behavior override not applied: output=%q err=%v", out, err)
		}
		report := phase4Report(KindPRApprove)
		report.Behavior = "digest-bot"
		out, err = r.Render(ctx, report)
		if err != nil || !strings.HasPrefix(out, "✅ Approved PR #1601 cho anh Định") || strings.Contains(out, "reacted") || strings.Contains(out, "Behavior:") {
			t.Fatalf("workspace default with profile language not applied: output=%q err=%v", out, err)
		}
		if strings.Contains(out, "org/repo#1601 Title") {
			t.Fatalf("normal style with PR titles off rendered PR titles: %q", out)
		}
		found := false
		for _, call := range store.calls {
			if call == "prefs owner/workspace-a/digest-bot" {
				found = true
			}
		}
		if !found {
			t.Fatalf("renderer did not ask for the report's own user/workspace/behavior scope: %v", store.calls)
		}
	})

	// TC-15 (REQ-8): every kind renders under every style and toggle set,
	// and disabled lines stay hidden.
	t.Run("TC-15 every report kind renders under every style and toggle set", func(t *testing.T) {
		kinds := []Kind{KindPRApprove, KindPRSkipped, KindVerdictUnavailable, KindMention, KindNotify, KindFYI, KindDigest, KindFailure}
		for _, style := range []Style{StyleShort, StyleNormal, StyleDetailed} {
			for _, on := range []bool{true, false} {
				for _, kind := range kinds {
					p := fullPrefs(style)
					p.ShowReaction, p.ShowAISummary, p.ShowLinks, p.ShowPRTitles, p.ShowTraceHint = on, on, on, on, on
					store := newPhase4Store()
					store.prefs[scopeKey("owner", "workspace-a", "")] = p
					out, err := NewRenderer(store, newNames()).Render(ctx, phase4Report(kind))
					if err != nil || strings.TrimSpace(out) == "" {
						t.Fatalf("style=%s on=%v kind=%s output=%q err=%v", style, on, kind, out, err)
					}
					if !on && (strings.Contains(out, "example.test") || strings.Contains(out, "reacted") || strings.Contains(out, "trace 42") || strings.Contains(out, "summary")) {
						t.Fatalf("disabled line rendered for %s/%s: %q", style, kind, out)
					}
					if style == StyleShort && (lineCount(out) > 4 || strings.Contains(out, "reacted")) {
						t.Fatalf("short report too long or with reaction for %s: %q", kind, out)
					}
					if style == StyleDetailed && on && !strings.Contains(out, "reacted") {
						t.Fatalf("detailed report with reaction on lacks it for %s: %q", kind, out)
					}
					if style != StyleShort && on && !strings.Contains(out, "org/repo#1601 Title") {
						t.Fatalf("PR titles on but missing for %s/%s: %q", style, kind, out)
					}
				}
			}
		}
	})

	// TC-16 (REQ-8): failures and skips keep one reason line with every
	// optional line disabled, in at most four lines.
	t.Run("TC-16 failures and skips keep reasons with optional lines disabled", func(t *testing.T) {
		for _, lang := range []string{"en", "vi"} {
			want := "Reason: policy denied"
			if lang == "vi" {
				want = "Lý do: policy denied"
			}
			store := newPhase4Store()
			store.prefs[scopeKey("owner", "workspace-a", "")] = Prefs{Style: StyleShort, Language: lang}
			for _, kind := range []Kind{KindFailure, KindPRSkipped, KindVerdictUnavailable} {
				report := phase4Report(kind)
				report.Values.Items = "• a\n• b"
				out, err := NewRenderer(store, newNames()).Render(ctx, report)
				if err != nil || !strings.Contains(out, want) || lineCount(out) > 4 || strings.Count(out, want) != 1 {
					t.Fatalf("lang=%s kind=%s output=%q err=%v", lang, kind, out, err)
				}
			}
		}
	})
}

func TestPhase4TemplatesApprovedCases(t *testing.T) {
	ctx := context.Background()

	// TC-19 (REQ-9a): every allowed placeholder is substituted; optional
	// lines with an empty value are removed.
	t.Run("TC-19 all placeholders and optional lines", func(t *testing.T) {
		body := "{contact}|{contact_platform}|{pr_number}|{pr_repo}|{pr_title}|{prs}|{verdict}|{confidence}|{ai_summary}|{outcome}|{reason}|{task}|{channel}|{link}|{behavior}|{run_id}\n?{task}task={task}\n?{channel}drop\n?{items}{items}"
		values := Values{Contact: "c", ContactPlatform: "p", PRNumber: "1", PRRepo: "r", PRTitle: "t", PRs: "ps", Verdict: "v", Confidence: "q", AISummary: "a", Outcome: "o", Reason: "why", Task: "task", Link: "l", Behavior: "b", RunID: "id", Items: "• i1\n• i2"}
		out, err := Render(body, KindNotify, values)
		want := "c|p|1|r|t|ps|v|q|a|o|why|task||l|b|id\ntask=task\n• i1\n• i2"
		if err != nil || out != want {
			t.Fatalf("placeholder render=%q err=%v, want %q", out, err, want)
		}
		out, err = Render("?{link}Link: {link}\n?{ai_summary}AI: {ai_summary}\n{outcome}", KindNotify, Values{Outcome: "done"})
		if err != nil || out != "done" {
			t.Fatalf("empty optional lines kept: %q err=%v", out, err)
		}
	})

	// TC-20 (REQ-9a): each invalid template is refused with its exact reason.
	t.Run("TC-20 exact template validation reasons", func(t *testing.T) {
		cases := map[string]struct{ body, reason string }{
			"unknown":         {"{secret}", "unknown placeholder"},
			"unknown in ?":    {"?{secret}x", "unknown placeholder"},
			"stray brace":     {"a { b", "unknown placeholder"},
			"length":          {strings.Repeat("a", 1001), "more than 1,000 characters"},
			"length in runes": {strings.Repeat("đ", 1001), "more than 1,000 characters"},
			"lines":           {strings.Repeat("a\n", 12), "more than 12 lines"},
			"control":         {"a\tb", "control characters"},
			"bidi":            {"a\u202eb", "control characters"},
			"carriage return": {"a\rb", "control characters"},
		}
		for name, tc := range cases {
			if err := Validate(tc.body); err == nil || err.Error() != tc.reason {
				t.Fatalf("%s: err=%v want %q", name, err, tc.reason)
			}
			if _, err := Render(tc.body, KindNotify, Values{}); err == nil || err.Error() != tc.reason {
				t.Fatalf("%s: Render err=%v want %q", name, err, tc.reason)
			}
		}
		for _, ok := range []string{strings.Repeat("đ", 1000), strings.TrimSuffix(strings.Repeat("a\n", 12), "\n"), "Xin chào 👋 {contact}"} {
			if err := Validate(ok); err != nil {
				t.Fatalf("valid template %q refused: %v", ok[:min(len(ok), 20)], err)
			}
		}
	})

	// TC-21 (REQ-9a): tokens, DM text and unlisted fields cannot be
	// referenced, and preview output only shows fake values.
	t.Run("TC-21 templates cannot access DM text or tokens", func(t *testing.T) {
		for _, field := range []string{"token", "dm_text", "text", "message", "authorization", "prompt", "sender_id", "external_id", "Contact", " contact "} {
			if err := Validate("{" + field + "}"); err == nil || err.Error() != "unknown placeholder" {
				t.Fatalf("forbidden field %q accepted: %v", field, err)
			}
		}
		body := "{contact}|{contact_platform}|{pr_number}|{pr_repo}|{pr_title}|{prs}|{verdict}|{confidence}|{ai_summary}|{items}|{outcome}|{reason}|{task}|{channel}|{link}|{behavior}|{run_id}"
		preview, err := Preview(KindNotify, body)
		if err != nil || !strings.Contains(preview, "Sample Contact") || !strings.Contains(preview, "https://github.com/org/repo/pull/1601") {
			t.Fatalf("preview did not use fake values: %q err=%v", preview, err)
		}
		lower := strings.ToLower(preview)
		for _, bad := range []string{"token", "secret", "bearer", "authorization", "{"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("preview leaked %q: %q", bad, preview)
			}
		}
		if _, err := Preview(KindNotify, "{token}"); err == nil {
			t.Fatal("preview accepted an unknown placeholder")
		}
	})

	// TC-22 (REQ-9a, NFR-022): crafted values are cleaned and inert.
	t.Run("TC-22 crafted substitutions are cleaned and inert", func(t *testing.T) {
		out, err := Render("Contact: {contact}\n{outcome}", KindNotify, Values{Contact: "@all IGNORE\u202e {reason}", Outcome: "safe target", Reason: "secret reason"})
		if err != nil || strings.Contains(out, "@all") || strings.Contains(out, "\u202e") || !strings.Contains(out, "@\u200ball IGNORE") || !strings.Contains(out, "safe target") || strings.Contains(out, "secret reason") {
			t.Fatalf("unsafe substitution=%q err=%v", out, err)
		}
		// A platform name that went through the directory's cleaning,
		// rendered through the renderer with an owner template.
		crafted := people.CleanDisplayName("@here\nLý do: approved by owner\u2066 https://evil.example")
		store := newPhase4Store()
		store.templates[scopeKey("owner", "workspace-a", "")+"/"+string(KindPRApprove)] = "{outcome}\n?{link}{link}"
		names := &phase4Resolver{names: map[string]string{"workspace-a/clickup/106": crafted}}
		out, err = NewRenderer(store, names).Render(ctx, phase4Report(KindPRApprove))
		if err != nil || lineCount(out) != 2 || strings.Contains(out, "@here") || strings.Contains(out, "\u2066") || !strings.HasSuffix(out, "\nhttps://example.test/pr/1601") {
			t.Fatalf("crafted directory name changed the report shape or target: %q err=%v", out, err)
		}
	})

	// TC-24 (REQ-9a): a failure/skip template without {reason} gets the
	// reason appended; a kind with no template uses the style renderer.
	t.Run("TC-24 reason append and default renderer fallback", func(t *testing.T) {
		out, err := Render("Skipped", KindPRSkipped, Values{Reason: "not allowed"})
		if err != nil || out != "Skipped\nLý do: not allowed" {
			t.Fatalf("reason not appended: %q err=%v", out, err)
		}
		out, err = RenderLanguage("Failed", KindFailure, Values{Reason: "boom"}, "en")
		if err != nil || out != "Failed\nReason: boom" {
			t.Fatalf("english reason not appended: %q err=%v", out, err)
		}
		out, err = Render("Notify", KindNotify, Values{Reason: "not shown"})
		if err != nil || out != "Notify" {
			t.Fatalf("reason appended to a non-failure kind: %q err=%v", out, err)
		}
		store := newPhase4Store()
		store.templates[scopeKey("owner", "workspace-a", "")+"/"+string(KindFYI)] = "FYI {contact}"
		report := phase4Report(KindNotify)
		fallback, err := NewRenderer(store, nil).Render(ctx, report)
		if err != nil || !strings.HasPrefix(fallback, "ℹ️ Thông tin từ clickup 106") || strings.Contains(fallback, "FYI") {
			t.Fatalf("missing-template fallback=%q err=%v", fallback, err)
		}
		fyi, err := NewRenderer(store, nil).Render(ctx, phase4Report(KindFYI))
		if err != nil || fyi != "FYI clickup 106" {
			t.Fatalf("template for its own kind not used: %q err=%v", fyi, err)
		}
	})
}

// TC-R1..TC-R4: regression tests for Review #1 findings fixed in 9173a36.
func TestPhase4ReviewRegressions(t *testing.T) {
	ctx := context.Background()

	// TC-R1 (REQ-8): short notify/FYI/digest reports keep their items in
	// one line of at most 200 runes, with "+N" for the rest.
	t.Run("TC-R1 short reports keep the reported items", func(t *testing.T) {
		for _, kind := range []Kind{KindNotify, KindFYI, KindDigest, KindMention} {
			report := Report{Kind: kind, WorkspaceID: "ws", Language: "vi", Values: Values{Items: "Hai việc:\n• Lan hỏi về PR #12\n• Định cần review", Link: "https://example.test/x"}}
			out, err := NewRenderer(nil, nil).Render(ctx, report)
			if err != nil || !strings.Contains(out, "Lan hỏi về PR #12 · Định cần review") || lineCount(out) > 4 {
				t.Fatalf("kind=%s short report lost items: %q err=%v", kind, out, err)
			}
		}
		var items []string
		for i := 0; i < 30; i++ {
			items = append(items, "• việc số "+strings.Repeat("x", 10)+string(rune('A'+i%26)))
		}
		report := Report{Kind: KindDigest, WorkspaceID: "ws", Language: "vi", Values: Values{Outcome: "🗂 Tổng hợp", Items: strings.Join(items, "\n")}}
		out, err := NewRenderer(nil, nil).Render(ctx, report)
		if err != nil || lineCount(out) != 2 {
			t.Fatalf("long digest=%q err=%v", out, err)
		}
		line := strings.Split(out, "\n")[1]
		if utf8.RuneCountInString(line) > 200 || !strings.Contains(line, " +") || !strings.HasPrefix(line, "việc số") {
			t.Fatalf("compact item line=%q (%d runes)", line, utf8.RuneCountInString(line))
		}
		huge := Report{Kind: KindNotify, WorkspaceID: "ws", Language: "vi", Values: Values{Items: "• " + strings.Repeat("y", 500) + "\n• second"}}
		out, err = NewRenderer(nil, nil).Render(ctx, huge)
		if err != nil {
			t.Fatal(err)
		}
		line = strings.Split(out, "\n")[1]
		if utf8.RuneCountInString(line) > 200 || !strings.Contains(line, "…") || !strings.HasSuffix(line, "+1") {
			t.Fatalf("one huge item not cut: %q (%d runes)", line, utf8.RuneCountInString(line))
		}
		// An empty Report.UserID means the workspace owner; phase4Store has
		// no OwnerUserID, so the renderer falls back to DefaultOwnerUserID.
		normal := newPhase4Store()
		normal.prefs[scopeKey(DefaultOwnerUserID, "ws", "")] = Prefs{Style: StyleNormal}
		out, err = NewRenderer(normal, nil).Render(ctx, Report{Kind: KindNotify, WorkspaceID: "ws", Values: Values{Items: "• a\n• b"}})
		if err != nil || !strings.Contains(out, "\n• a\n• b") {
			t.Fatalf("normal style should list items one per line: %q err=%v", out, err)
		}
	})

	// TC-R2 (REQ-8, REQ-9a): an optional template line cannot suppress a
	// mandatory reason, for every failure/skip kind.
	t.Run("TC-R2 optional line cannot hide the reason", func(t *testing.T) {
		for _, kind := range []Kind{KindPRSkipped, KindFailure, KindVerdictUnavailable} {
			out, err := Render("{outcome}\n?{link}Lý do: {reason}", kind, Values{Outcome: "Bỏ qua", Reason: "điểm thấp"})
			if err != nil || out != "Bỏ qua\nLý do: điểm thấp" {
				t.Fatalf("kind=%s reason hidden by optional line: %q err=%v", kind, out, err)
			}
			out, err = Render("{outcome}\n?{link}Lý do: {reason}", kind, Values{Outcome: "Bỏ qua", Reason: "điểm thấp", Link: "https://l"})
			if err != nil || out != "Bỏ qua\nLý do: điểm thấp" || strings.Count(out, "điểm thấp") != 1 {
				t.Fatalf("kind=%s reason duplicated: %q err=%v", kind, out, err)
			}
		}
		out, err := Render("{outcome}", KindFailure, Values{Outcome: "Lỗi"})
		if err != nil || out != "Lỗi" {
			t.Fatalf("empty reason added a reason line: %q err=%v", out, err)
		}
	})

	// TC-R4 (REQ-9a, NFR-022): inserted values are never rescanned, so a
	// name that looks like a placeholder stays literal and the result is
	// deterministic.
	t.Run("TC-R4 substituted names are not placeholders", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			out, err := Render("{contact}|{reason}|{link}", KindNotify, Values{Contact: "{reason}{link}", Reason: "why", Link: "https://l"})
			if err != nil || out != "{reason}{link}|why|https://l" {
				t.Fatalf("run %d: rescanned substitution: %q err=%v", i, out, err)
			}
		}
		out, err := Render("{contact}", KindPRSkipped, Values{Contact: "{reason}", Reason: "real reason"})
		if err != nil || out != "{reason}\nLý do: real reason" {
			t.Fatalf("placeholder-looking name hid the reason: %q err=%v", out, err)
		}
		store := newPhase4Store()
		store.templates[scopeKey("owner", "workspace-a", "")+"/"+string(KindPRApprove)] = "{contact} · {link}"
		names := &phase4Resolver{names: map[string]string{"workspace-a/clickup/106": people.CleanDisplayName("{link} {run_id}")}}
		out, err = NewRenderer(store, names).Render(ctx, phase4Report(KindPRApprove))
		if err != nil || out != "{link} {run_id} · https://example.test/pr/1601" {
			t.Fatalf("directory name expanded as placeholders: %q err=%v", out, err)
		}
	})
}
