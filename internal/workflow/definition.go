// Package workflow contains the SDK-free workflow domain and runtime types.
package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	MaxSteps = 64

	StepKindCollectContext = "collect_context"

	StepKindModel = "model"

	StepKindActions = "actions"

	StepKindNotify = "notify"

	StepKindApproval = "approval"

	CapabilityInProcess = "in_process"

	NotificationHandlerTelegram = "telegram.message"

	ApprovalHandlerBehavior = "behavior.action"
)

var registeredStepKinds = map[string]bool{StepKindCollectContext: true, StepKindModel: true, StepKindActions: true, StepKindNotify: true, StepKindApproval: true}

// Definition is the immutable orchestration configuration pinned to a
// published behavior version.
type Definition struct {
	Steps []StepDefinition

	Retry RetryPolicy

	StepTimeout time.Duration

	LeaseDuration time.Duration

	ExecutionDeadline time.Duration // zero means no execution deadline

	WorkerCapabilities []string

	NotificationHandlers []string

	ApprovalHandlers []string
}

// StepDefinition is one registered step and its dependencies.
type StepDefinition struct {
	Key string

	Kind string

	DependsOn []string

	WorkerCapabilities []string
}

// RetryPolicy bounds total attempts and gives the delay before each retry.
type RetryPolicy struct {
	MaxAttempts int

	Delays []time.Duration
}

// ValidationError identifies an invalid definition field.
type ValidationError struct {
	Path string

	Msg string
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// ValidationErrors reports every independently detectable definition error.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	lines := make([]string, len(e))
	for i := range e {
		lines[i] = e[i].Error()
	}
	return strings.Join(lines, "\n")
}

// ValidateDefinition checks graph bounds, runtime policy, registered kinds
// and worker constraints. A nil supportedCapabilities list skips only the
// environment-dependent capability check.
func ValidateDefinition(d Definition, supportedCapabilities []string) error {
	var errs ValidationErrors
	add := func(path, msg string, args ...any) {
		errs = append(errs, ValidationError{Path: path, Msg: fmt.Sprintf(msg, args...)})
	}
	if len(d.Steps) == 0 {
		add("steps", "must have at least one step")
	} else if len(d.Steps) > MaxSteps {
		add("steps", "must have at most %d steps", MaxSteps)
	}
	keys := make(map[string]int, len(d.Steps))
	for i, step := range d.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		if strings.TrimSpace(step.Key) == "" {
			add(path+".key", "must not be empty")
		} else if _, exists := keys[step.Key]; exists {
			add(path+".key", "duplicate step key")
		} else {
			keys[step.Key] = i
		}
		if !registeredStepKinds[step.Kind] {
			add(path+".kind", "is not a registered step kind")
		}
	}
	for i, step := range d.Steps {
		seen := map[string]bool{}
		for j, dependency := range step.DependsOn {
			path := fmt.Sprintf("steps[%d].depends_on[%d]", i, j)
			if _, exists := keys[dependency]; !exists {
				add(path, "references an unknown step")
			} else if seen[dependency] {
				add(path, "duplicate dependency")
			}
			seen[dependency] = true
		}
	}
	state := make(map[string]uint8, len(keys))
	var visit func(string) bool
	visit = func(key string) bool {
		if state[key] == 1 {
			return true
		}
		if state[key] == 2 {
			return false
		}
		state[key] = 1
		for _, dependency := range d.Steps[keys[key]].DependsOn {
			if _, exists := keys[dependency]; exists && visit(dependency) {
				return true
			}
		}
		state[key] = 2
		return false
	}
	orderedKeys := make([]string, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	for _, key := range orderedKeys {
		if visit(key) {
			add("steps", "must be acyclic")
			break
		}
	}
	if d.Retry.MaxAttempts < 1 || d.Retry.MaxAttempts > 5 {
		add("retry.max_attempts", "must be between 1 and 5")
	}
	if len(d.Retry.Delays) != d.Retry.MaxAttempts-1 {
		add("retry.delays", "must contain one delay for each retry")
	}
	for i, delay := range d.Retry.Delays {
		if delay <= 0 {
			add(fmt.Sprintf("retry.delays[%d]", i), "must be positive")
		}
	}
	if d.StepTimeout <= 0 {
		add("step_timeout", "must be positive")
	}
	if d.LeaseDuration <= 0 {
		add("lease", "must be positive")
	} else if d.StepTimeout > 0 && d.LeaseDuration < d.StepTimeout+30*time.Second {
		add("lease", "must be at least 30 seconds longer than step_timeout")
	}
	if d.ExecutionDeadline < 0 {
		add("execution_deadline", "must be positive when set")
	}
	supported := map[string]bool{}
	for _, capability := range supportedCapabilities {
		supported[capability] = true
	}
	checkCapabilities := func(path string, capabilities []string) {
		seen := map[string]bool{}
		for i, capability := range capabilities {
			p := fmt.Sprintf("%s[%d]", path, i)
			if strings.TrimSpace(capability) == "" {
				add(p, "must not be empty")
			} else if seen[capability] {
				add(p, "duplicate capability")
			} else if supportedCapabilities != nil && !supported[capability] {
				add(p, "is not supported by the in-process executor")
			}
			seen[capability] = true
		}
	}
	checkCapabilities("worker_capabilities", d.WorkerCapabilities)
	for i, step := range d.Steps {
		checkCapabilities(fmt.Sprintf("steps[%d].worker_capabilities", i), step.WorkerCapabilities)
	}
	checkHandlers := func(path string, handlers []string, registered map[string]bool) {
		for i, handler := range handlers {
			if !registered[handler] {
				add(fmt.Sprintf("%s[%d]", path, i), "is not a registered handler")
			}
		}
	}
	checkHandlers("notification_handlers", d.NotificationHandlers, map[string]bool{NotificationHandlerTelegram: true})
	checkHandlers("approval_handlers", d.ApprovalHandlers, map[string]bool{ApprovalHandlerBehavior: true})
	if len(errs) > 0 {
		return errs
	}
	return nil
}
