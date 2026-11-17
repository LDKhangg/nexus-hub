package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Decisions is the fixed set of evaluation decisions (FR-051).
var Decisions = []string{"EXECUTE", "DELEGATE", "REQUEST_CLARIFICATION", "REQUEST_APPROVAL", "REPLAN", "COMPLETE", "FAIL"}

// Fixed vocabularies shared by the schemas and the plan tables.
var (
	Priorities   = []string{"urgent", "high", "normal", "low"}
	Complexities = []string{"xs", "s", "m", "l", "xl"}
)

// Limits baked into the schemas; Go checks in later tasks apply the
// configured (smaller or equal) limits.
const (
	MaxTitleRunes   = 200
	MaxPlanTasks    = 50
	MaxEvidenceRefs = 20
)

// Schema is one versioned, compiled output schema.
type Schema struct {
	Name    string
	Version int
	Raw     json.RawMessage
	root    *node
}

// Validate checks raw JSON against the schema. The error lists violations by
// path and rule only; it never repeats a value from raw.
func (s *Schema) Validate(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return errors.New("output is not valid JSON")
	}
	if dec.More() {
		return errors.New("output has trailing content after the JSON value")
	}
	var errs []string
	s.root.validate("$", v, &errs)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	if len(errs) > 10 {
		errs = errs[:10]
	}
	return errors.New(strings.Join(errs, "; "))
}

var registry = map[string]*Schema{}

func register(name string, version int, raw string) *Schema {
	root, err := compileNode([]byte(raw))
	if err != nil {
		panic(fmt.Sprintf("orchestrator schema %s: %v", name, err))
	}
	s := &Schema{Name: name, Version: version, Raw: json.RawMessage(raw), root: root}
	registry[fmt.Sprintf("%s@%d", name, version)] = s
	return s
}

// LookupSchema returns a registered schema by name and version.
func LookupSchema(name string, version int) (*Schema, bool) {
	s, ok := registry[fmt.Sprintf("%s@%d", name, version)]
	return s, ok
}

func enumJSON(vals []string) string {
	b, _ := json.Marshal(vals)
	return string(b)
}

var (
	// SchemaClassify is the Classify output (FR-043..046).
	SchemaClassify = register("classify", 1, `{
  "type":"object","additionalProperties":false,
  "required":["intent","task_type","priority","complexity","confidence"],
  "properties":{
    "intent":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,63}$"},
    "task_type":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,63}$"},
    "priority":{"type":"string","enum":`+enumJSON(Priorities)+`},
    "complexity":{"type":"string","enum":`+enumJSON(Complexities)+`},
    "confidence":{"type":"number","minimum":0,"maximum":1}
  }}`)

	// SchemaPlan is the Plan output (FR-047, FR-048, FR-050, FR-056).
	SchemaPlan = register("plan", 1, `{
  "type":"object","additionalProperties":false,
  "required":["tasks","decision"],
  "properties":{
    "tasks":{"type":"array","minItems":1,"maxItems":50,"items":{
      "type":"object","additionalProperties":false,
      "required":["key","title","capabilities","depends_on","priority","complexity"],
      "properties":{
        "key":{"type":"string","pattern":"^[a-z][a-z0-9_-]{0,31}$"},
        "title":{"type":"string","minLength":1,"maxLength":200},
        "capabilities":{"type":"array","maxItems":16,"items":{"type":"string","pattern":"^[a-z][a-z0-9_.]{0,63}$"}},
        "depends_on":{"type":"array","maxItems":32,"items":{"type":"string","pattern":"^[a-z][a-z0-9_-]{0,31}$"}},
        "priority":{"type":"string","enum":`+enumJSON(Priorities)+`},
        "complexity":{"type":"string","enum":`+enumJSON(Complexities)+`},
        "acceptance_hint":{"type":"string","maxLength":500},
        "executor":{"type":"string","pattern":"^[a-z][a-z0-9_.-]{0,63}$"}
      }}},
    "decision":{"type":"string","enum":`+enumJSON(Decisions)+`},
    "budget":{"type":"object","additionalProperties":false,"properties":{
      "plan_iterations":{"type":"integer","minimum":0,"maximum":100},
      "replans":{"type":"integer","minimum":0,"maximum":100},
      "tokens":{"type":"integer","minimum":0,"maximum":100000000},
      "wall_seconds":{"type":"integer","minimum":0,"maximum":604800}
    }}
  }}`)

	// SchemaEvaluate is the Evaluate output (FR-051, FR-052).
	SchemaEvaluate = register("evaluate", 1, `{
  "type":"object","additionalProperties":false,
  "required":["decision","reason_code"],
  "properties":{
    "decision":{"type":"string","enum":`+enumJSON(Decisions)+`},
    "reason_code":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,63}$"},
    "task_key":{"type":"string","pattern":"^[a-z][a-z0-9_-]{0,31}$"},
    "evidence_refs":{"type":"array","maxItems":20,"items":{"type":"string","minLength":1,"maxLength":128}},
    "next_action":{"type":"string","maxLength":300}
  }}`)
)

// Validated is a schema-valid output with the usage of every attempt.
type Validated struct {
	JSON     json.RawMessage
	Usage    domain.Usage
	CostUSD  float64
	Attempts int
}

// GenerateValidated calls rt and validates the output against schema. On
// invalid output it retries at most maxRepair times, feeding back only the
// validation error text (never the bad output). Still invalid: it returns
// ErrInvalidOutput and the usage spent, so the caller can still charge it.
// A runtime error is returned at once; it is not repaired.
func GenerateValidated(ctx context.Context, rt AIRuntime, req StructuredRequest, schema *Schema, maxRepair int) (Validated, error) {
	if rt == nil || schema == nil {
		return Validated{}, errors.New("orchestrator: runtime and schema are required")
	}
	if maxRepair < 0 {
		maxRepair = 0
	}
	req.SchemaName, req.SchemaVersion, req.Schema = schema.Name, schema.Version, schema.Raw
	basePrompt := req.Prompt
	var out Validated
	for attempt := 0; attempt <= maxRepair; attempt++ {
		resp, err := rt.Generate(ctx, req)
		out.Attempts++
		if err != nil {
			return out, err
		}
		out.Usage.Add(resp.Usage)
		out.CostUSD += resp.CostUSD
		verr := schema.Validate(resp.JSON)
		if verr == nil {
			out.JSON = append(json.RawMessage(nil), resp.JSON...)
			return out, nil
		}
		req.Prompt = basePrompt + "\n\nYour previous answer was rejected: " + verr.Error() +
			"\nReturn one corrected JSON value that matches the schema."
	}
	return out, fmt.Errorf("%w: %s", ErrInvalidOutput, ReasonInvalidOutput)
}
