package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Evidence reference kinds.
const (
	EvidenceStepOutput = "step_output"
	EvidenceResource   = "resource"
	EvidenceArtifact   = "artifact"
)

// maxEvidenceRefRunes matches the evaluate schema item maxLength.
const maxEvidenceRefRunes = 128

// EvidenceRef points at proof of a task result: a step output id, a
// resource binding or an artifact ref/URL. ID is matched exactly.
type EvidenceRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// TaskResult is the structured result of one task. Outcome and ReasonCode
// are labels from the executor; Summary is untrusted text.
type TaskResult struct {
	Outcome    string
	ReasonCode string
	Summary    string
	Evidence   []EvidenceRef
}

// EvaluateInput is one Evaluate call for TaskKey in Plan.
type EvaluateInput struct {
	Plan    Plan
	TaskKey string
	Result  TaskResult
}

// Decision is a validated Evaluate output. EvidenceRefs is a subset of the
// input evidence ids; TaskKey, when set, names a task of the plan. Rules
// such as COMPLETE-needs-evidence are applied by the caller.
type Decision struct {
	Decision     string
	ReasonCode   string
	TaskKey      string
	NextAction   string
	EvidenceRefs []string
}

type evaluateOutput struct {
	Decision     string   `json:"decision"`
	ReasonCode   string   `json:"reason_code"`
	TaskKey      string   `json:"task_key"`
	EvidenceRefs []string `json:"evidence_refs"`
	NextAction   string   `json:"next_action"`
}

// Evaluate decides what follows a task result (REQ-6): one of Decisions, a
// reason code and a next-action recommendation. Evidence refs outside the
// input set or an unknown task key are invalid output and are repaired
// within MaxRepair, then ErrInvalidOutput. Usage is returned on error too.
func (o *Orchestrator) Evaluate(ctx context.Context, in EvaluateInput) (Decision, Usage, error) {
	if err := o.ready(); err != nil {
		return Decision{}, Usage{}, err
	}
	if _, ok := in.Plan.Task(in.TaskKey); !ok {
		return Decision{}, Usage{}, errors.Join(ErrInvalidInput, errors.New("task key is not in the plan"))
	}
	refs := map[string]bool{}
	evidence := make([]EvidenceRef, 0, len(in.Result.Evidence))
	for _, e := range in.Result.Evidence {
		id := strings.TrimSpace(e.ID)
		if id == "" || utf8.RuneCountInString(id) > maxEvidenceRefRunes || refs[id] {
			continue
		}
		refs[id] = true
		kind := ""
		if labelRe.MatchString(e.Kind) {
			kind = e.Kind
		}
		evidence = append(evidence, EvidenceRef{Kind: kind, ID: id})
	}
	label := func(s string) string {
		if labelRe.MatchString(s) {
			return s
		}
		return ""
	}
	prompt := buildPrompt([]param{
		{"task key", in.TaskKey},
		{"plan task keys", taskKeys(in.Plan)},
	}, struct {
		Plan   []PlanTask `json:"plan"`
		Result any        `json:"task_result"`
	}{boundPlanTasks(in.Plan.Tasks), struct {
		Outcome    string        `json:"outcome,omitempty"`
		ReasonCode string        `json:"reason_code,omitempty"`
		Summary    string        `json:"summary,omitempty"`
		Evidence   []EvidenceRef `json:"evidence"`
	}{label(in.Result.Outcome), label(in.Result.ReasonCode), sanitizeText(in.Result.Summary, maxSummaryRunes), evidence}})

	var result Decision
	check := func(raw json.RawMessage) error {
		var d evaluateOutput
		if err := json.Unmarshal(raw, &d); err != nil {
			return errors.New("$: output does not decode")
		}
		var errs []string
		if d.TaskKey != "" {
			if _, ok := in.Plan.Task(d.TaskKey); !ok {
				errs = append(errs, "$.task_key: no plan task has this key")
			}
		}
		for i, r := range d.EvidenceRefs {
			if !refs[r] {
				errs = append(errs, fmt.Sprintf("$.evidence_refs[%d]: not one of the task result evidence ids", i))
			}
		}
		if len(errs) > 0 {
			return checkErr(errs)
		}
		result = Decision{
			Decision: d.Decision, ReasonCode: d.ReasonCode, TaskKey: d.TaskKey,
			NextAction:   sanitizeText(d.NextAction, maxNextAction),
			EvidenceRefs: uniqueNonEmpty(d.EvidenceRefs),
		}
		return nil
	}
	v, err := generateChecked(ctx, o.Runtime, StructuredRequest{
		Model: o.EvaluateModel, System: evaluateSystem, Prompt: prompt,
	}, SchemaEvaluate, o.maxRepair(), check)
	u := usageOf(v)
	if err != nil {
		return Decision{}, u, err
	}
	return result, u, nil
}

func taskKeys(p Plan) []string {
	keys := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		keys = append(keys, t.Key)
	}
	return keys
}
