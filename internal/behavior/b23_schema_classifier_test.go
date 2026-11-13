package behavior

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func b23Ptr[T any](v T) *T { return &v }

func b23Doc() Doc {
	return Doc{Schema: 1, Name: "owner-review", Instructions: "Analyze the supplied evidence.",
		Triggers: []TriggerDoc{{Type: TriggerTelegramCommand, Command: "/review", Args: []string{"task"}}},
		Actions:  map[string][]ActionDoc{"auto": {{Type: ActionClickUpComment}}, "ask": {{Type: ActionClickUpComment}}}}
}

func b23Version(t *testing.T, doc Doc, id int64) Version {
	t.Helper()
	raw, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return Version{ID: id, BehaviorID: id, Name: doc.Name, Version: int(id), Document: raw, Vars: json.RawMessage(`{}`)}
}

// P3-01 (REQ-1, REQ-2): owners receive every bad field path, not a first-error-only response.
func TestB23SchemaReportsNestedUnknownFieldsAndTypes(t *testing.T) {
	for _, tc := range []struct {
		format Format
		source string
	}{
		{FormatJSON, `{"schema":1,"name":"owner-review","surprise":true,"autonomy":{"class":"auto"},"reads":[{"tool":42,"max_bytes":"huge"}],"actions":{"ask":[{"type":"clickup.comment","target":"evil"}]}}`},
		{FormatYAML, "schema: 1\nname: owner-review\nsurprise: true\nautonomy:\n  class: auto\nreads:\n  - tool: 42\n    max_bytes: huge\nactions:\n  ask:\n    - type: clickup.comment\n      target: evil\n"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			_, err := Parse([]byte(tc.source), tc.format)
			if err == nil {
				t.Fatal("unsafe document accepted")
			}
			for _, path := range []string{"surprise", "autonomy.class", "reads[0].tool", "reads[0].max_bytes", "actions.ask[0].target"} {
				if !strings.Contains(err.Error(), path) {
					t.Errorf("missing error path %s", path)
				}
			}
		})
	}
}

// P3-01 (REQ-2): unknown tools/actions, invalid bounds and forged write targets fail together.
func TestB23ValidationRejectsCapabilitiesAndBudgetAbuse(t *testing.T) {
	d := b23Doc()
	d.Reads = []ReadDoc{{Tool: "shell_exec", MaxBytes: MaxReadBytes + 1}, {Tool: "clickup_get_task", MaxBytes: -1}}
	d.Actions["auto"] = []ActionDoc{{Type: "github.force_push"}, {Type: ActionClickUpComment, Params: map[string]string{"task_id": "attacker"}}}
	d.Budgets = &BudgetsDoc{MaxTokens: b23Ptr(0), MaxItems: b23Ptr(101), MaxSteps: b23Ptr(21)}
	d.AskExpiryHours = b23Ptr(0)
	d.Autonomy = &AutonomyDoc{ConfidenceThreshold: b23Ptr(1.1)}
	errs := FieldErrors(Validate(d, nil, []string{"clickup_get_task"})).Error()
	for _, p := range []string{"reads[0].tool", "reads[0].max_bytes", "reads[1].max_bytes", "actions.auto[0].type", "actions.auto[1].params.task_id", "budgets.max_tokens", "budgets.max_items", "budgets.max_steps", "ask_expiry_hours", "autonomy.confidence_threshold"} {
		if !strings.Contains(errs, p) {
			t.Errorf("missing %s", p)
		}
	}
	if got := Validate(b23Doc(), nil, nil); len(got) != 0 {
		t.Fatalf("valid minimal document rejected: %v", got)
	}
	for _, src := range []string{strings.Repeat("x", MaxSourceBytes+1), "{} {}", "null"} {
		if _, err := Parse([]byte(src), FormatJSON); err == nil {
			t.Error("malformed/oversized document accepted")
		}
	}
}

