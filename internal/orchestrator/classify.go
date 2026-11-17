package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Default Classify vocabularies (REQ-3). Configure Orchestrator.Intents and
// TaskTypes to replace them; values must match ^[a-z][a-z0-9_]{0,63}$.
var (
	DefaultIntents   = []string{"question", "task", "plan", "status", "chat", "other"}
	DefaultTaskTypes = []string{"code_change", "bug_fix", "research", "review", "docs", "ops", "admin", "other"}
)

var labelRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// TaskRef is task context passed to the model: ids and titles only.
type TaskRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ClassifyInput is one request to classify. Request and task titles are
// untrusted text. Source is a fixed label such as "telegram".
type ClassifyInput struct {
	Request string
	Source  string
	Tasks   []TaskRef
}

// Classification is a validated Classify output. Threshold is the
// configured MinConfidence at the time of the call.
type Classification struct {
	Intent     string
	TaskType   string
	Priority   string
	Complexity string
	Confidence float64
	Threshold  float64
}

// NeedsClarification reports whether confidence is below the threshold.
// The caller maps it to REQUEST_CLARIFICATION.
func (c Classification) NeedsClarification() bool { return c.Confidence < c.Threshold }

type classifyOutput struct {
	Intent     string  `json:"intent"`
	TaskType   string  `json:"task_type"`
	Priority   string  `json:"priority"`
	Complexity string  `json:"complexity"`
	Confidence float64 `json:"confidence"`
}

func vocab(configured, def []string) []string {
	var out []string
	for _, v := range uniqueNonEmpty(configured) {
		if labelRe.MatchString(v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// Classify proposes intent, task type, priority and complexity with a
// confidence (REQ-3). Invalid output is repaired within MaxRepair, then
// ErrInvalidOutput. Usage covers every attempt and is returned on error.
func (o *Orchestrator) Classify(ctx context.Context, in ClassifyInput) (Classification, Usage, error) {
	if err := o.ready(); err != nil {
		return Classification{}, Usage{}, err
	}
	request := strings.TrimSpace(in.Request)
	if request == "" {
		return Classification{}, Usage{}, errors.Join(ErrInvalidInput, errors.New("request is empty"))
	}
	intents := vocab(o.Intents, DefaultIntents)
	taskTypes := vocab(o.TaskTypes, DefaultTaskTypes)
	source := ""
	if labelRe.MatchString(in.Source) {
		source = in.Source
	}
	prompt := buildPrompt([]param{
		{"intent vocabulary", intents},
		{"task_type vocabulary", taskTypes},
	}, struct {
		Source  string    `json:"source,omitempty"`
		Request string    `json:"request"`
		Tasks   []TaskRef `json:"tasks,omitempty"`
	}{source, sanitizeRequest(request), boundTasks(in.Tasks)})

	var result Classification
	check := func(raw json.RawMessage) error {
		var c classifyOutput
		if err := json.Unmarshal(raw, &c); err != nil {
			return errors.New("$: output does not decode")
		}
		var errs []string
		if !contains(intents, c.Intent) {
			errs = append(errs, "$.intent: must be one of: "+strings.Join(intents, ", "))
		}
		if !contains(taskTypes, c.TaskType) {
			errs = append(errs, "$.task_type: must be one of: "+strings.Join(taskTypes, ", "))
		}
		if len(errs) > 0 {
			return checkErr(errs)
		}
		result = Classification{Intent: c.Intent, TaskType: c.TaskType, Priority: c.Priority,
			Complexity: c.Complexity, Confidence: c.Confidence}
		return nil
	}
	v, err := generateChecked(ctx, o.Runtime, StructuredRequest{
		Model: o.ClassifyModel, System: classifySystem, Prompt: prompt,
	}, SchemaClassify, o.maxRepair(), check, o.BeforeAttempt)
	u := usageOf(v)
	if err != nil {
		return Classification{}, u, err
	}
	result.Threshold = o.minConfidence()
	return result, u, nil
}

// sanitizeRequest bounds untrusted request text but keeps its line breaks
// readable; JSON encoding in buildPrompt escapes them.
func sanitizeRequest(s string) string {
	s = strings.ToValidUTF8(s, " ")
	r := []rune(s)
	if len(r) > maxRequestRunes {
		r = r[:maxRequestRunes]
	}
	return string(r)
}
