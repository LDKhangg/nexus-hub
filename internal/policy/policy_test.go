package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Phase 9 Batch A: parsing, validation, canonical form and the pure
// decision function (REQ-1, REQ-2, REQ-3).

const validYAML = `schema: policy/v1
defaults:
  read: allow
  write: allow
  dangerous: approval_required
actions:
  github_pr_merge: approval_required
approval_ttl_hours: 24
`

var testKnown = NewKnownActions("github_pr_merge", "github_pr_comment", "code_search", "git_push")

func mustParse(t *testing.T, src string) RuleSet {
	t.Helper()
	rs, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return rs
}

// TC-P1 (REQ-1): a valid file parses, validates and gets the documented defaults.
func TestParse_ValidAndDefaultTTL(t *testing.T) {
	rs := mustParse(t, validYAML)
	if rs.Schema != SchemaV1 || rs.Defaults[TierDangerous] != EffectApprovalRequired || rs.Actions["github_pr_merge"] != EffectApprovalRequired || rs.ApprovalTTL != 24*time.Hour {
		t.Fatalf("%+v", rs)
	}
	if err := Validate(rs, testKnown); err != nil {
		t.Fatal(err)
	}
	// ttl and actions are optional.
	rs = mustParse(t, "schema: policy/v1\ndefaults:\n  read: allow\n  write: deny\n  dangerous: deny\n")
	if rs.ApprovalTTL != DefaultApprovalTTLHours*time.Hour || len(rs.Actions) != 0 {
		t.Fatalf("%+v", rs)
	}
}

// TC-P2 (REQ-1): every malformed file is refused and the error names the path.
func TestParse_RejectsAndNamesThePath(t *testing.T) {
	base := func(mods ...string) string {
		return "schema: policy/v1\ndefaults:\n  read: allow\n  write: allow\n  dangerous: deny\n" + strings.Join(mods, "")
	}
	cases := []struct{ name, src, path string }{
		{"unknown schema", strings.Replace(validYAML, "policy/v1", "policy/v2", 1), "schema"},
		{"schema not a string", strings.Replace(validYAML, "schema: policy/v1", "schema: 1", 1), "schema"},
		{"missing schema", strings.Replace(validYAML, "schema: policy/v1\n", "", 1), "schema"},
		{"unknown top key", validYAML + "extra: 1\n", "extra"},
		{"unknown tier key", strings.Replace(validYAML, "  dangerous:", "  admin: allow\n  dangerous:", 1), "defaults.admin"},
		{"duplicate top key", validYAML + "schema: policy/v1\n", "schema"},
		{"duplicate defaults key", strings.Replace(validYAML, "  read: allow\n", "  read: allow\n  read: deny\n", 1), "defaults.read"},
		{"duplicate action key", strings.Replace(validYAML, "  github_pr_merge: approval_required\n", "  github_pr_merge: allow\n  github_pr_merge: deny\n", 1), "actions.github_pr_merge"},
		{"bad default effect", strings.Replace(validYAML, "read: allow", "read: maybe", 1), "defaults.read"},
		{"bad action effect", strings.Replace(validYAML, "github_pr_merge: approval_required", "github_pr_merge: yes", 1), "actions.github_pr_merge"},
		{"effect not a string", strings.Replace(validYAML, "read: allow", "read: true", 1), "defaults.read"},
		{"ttl zero", strings.Replace(validYAML, "approval_ttl_hours: 24", "approval_ttl_hours: 0", 1), "approval_ttl_hours"},
		{"ttl 169", strings.Replace(validYAML, "approval_ttl_hours: 24", "approval_ttl_hours: 169", 1), "approval_ttl_hours"},
		{"ttl negative", strings.Replace(validYAML, "approval_ttl_hours: 24", "approval_ttl_hours: -1", 1), "approval_ttl_hours"},
		{"ttl not an int", strings.Replace(validYAML, "approval_ttl_hours: 24", "approval_ttl_hours: abc", 1), "approval_ttl_hours"},
		{"ttl float", strings.Replace(validYAML, "approval_ttl_hours: 24", "approval_ttl_hours: 24.5", 1), "approval_ttl_hours"},
		{"ttl quoted", strings.Replace(validYAML, "approval_ttl_hours: 24", `approval_ttl_hours: "24"`, 1), "approval_ttl_hours"},
		{"missing defaults", "schema: policy/v1\nactions: {}\n", "defaults"},
		{"missing dangerous default", "schema: policy/v1\ndefaults:\n  read: allow\n  write: allow\n", "defaults.dangerous"},
		{"missing read default", "schema: policy/v1\ndefaults:\n  write: allow\n  dangerous: deny\n", "defaults.read"},
		{"defaults not a mapping", "schema: policy/v1\ndefaults: allow\n", "defaults"},
		{"bad action name", base("actions:\n  Bad-Name: allow\n"), "actions."},
		{"two documents", validYAML + "---\n" + validYAML, ""},
		{"empty document", "", ""},
		{"not a mapping", "- a\n- b\n", ""},
		{"invalid yaml", "schema: [policy/v1\n", ""},
		{"too large", validYAML + "# " + strings.Repeat("x", MaxSourceBytes) + "\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatal("accepted")
			}
			var errs Errors
			if !errors.As(err, &errs) || len(errs) == 0 {
				t.Fatalf("not a path error list: %T %v", err, err)
			}
			if c.path != "" && !strings.Contains(err.Error(), c.path) {
				t.Fatalf("error %q does not name %q", err.Error(), c.path)
			}
		})
	}
}

