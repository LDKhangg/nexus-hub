package behavior

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// WorkflowDoc is the optional schema 2 orchestration block. Durations are in
// seconds so JSON and YAML use the same bounded scalar representation.
type WorkflowDoc struct {
	Steps []WorkflowStepDoc `json:"steps,omitempty" yaml:"steps,omitempty"`

	Retry *WorkflowRetryDoc `json:"retry,omitempty" yaml:"retry,omitempty"`

	StepTimeoutSeconds *int `json:"step_timeout_seconds,omitempty" yaml:"step_timeout_seconds,omitempty"`

	LeaseSeconds *int `json:"lease_seconds,omitempty" yaml:"lease_seconds,omitempty"`

	ExecutionDeadlineSeconds *int `json:"execution_deadline_seconds,omitempty" yaml:"execution_deadline_seconds,omitempty"`

	WorkerCapabilities []string `json:"worker_capabilities,omitempty" yaml:"worker_capabilities,omitempty"`

	NotificationHandlers []string `json:"notification_handlers,omitempty" yaml:"notification_handlers,omitempty"`

	ApprovalHandlers []string `json:"approval_handlers,omitempty" yaml:"approval_handlers,omitempty"`
}

type WorkflowStepDoc struct {
	Key string `json:"key" yaml:"key"`

	Kind string `json:"kind" yaml:"kind"`

	DependsOn []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`

	WorkerCapabilities []string `json:"worker_capabilities,omitempty" yaml:"worker_capabilities,omitempty"`
}

type WorkflowRetryDoc struct {
	MaxAttempts *int `json:"max_attempts,omitempty" yaml:"max_attempts,omitempty"`

	DelaySeconds []int `json:"delay_seconds,omitempty" yaml:"delay_seconds,omitempty"`
}

func defaultWorkflowDefinition() workflow.Definition {
	return workflow.Definition{
		Retry:         workflow.RetryPolicy{MaxAttempts: 5, Delays: []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}},
		StepTimeout:   10 * time.Minute,
		LeaseDuration: 15 * time.Minute,
	}
}

func workflowDuration(seconds int) time.Duration {
	const maxSeconds = int64(^uint64(0)>>1) / int64(time.Second)
	if int64(seconds) > maxSeconds {
		return -1
	}
	return time.Duration(seconds) * time.Second
}

// NormalizeWorkflow returns the deterministic orchestration pinned to a
// resolved behavior. Schema 1 and schema 2 without a workflow block use the
// legacy built-in graph without changing their stored document.
func NormalizeWorkflow(doc ResolvedDoc) (workflow.Definition, error) {
	definition := defaultWorkflowDefinition()
	if doc.Workflow == nil {
		var previous string
		appendStep := func(key, kind string) {
			step := workflow.StepDefinition{Key: key, Kind: kind}
			if previous != "" {
				step.DependsOn = []string{previous}
			}
			definition.Steps = append(definition.Steps, step)
			previous = key
		}
		if len(doc.Reads) > 0 {
			appendStep("collect_context", workflow.StepKindCollectContext)
		}
		if doc.HasModelStep() {
			appendStep("model", workflow.StepKindModel)
		}
		if len(doc.Actions) > 0 || len(doc.RunActions) > 0 {
			appendStep("actions", workflow.StepKindActions)
		}
		if len(definition.Steps) == 0 {
			appendStep("actions", workflow.StepKindActions)
		}
	} else {
		w := doc.Workflow
		definition.WorkerCapabilities = append([]string(nil), w.WorkerCapabilities...)
		definition.NotificationHandlers = append([]string(nil), w.NotificationHandlers...)
		definition.ApprovalHandlers = append([]string(nil), w.ApprovalHandlers...)
		for _, source := range w.Steps {
			definition.Steps = append(definition.Steps, workflow.StepDefinition{
				Key:                source.Key,
				Kind:               source.Kind,
				DependsOn:          append([]string(nil), source.DependsOn...),
				WorkerCapabilities: append([]string(nil), source.WorkerCapabilities...),
			})
		}
		if w.Retry != nil {
			if w.Retry.MaxAttempts != nil {
				definition.Retry.MaxAttempts = *w.Retry.MaxAttempts
				if w.Retry.DelaySeconds == nil && definition.Retry.MaxAttempts >= 1 && definition.Retry.MaxAttempts <= 5 {
					definition.Retry.Delays = definition.Retry.Delays[:definition.Retry.MaxAttempts-1]
				}
			}
			if w.Retry.DelaySeconds != nil {
				definition.Retry.Delays = make([]time.Duration, len(w.Retry.DelaySeconds))
				for i, seconds := range w.Retry.DelaySeconds {
					definition.Retry.Delays[i] = workflowDuration(seconds)
				}
			}
		}
		if w.StepTimeoutSeconds != nil {
			definition.StepTimeout = workflowDuration(*w.StepTimeoutSeconds)
		}
		if w.LeaseSeconds != nil {
			definition.LeaseDuration = workflowDuration(*w.LeaseSeconds)
		}
		if w.ExecutionDeadlineSeconds != nil {
			definition.ExecutionDeadline = workflowDuration(*w.ExecutionDeadlineSeconds)
		}
	}
	if err := workflow.ValidateDefinition(definition, nil); err != nil {
		return workflow.Definition{}, err
	}
	return definition, nil
}

func validateWorkflow(doc ResolvedDoc, capabilities []string) FieldErrors {
	definition, err := NormalizeWorkflow(doc)
	if err == nil {
		err = workflow.ValidateDefinition(definition, capabilities)
	}
	if err == nil {
		return nil
	}
	var validation workflow.ValidationErrors
	if !errors.As(err, &validation) {
		return FieldErrors{{Path: "workflow", Msg: "is invalid"}}
	}
	out := make(FieldErrors, 0, len(validation))
	for _, item := range validation {
		path := "workflow"
		if item.Path != "" {
			path += "." + item.Path
		}
		out = append(out, FieldError{Path: path, Msg: item.Msg})
	}
	return out
}

// RestoreDraft copies one immutable published version into the mutable draft.
// Publishing the restored draft remains an explicit, separate operation.
func RestoreDraft(ctx context.Context, store DefinitionStore, workspaceID, name string, version int) (Behavior, error) {
	if version <= 0 {
		return Behavior{}, fmt.Errorf("%w: version must be positive", ErrInvalid)
	}
	v, err := store.GetVersion(ctx, workspaceID, name, version)
	if err != nil {
		return Behavior{}, err
	}
	return store.SaveDraft(ctx, workspaceID, name, append([]byte(nil), v.Document...), append([]byte(nil), v.Vars...), v.Source)
}
