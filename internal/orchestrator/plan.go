package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// BudgetRequest is a plan's budget, and also the caps it is clamped to.
type BudgetRequest struct {
	PlanIterations int   `json:"plan_iterations"`
	Replans        int   `json:"replans"`
	Tokens         int64 `json:"tokens"`
	WallSeconds    int   `json:"wall_seconds"`
}

// PlanTask is one validated plan task. Executor is only the model's
// proposal; the capability registry chooses the real executor.
type PlanTask struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	Capabilities   []string `json:"capabilities"`
	DependsOn      []string `json:"depends_on"`
	Priority       string   `json:"priority"`
	Complexity     string   `json:"complexity"`
	AcceptanceHint string   `json:"acceptance_hint,omitempty"`
	Executor       string   `json:"executor,omitempty"`
}

// Plan is a validated Plan output. Order lists every task key in a
// dependency-respecting order (ties keep the model's order). Budget is
// already clamped to the caps.
type Plan struct {
	Tasks    []PlanTask
	Decision string
	Budget   BudgetRequest
	Order    []string
}

// Task returns the task with key.
func (p Plan) Task(key string) (PlanTask, bool) {
	for _, t := range p.Tasks {
		if t.Key == key {
			return t, true
		}
	}
	return PlanTask{}, false
}

// PlanInput is one Plan call. Request, task titles and the previous plan
// are untrusted text. Capabilities, MaxTasks and Caps are trusted
// configuration: MaxTasks <= 0 or above MaxPlanTasks means MaxPlanTasks; a
// cap <= 0 allows nothing for that counter.
type PlanInput struct {
	Request        string
	Classification *Classification
	Tasks          []TaskRef
	Capabilities   []string
	MaxTasks       int
	Caps           BudgetRequest
	Previous       *Plan
	ReplanReason   string
}

type planOutput struct {
	Tasks    []PlanTask `json:"tasks"`
	Decision string     `json:"decision"`
	Budget   *struct {
		PlanIterations *int   `json:"plan_iterations"`
		Replans        *int   `json:"replans"`
		Tokens         *int64 `json:"tokens"`
		WallSeconds    *int   `json:"wall_seconds"`
	} `json:"budget"`
}

// Plan proposes a plan (REQ-4). After schema validation Go checks unique
// keys, known capabilities, existing and acyclic dependencies, the task
// limit and the initial decision; a failing check is repaired like a schema
// error within MaxRepair, then ErrInvalidOutput. The budget request is
// clamped to Caps and never widened. Usage is returned on error too.
func (o *Orchestrator) Plan(ctx context.Context, in PlanInput) (Plan, Usage, error) {
	if err := o.ready(); err != nil {
		return Plan{}, Usage{}, err
	}
	request := strings.TrimSpace(in.Request)
	if request == "" {
		return Plan{}, Usage{}, errors.Join(ErrInvalidInput, errors.New("request is empty"))
	}
	caps := uniqueNonEmpty(in.Capabilities)
	if len(caps) == 0 {
		return Plan{}, Usage{}, errors.Join(ErrInvalidInput, errors.New("no known capabilities"))
	}
	maxTasks := in.MaxTasks
	if maxTasks <= 0 || maxTasks > MaxPlanTasks {
		maxTasks = MaxPlanTasks
	}
	budgetCaps := BudgetRequest{
		PlanIterations: max(in.Caps.PlanIterations, 0),
		Replans:        max(in.Caps.Replans, 0),
		Tokens:         max(in.Caps.Tokens, 0),
		WallSeconds:    max(in.Caps.WallSeconds, 0),
	}

	type classData struct {
		Intent     string  `json:"intent"`
		TaskType   string  `json:"task_type"`
		Priority   string  `json:"priority"`
		Complexity string  `json:"complexity"`
		Confidence float64 `json:"confidence"`
	}
	data := struct {
		Request        string     `json:"request"`
		Classification *classData `json:"classification,omitempty"`
		Tasks          []TaskRef  `json:"tasks,omitempty"`
		PreviousPlan   []PlanTask `json:"previous_plan,omitempty"`
		ReplanReason   string     `json:"replan_reason,omitempty"`
	}{Request: sanitizeRequest(request), Tasks: boundTasks(in.Tasks)}
	if c := in.Classification; c != nil {
		data.Classification = &classData{c.Intent, c.TaskType, c.Priority, c.Complexity, c.Confidence}
	}
	if in.Previous != nil {
		data.PreviousPlan = boundPlanTasks(in.Previous.Tasks)
		data.ReplanReason = sanitizeText(in.ReplanReason, maxReasonRunes)
	}
	prompt := buildPrompt([]param{
		{"known capabilities", caps},
		{"max tasks", maxTasks},
		{"budget caps", budgetCaps},
		{"replan", in.Previous != nil},
	}, data)

	var result Plan
	check := func(raw json.RawMessage) error {
		var p planOutput
		if err := json.Unmarshal(raw, &p); err != nil {
			return errors.New("$: output does not decode")
		}
		tasks, order, err := checkPlanTasks(p.Tasks, caps, maxTasks)
		if p.Decision == DecisionReplan || p.Decision == DecisionComplete {
			err = errors.Join(err, errors.New("$.decision: must not be REPLAN or COMPLETE for a new plan"))
		}
		if err != nil {
			return checkErr(strings.Split(err.Error(), "\n"))
		}
		result = Plan{Tasks: tasks, Decision: p.Decision, Order: order}
		if b := p.Budget; b != nil {
			result.Budget = BudgetRequest{
				PlanIterations: clampInt(b.PlanIterations, budgetCaps.PlanIterations),
				Replans:        clampInt(b.Replans, budgetCaps.Replans),
				Tokens:         clampInt(b.Tokens, budgetCaps.Tokens),
				WallSeconds:    clampInt(b.WallSeconds, budgetCaps.WallSeconds),
			}
		} else {
			result.Budget = budgetCaps
		}
		return nil
	}
	v, err := generateChecked(ctx, o.Runtime, StructuredRequest{
		Model: o.PlanModel, System: planSystem, Prompt: prompt,
	}, SchemaPlan, o.maxRepair(), check, o.BeforeAttempt)
	u := usageOf(v)
	if err != nil {
		return Plan{}, u, err
	}
	return result, u, nil
}