// TC-P3 (REQ-1): errors never echo values from the file.
func TestParse_ErrorsNeverEchoValues(t *testing.T) {
	secret := "hunter2-private-value"
	for _, src := range []string{
		"schema: " + secret + "\ndefaults: {read: allow, write: allow, dangerous: deny}\n",
		"schema: policy/v1\ndefaults:\n  read: " + secret + "\n  write: allow\n  dangerous: deny\n",
		"schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\napproval_ttl_hours: " + secret + "\n",
		"schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\nactions:\n  a_b: " + secret + "\n",
		"schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\nactions:\n  \"" + secret + " with space\": allow\n",
		"schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\n\"" + secret + " key\": 1\n",
		secret + ": [\n",
	} {
		_, err := Parse([]byte(src))
		if err == nil {
			t.Fatalf("accepted: %q", src)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error echoes a value: %q", err.Error())
		}
	}
	// Validate (unknown action) names the path, not a value either.
	rs := mustParse(t, strings.Replace(validYAML, "github_pr_merge", "not_a_tool", 1))
	err := Validate(rs, testKnown)
	if err == nil || !strings.Contains(err.Error(), "actions.") || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("%v", err)
	}
}

// TC-P4 (REQ-1): an action that is not a known tool or capability is refused.
func TestValidate_UnknownActionAndShape(t *testing.T) {
	rs := mustParse(t, validYAML)
	if err := Validate(rs, NewKnownActions("something_else")); err == nil {
		t.Fatal("unknown action accepted")
	}
	if err := Validate(rs, nil); err == nil {
		t.Fatal("nil known set accepted an action")
	}
	bad := DefaultV1()
	bad.Defaults[TierWrite] = Effect("maybe")
	if err := Validate(bad, testKnown); err == nil {
		t.Fatal("bad effect accepted")
	}
	bad = DefaultV1()
	delete(bad.Defaults, TierDangerous)
	if err := Validate(bad, testKnown); err == nil {
		t.Fatal("missing tier accepted")
	}
	bad = DefaultV1()
	bad.ApprovalTTL = 169 * time.Hour
	if err := Validate(bad, testKnown); err == nil {
		t.Fatal("ttl out of range accepted")
	}
	bad = DefaultV1()
	bad.ApprovalTTL = 90 * time.Minute
	if err := Validate(bad, testKnown); err == nil {
		t.Fatal("fractional hours accepted")
	}
	bad = DefaultV1()
	bad.Schema = "policy/v2"
	if err := Validate(bad, testKnown); err == nil {
		t.Fatal("schema accepted")
	}
}

// TC-P5 (REQ-1): YAML -> Parse gives the same canonical hash; the seeded v1 is canonical.
func TestCanonical_RoundTripAndDefaultV1(t *testing.T) {
	sets := []RuleSet{DefaultV1(), SafeRuleSet(), mustParse(t, validYAML)}
	many := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: deny, dangerous: deny}\nactions:\n  git_push: allow\n  code_search: deny\n  github_pr_merge: approval_required\napproval_ttl_hours: 168\n")
	sets = append(sets, many)
	for i, rs := range sets {
		body, sum, err := Canonical(rs)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Parse(YAML(rs))
		if err != nil {
			t.Fatalf("set %d: YAML does not parse back: %v", i, err)
		}
		body2, sum2, _ := Canonical(back)
		if sum != sum2 || string(body) != string(body2) {
			t.Fatalf("set %d: hash differs after round trip", i)
		}
		dec, err := DecodeStored(body)
		if err != nil {
			t.Fatal(err)
		}
		if _, sum3, _ := Canonical(dec); sum3 != sum {
			t.Fatalf("set %d: stored decode changes the hash", i)
		}
	}
	// Key order and spacing in the file do not change the hash.
	a := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\nactions: {git_push: allow, code_search: deny}\n")
	b := mustParse(t, "actions:\n  code_search: deny\n  git_push: allow\ndefaults:\n  dangerous: deny\n  write: allow\n  read: allow\nschema: policy/v1\n")
	if _, h1, _ := Canonical(a); func() bool { _, h2, _ := Canonical(b); return h1 != h2 }() {
		t.Fatal("canonical hash depends on file order")
	}
	// DefaultV1JSON is exactly the canonical form of DefaultV1 (migration 00035 seeds this literal).
	body, sum, _ := Canonical(DefaultV1())
	if string(body) != DefaultV1JSON || sum != SHA256Hex([]byte(DefaultV1JSON)) {
		t.Fatalf("DefaultV1JSON drifted from Canonical(DefaultV1()): %s", body)
	}
	if !json.Valid([]byte(DefaultV1JSON)) {
		t.Fatal("DefaultV1JSON is not JSON")
	}
}