// P3-12 (REQ-24): settings substitute as data in every supported site, never recursively.
func TestB23VarsResolveAcrossTriggersInstructionsAndActions(t *testing.T) {
	d := b23Doc()
	d.Vars = map[string]VarDoc{"status": {Default: b23Ptr("draft")}, "list": {}, "literal": {}}
	d.Triggers = []TriggerDoc{{Type: TriggerTaskStatusChanged, Filter: FilterDoc{ToStatus: []string{"{{vars.status}}"}, List: []string{"{{vars.list}}"}}}}
	d.Instructions = "Check {{vars.literal}} at {{vars.status}}"
	d.Actions["ask"] = []ActionDoc{{Type: ActionSetStatus, Params: map[string]string{"status": "{{vars.status}}"}}}
	r, err := Resolve(d, map[string]string{"status": "review", "list": "list-1", "literal": "{{vars.status}}"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Instructions != "Check {{vars.status}} at review" || r.Triggers[0].ToStatus[0] != "review" || r.Triggers[0].Lists[0] != "list-1" || r.ActionsFor(ClassAsk)[0].Params["status"] != "review" {
		t.Fatalf("substitution changed settings: %+v", r)
	}
	for _, vals := range []map[string]string{{}, {"unknown": "x"}, {"list": "../evil"}, {"list": "list-1", "literal": strings.Repeat("x", MaxVarValueChars+1)}} {
		if _, err := Resolve(d, vals); err == nil {
			t.Error("invalid values accepted")
		}
	}
}

// P3-05 (REQ-9): the model cannot choose class, resource, tools or bypass caps.
func TestB23GenericOutputRejectsForgedActionsAndMalformedItems(t *testing.T) {
	base := map[string]any{"key": "q1", "label": "finding", "text": "Cần xác nhận ✅", "proposal": "Giữ nguyên", "citations": []any{}, "topic": "design", "confidence": 0.9, "informational": false}
	for _, tc := range []struct {
		name      string
		mutate    func(map[string]any)
		duplicate bool
	}{
		{"class", func(m map[string]any) { m["class"] = "auto" }, false},
		{"target", func(m map[string]any) { m["target"] = "clickup:task:evil" }, false},
		{"missing-confidence", func(m map[string]any) { delete(m, "confidence") }, false},
		{"confidence-range", func(m map[string]any) { m["confidence"] = 1.01 }, false},
		{"empty-text", func(m map[string]any) { m["text"] = " \n " }, false},
		{"oversize", func(m map[string]any) { m["proposal"] = strings.Repeat("🙂", MaxItemProposalChars+1) }, false},
		{"duplicate-key", func(m map[string]any) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			tc.mutate(m)
			items := []any{m}
			if tc.duplicate {
				items = append(items, m)
			}
			raw, _ := json.Marshal(map[string]any{"items": items})
			if out, errs := ParseItems(string(raw), 10); len(errs) == 0 || out != nil {
				t.Fatal("invalid model output became actionable")
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{"items": []any{base}})
	if out, errs := ParseItems("```json\n"+string(raw)+"\n```", 1); len(errs) != 0 || len(out) != 1 || out[0].Text != "Cần xác nhận ✅" {
		t.Fatal("valid generic output rejected")
	}
	if _, errs := ParseItems(string(raw), 0); len(errs) == 0 {
		t.Fatal("item cap bypassed")
	}
}

// P3-06 (REQ-10): engine policy dominates confidence, including NaN and exact threshold boundaries.
func TestB23ClassifierNeverLetsConfidenceOverrideEvidenceOrOwnerPolicy(t *testing.T) {
	rules := AutonomyRules{ConfidenceThreshold: .8, RequireCitations: true, AlwaysAskTopics: []string{"Billing"}, NotifyWhenInformational: true}
	for _, tc := range []struct {
		name       string
		confidence float64
		topic      string
		cites      []Citation
		valid      []bool
		injection  bool
		want       Class
		rule       string
	}{
		{"threshold", .8, "design", []Citation{{Locator: "source:L1", Quote: "evidence"}}, []bool{true}, false, ClassAuto, RuleAutoAllChecks},
		{"below", .799, "design", []Citation{{}}, []bool{true}, false, ClassAsk, RuleAskLowConfidence},
		{"nan", math.NaN(), "design", []Citation{{}}, []bool{true}, false, ClassAsk, RuleAskLowConfidence},
		{"always-ask", 1, " billing ", []Citation{{}}, []bool{true}, false, ClassAsk, RuleAskTopicAlwaysAsk},
		{"missing", 1, "design", nil, nil, false, ClassAsk, RuleAskCitationMissing},
		{"fabricated", 1, "design", []Citation{{}}, []bool{false}, false, ClassAsk, RuleAskCitationInvalid},
		{"injection", 1, "design", []Citation{{}}, []bool{true}, true, ClassAsk, RuleAskInstructionLike},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(ModelItem{Proposal: "answer", Topic: tc.topic, Confidence: tc.confidence, Citations: tc.cites}, rules, tc.valid, tc.injection)
			if got.Class != tc.want || got.Rule != tc.rule {
				t.Fatalf("got %+v, want %s %s", got, tc.want, tc.rule)
			}
		})
	}
	got := Classify(ModelItem{Informational: true}, rules, nil, false)
	if got.Class != ClassNotify {
		t.Fatalf("informational item = %+v", got)
	}
	rules.NotifyWhenInformational = false
	if Classify(ModelItem{Informational: true}, rules, nil, false).Class != ClassAsk {
		t.Fatal("notify override ignored")
	}
}

// P3-06 (REQ-10): accepted T5 ruling permits a two-line window and whitespace
// normalization, but not invented evidence or short partial-word quotes.
func TestB23CitationHonorsApprovedToleranceAndQuoteBoundaries(t *testing.T) {
	snap := ContextSnapshot{Fragments: []Fragment{{Source: "clickup:task/abc#desc", Lines: []string{"First evidence line", "Keep  two spaces", "Unrelated third line", "Fourth line", "OK", "Sixth line"}}}}
	for _, tc := range []struct {
		name, locator, quote string
		want                 bool
	}{
		{"exact", "clickup:task/abc#desc:L1", "First evidence line", true},
		{"two-lines-away", "clickup:task/abc#desc:L3", "First evidence line", true},
		{"three-lines-away", "clickup:task/abc#desc:L4", "First evidence line", false},
		{"normalized-whitespace", "clickup:task/abc#desc:L2", "Keep two spaces", true},
		{"short-whole-line", "clickup:task/abc#desc:L5", "OK", true},
		{"short-partial-line", "clickup:task/abc#desc:L1", "First", false},
		{"oversize-range", "clickup:task/abc#desc:L1-L51", "First evidence line", false},
		{"reversed-range", "clickup:task/abc#desc:L3-L1", "First evidence line", false},
		{"unknown-source", "clickup:task/other#desc:L1", "First evidence line", false},
		{"invented-quote", "clickup:task/abc#desc:L1", "Approved without evidence", false},
		{"zero-line", "clickup:task/abc#desc:L0", "First evidence line", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := VerifyCitation(&snap, Citation{Locator: tc.locator, Quote: tc.quote})
			if got != tc.want {
				t.Fatalf("verified=%v, want %v", got, tc.want)
			}
		})
	}
}