// clampInt returns the requested value bounded by [0, cap]; an absent
// request gets the cap. It never returns more than cap.
func clampInt[T int | int64](req *T, cap T) T {
	if cap <= 0 {
		return 0
	}
	if req == nil {
		return cap
	}
	return min(max(*req, 0), cap)
}

// checkPlanTasks applies the Go plan checks and returns sanitized tasks and
// a topological order. Errors are newline-separated path/rule messages that
// never repeat output values.
func checkPlanTasks(in []PlanTask, known []string, maxTasks int) ([]PlanTask, []string, error) {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if len(in) == 0 {
		add("$.tasks: needs at least 1 item")
	}
	if len(in) > maxTasks {
		add("$.tasks: allows at most %d items", maxTasks)
	}
	index := map[string]int{}
	for i, t := range in {
		if _, dup := index[t.Key]; dup {
			add("$.tasks[%d].key: duplicate key", i)
			continue
		}
		index[t.Key] = i
	}
	tasks := make([]PlanTask, len(in))
	for i, t := range in {
		t.Title = sanitizeText(t.Title, MaxTitleRunes)
		if t.Title == "" {
			add("$.tasks[%d].title: must not be empty", i)
		}
		t.AcceptanceHint = sanitizeText(t.AcceptanceHint, 500)
		t.Capabilities = uniqueNonEmpty(t.Capabilities)
		if len(t.Capabilities) == 0 {
			add("$.tasks[%d].capabilities: needs at least 1 item", i)
		}
		for j, c := range in[i].Capabilities {
			if !contains(known, c) {
				add("$.tasks[%d].capabilities[%d]: unknown capability; use only the known capabilities", i, j)
			}
		}
		for j, d := range in[i].DependsOn {
			if d == t.Key {
				add("$.tasks[%d].depends_on[%d]: must not reference its own task", i, j)
			} else if _, ok := index[d]; !ok {
				add("$.tasks[%d].depends_on[%d]: no task has this key", i, j)
			}
		}
		t.DependsOn = uniqueNonEmpty(t.DependsOn)
		tasks[i] = t
	}
	if len(errs) > 0 {
		return nil, nil, errors.New(strings.Join(errs, "\n"))
	}
	order, ok := topoOrder(tasks)
	if !ok {
		return nil, nil, errors.New("$.tasks: depends_on forms a cycle")
	}
	return tasks, order, nil
}

// topoOrder returns a stable topological order (Kahn; ties keep input
// order) or false on a cycle. Keys are unique and dependencies exist.
func topoOrder(tasks []PlanTask) ([]string, bool) {
	indeg := make(map[string]int, len(tasks))
	for _, t := range tasks {
		indeg[t.Key] = len(t.DependsOn)
	}
	done := make(map[string]bool, len(tasks))
	order := make([]string, 0, len(tasks))
	for len(order) < len(tasks) {
		progressed := false
		for _, t := range tasks {
			if done[t.Key] || indeg[t.Key] > 0 {
				continue
			}
			done[t.Key] = true
			order = append(order, t.Key)
			progressed = true
			for _, u := range tasks {
				if contains(u.DependsOn, t.Key) {
					indeg[u.Key]--
				}
			}
			break
		}
		if !progressed {
			return nil, false
		}
	}
	return order, true
}

func boundPlanTasks(tasks []PlanTask) []PlanTask {
	out := make([]PlanTask, 0, min(len(tasks), MaxPlanTasks))
	for _, t := range tasks {
		if len(out) == MaxPlanTasks {
			break
		}
		out = append(out, PlanTask{
			Key: sanitizeText(t.Key, 32), Title: sanitizeText(t.Title, MaxTitleRunes),
			Capabilities: t.Capabilities, DependsOn: t.DependsOn,
			Priority: t.Priority, Complexity: t.Complexity,
		})
	}
	return out
}