// TC-P6 (REQ-1): the stored form is strict; a DB-edited body counts as invalid.
func TestLoadStored(t *testing.T) {
	body, sum, _ := Canonical(DefaultV1())
	ok := StoredRuleSet{Schema: SchemaV1, Body: body, BodySHA256: sum, Version: 1}
	if _, err := LoadStored(ok, NewKnownActions("github_pr_merge")); err != nil {
		t.Fatal(err)
	}
	edited := ok
	edited.Body = []byte(strings.Replace(string(body), `"write":"allow"`, `"write":"deny"`, 1))
	if _, err := LoadStored(edited, NewKnownActions("github_pr_merge")); err == nil {
		t.Fatal("body edited outside the CLI accepted (hash mismatch)")
	}
	for name, s := range map[string]StoredRuleSet{
		"schema":        {Schema: "policy/v2", Body: body, BodySHA256: sum},
		"unknown field": {Schema: SchemaV1, Body: []byte(`{"schema":"policy/v1","extra":1}`), BodySHA256: sum},
		"trailing":      {Schema: SchemaV1, Body: append(append([]byte{}, body...), []byte(`{}`)...), BodySHA256: sum},
		"not json":      {Schema: SchemaV1, Body: []byte(`nope`), BodySHA256: sum},
	} {
		if _, err := LoadStored(s, NewKnownActions("github_pr_merge")); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// A tool that no longer exists makes an old version invalid.
	if _, err := LoadStored(ok, NewKnownActions("other")); err == nil {
		t.Fatal("stored action unknown to the registry accepted")
	}
}

func req(action string, tier Tier) Request {
	return Request{WorkspaceID: "ws", ActorKind: ActorAgent, Action: action, Tier: tier}
}

// TC-R1 (REQ-2): an action rule beats the tier default, both ways.
func TestResolve_ActionRuleBeatsTierDefault(t *testing.T) {
	rs := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\nactions:\n  git_push: deny\n  github_pr_merge: allow\n")
	known := NewKnownActions("git_push", "github_pr_merge", "github_pr_comment")
	d := Resolve(&rs, 3, "", known, req("git_push", TierWrite))
	if d.Effect != EffectDeny || d.Reason != ReasonActionRule || d.RuleKey != "actions.git_push" || d.RuleSetVersion != 3 {
		t.Fatalf("%+v", d)
	}
	d = Resolve(&rs, 3, "", known, req("github_pr_merge", TierDangerous))
	if d.Effect != EffectAllow || d.Reason != ReasonActionRule {
		t.Fatalf("action allow must beat tier deny: %+v", d)
	}
	d = Resolve(&rs, 3, "", known, req("github_pr_comment", TierWrite))
	if d.Effect != EffectAllow || d.Reason != ReasonTierDefault || d.RuleKey != "defaults.write" {
		t.Fatalf("%+v", d)
	}
}

// TC-R2 (REQ-2): with no usable rule set the built-in safe set decides, with the reason.
func TestResolve_SafeSetFallback(t *testing.T) {
	known := NewKnownActions("code_search", "github_pr_comment", "github_pr_merge")
	for _, reason := range []string{ReasonFallbackNoSet, ReasonFallbackInvalid, ReasonFallbackStoreErr} {
		for tier, want := range map[Tier]Effect{TierRead: EffectAllow, TierWrite: EffectApprovalRequired, TierDangerous: EffectDeny} {
			action := map[Tier]string{TierRead: "code_search", TierWrite: "github_pr_comment", TierDangerous: "github_pr_merge"}[tier]
			d := Resolve(nil, 0, reason, known, req(action, tier))
			if d.Effect != want || d.Reason != reason || d.RuleSetVersion != 0 || d.RuleKey != "fallback."+string(tier) {
				t.Fatalf("%s/%s: %+v", reason, tier, d)
			}
		}
	}
	if d := Resolve(nil, 0, "", known, req("github_pr_comment", TierWrite)); d.Reason != ReasonFallbackNoSet {
		t.Fatalf("empty fallback reason: %+v", d)
	}
}

// TC-R3 (REQ-2): an action the registry does not know is denied even when the default allows it.
func TestResolve_UnknownActionDenied(t *testing.T) {
	rs := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: allow}\n")
	for _, tier := range Tiers {
		d := Resolve(&rs, 1, "", NewKnownActions("other"), req("invented_tool", tier))
		if d.Effect != EffectDeny || d.Reason != ReasonUnknownAction {
			t.Fatalf("%s: %+v", tier, d)
		}
	}
	if d := Resolve(&rs, 1, "", nil, req("code_search", TierRead)); d.Effect != EffectDeny {
		t.Fatalf("nil known set allowed: %+v", d)
	}
}

