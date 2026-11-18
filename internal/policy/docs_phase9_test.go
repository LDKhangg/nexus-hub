package policy

import (
	"os"
	"strings"
	"testing"
)

// Phase 9 Batch B (REQ-12): the runbook section exists, its sample rule file
// is the v1 default, and every reason code the code can return is explained.

func p9ReadDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TC-DOC-1 (REQ-12): the section, its parts and the sample file.
func TestP9Runbook_PolicySectionAndSample(t *testing.T) {
	doc := p9ReadDoc(t, "docs/runbooks/self-host-home.md")
	i := strings.Index(doc, "\n## Policy and approvals (Phase 9)\n")
	if i < 0 {
		t.Fatal("the runbook has no section \"Policy and approvals (Phase 9)\"")
	}
	section := doc[i+1:]
	if j := strings.Index(section[3:], "\n## "); j >= 0 {
		section = section[:j+3]
	}
	for _, want := range []string{"### The rule file", "### `nh policy`", "### Merge approval", "### Reason codes you may see", "### `allow_dangerous` is retired",
		"nh policy validate", "nh policy apply", "nh policy rollback", "Duyệt", "Từ chối", "approval_ttl_hours", "Tier escalation", "Safe fallback set"} {
		if !strings.Contains(section, want) {
			t.Errorf("the section does not mention %q", want)
		}
	}
	// The sample rule file is a valid policy/v1 file equal to the default rule set.
	start := strings.Index(section, "```yaml\n")
	if start < 0 {
		t.Fatal("no sample rule file")
	}
	rest := section[start+len("```yaml\n"):]
	sample := rest[:strings.Index(rest, "```")]
	rs, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("the sample does not parse: %v", err)
	}
	_, got, err := Canonical(rs)
	if err != nil {
		t.Fatal(err)
	}
	_, want, _ := Canonical(DefaultV1())
	if got != want {
		t.Fatal("the runbook's sample rule file is not the default rule set (v1)")
	}
	if _, err := LoadStored(StoredRuleSet{Schema: SchemaV1, Body: []byte(DefaultV1JSON), BodySHA256: SHA256Hex([]byte(DefaultV1JSON))}, NewKnownActions("github_pr_merge")); err != nil {
		t.Fatalf("v1 does not load: %v", err)
	}
}

// TC-DOC-2 (REQ-12): each reason code the policy and merge paths return is in the runbook, and the architecture note names where Decide is called.
func TestP9Runbook_ReasonCodesAreExplained(t *testing.T) {
	doc := p9ReadDoc(t, "docs/runbooks/self-host-home.md")
	for _, code := range []string{"policy_denied", "approval_required", "revalidate_denied", "approval_rejected", "approval_expired",
		"approval_pending_elsewhere", "approval_not_sent", "merge_not_configured", "no_owner_member", "head_sha_mismatch", "tier_escalated",
		ReasonActionRule, ReasonTierDefault, ReasonUnknownAction, ReasonInvalidRequest, ReasonFallbackNoSet, ReasonFallbackInvalid,
		ReasonFallbackStoreErr, ReasonRecordFailed, ReasonEngineUnavailable} {
		if !strings.Contains(doc, "`"+code+"`") && !strings.Contains(doc, code) {
			t.Errorf("the runbook does not explain %q", code)
		}
	}
	arch := p9ReadDoc(t, "docs/architecture.md")
	if !strings.Contains(arch, "policy.Engine.Decide") {
		t.Error("docs/architecture.md does not say where Decide is called")
	}
}