// TC-R4 (REQ-2): a request that does not fit its shape is denied with invalid_request.
func TestResolve_InvalidRequestDenied(t *testing.T) {
	rs := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: allow}\n")
	known := NewKnownActions("code_search")
	ok := req("code_search", TierRead)
	mods := map[string]func(r *Request){
		"no workspace":   func(r *Request) { r.WorkspaceID = "" },
		"bad actor":      func(r *Request) { r.ActorKind = "root" },
		"empty actor":    func(r *Request) { r.ActorKind = "" },
		"bad action":     func(r *Request) { r.Action = "Code Search" },
		"empty action":   func(r *Request) { r.Action = "" },
		"bad tier":       func(r *Request) { r.Tier = "admin" },
		"resource space": func(r *Request) { r.ResourceKey = "a b" },
		"resource utf8":  func(r *Request) { r.ResourceKey = "repo#é" },
		"resource long":  func(r *Request) { r.ResourceKey = strings.Repeat("a", MaxResourceKeyBytes+1) },
		"bad hash":       func(r *Request) { r.InputSHA256 = "xyz" },
		"upper hash":     func(r *Request) { r.InputSHA256 = strings.Repeat("A", 64) },
		"neg execution":  func(r *Request) { r.ExecutionID = -1 },
		"neg tool run":   func(r *Request) { r.ToolRunID = -1 },
		"neg approval":   func(r *Request) { r.ApprovalID = -1 },
	}
	if d := Resolve(&rs, 1, "", known, ok); d.Effect != EffectAllow {
		t.Fatalf("control request denied: %+v", d)
	}
	for name, m := range mods {
		r := ok
		m(&r)
		d := Resolve(&rs, 1, "", known, r)
		if d.Effect != EffectDeny || d.Reason != ReasonInvalidRequest {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	// The boundary itself is valid.
	r := ok
	r.ResourceKey = strings.Repeat("a", MaxResourceKeyBytes)
	r.InputSHA256 = strings.Repeat("a", 64)
	if d := Resolve(&rs, 1, "", known, r); d.Effect != EffectAllow {
		t.Fatalf("boundary request denied: %+v", d)
	}
}

// TC-R5 (REQ-2, REQ-7): the re-check before an approved action.
func TestResolve_RevalidateAndGrantRejected(t *testing.T) {
	allow := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: allow}\n")
	deny := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\n")
	known := NewKnownActions("github_pr_merge")
	r := req("github_pr_merge", TierDangerous)
	r.Revalidate = true
	if d := Resolve(&allow, 1, "", known, r); d.Effect != EffectAllow || d.Reason != ReasonTierDefault {
		t.Fatalf("revalidate under allow: %+v", d)
	}
	if d := Resolve(&deny, 2, "", known, r); d.Effect != EffectDeny || d.Reason != ReasonRevalidateDenied {
		t.Fatalf("revalidate under deny: %+v", d)
	}
	r = req("github_pr_merge", TierDangerous)
	r.GrantRejected = true
	if d := Resolve(&allow, 1, "", known, r); d.Effect != EffectDeny || d.Reason != ReasonRevalidateDenied {
		t.Fatalf("a rejected grant must deny whatever the rules say: %+v", d)
	}
	if d := Resolve(&allow, 1, "", NewKnownActions("x"), r); d.Effect != EffectDeny || d.Reason != ReasonRevalidateDenied {
		t.Fatalf("rejected grant, unknown action: %+v", d)
	}
}

// TC-R6 (REQ-2): a missing or invalid effect inside Evaluate is deny, never allow.
func TestEvaluate_InvalidEffectIsDeny(t *testing.T) {
	rs := RuleSet{Defaults: map[Tier]Effect{TierRead: "bogus"}, Actions: map[string]Effect{"a": "bogus"}}
	if e, _, byAction := Evaluate(rs, "a", TierRead); e != EffectDeny || !byAction {
		t.Fatalf("%v %v", e, byAction)
	}
	if e, _, _ := Evaluate(rs, "b", TierRead); e != EffectDeny {
		t.Fatal("invalid default allowed")
	}
	if e, _, _ := Evaluate(rs, "b", TierWrite); e != EffectDeny {
		t.Fatal("missing default allowed")
	}
}
