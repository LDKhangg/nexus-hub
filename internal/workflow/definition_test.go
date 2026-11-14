package workflow_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

func validDefinition() workflow.Definition {
	return workflow.Definition{
		Steps: []workflow.StepDefinition{
			{Key: "collect", Kind: workflow.StepKindCollectContext},
			{Key: "model", Kind: workflow.StepKindModel, DependsOn: []string{"collect"}},
			{Key: "actions", Kind: workflow.StepKindActions, DependsOn: []string{"model"}},
		},
		Retry:         workflow.RetryPolicy{MaxAttempts: 5, Delays: []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}},
		StepTimeout:   10 * time.Minute,
		LeaseDuration: 15 * time.Minute,
	}
}

// validationPaths returns every field path of a ValidationErrors result.
func validationPaths(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	var ve workflow.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("error is not ValidationErrors: %T %v", err, err)
	}
	out := make([]string, len(ve))
	for i := range ve {
		out[i] = ve[i].Path
	}
	return out
}

func requirePath(t *testing.T, err error, path string) {
	t.Helper()
	paths := validationPaths(t, err)
	for _, p := range paths {
		if p == path {
			return
		}
	}
	t.Fatalf("want field error at %q, got paths %v (err=%v)", path, paths, err)
}

func TestWorkflowDefinitionAcceptsValidGraph(t *testing.T) {
	// TC-4/TC-5 baseline (REQ-2): the reference definition is valid, with
	// and without the in-process capability registry.
	if err := workflow.ValidateDefinition(validDefinition(), nil); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	if err := workflow.ValidateDefinition(validDefinition(), []string{workflow.CapabilityInProcess}); err != nil {
		t.Fatalf("valid definition rejected with registry: %v", err)
	}
}

// TC-4 (REQ-2): invalid graphs are rejected at the exact field, never panic.
func TestWorkflowDefinitionRejectsInvalidGraph(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*workflow.Definition)
		path   string
	}{
		{"TC-4/cycle", func(d *workflow.Definition) { d.Steps[0].DependsOn = []string{"actions"} }, "steps"},
		{"TC-4/self dependency", func(d *workflow.Definition) { d.Steps[0].DependsOn = []string{"collect"} }, "steps"},
		{"TC-4/missing dependency", func(d *workflow.Definition) { d.Steps[1].DependsOn = []string{"nope"} }, "steps[1].depends_on[0]"},
		{"TC-4/65 steps", func(d *workflow.Definition) {
			d.Steps = nil
			for i := 0; i < workflow.MaxSteps+1; i++ {
				d.Steps = append(d.Steps, workflow.StepDefinition{Key: fmt.Sprintf("s%d", i), Kind: workflow.StepKindNotify})
			}
		}, "steps"},
		{"TC-4/no steps", func(d *workflow.Definition) { d.Steps = nil }, "steps"},
		{"TC-4/unknown kind", func(d *workflow.Definition) { d.Steps[2].Kind = "shell.exec" }, "steps[2].kind"},
		{"TC-4/empty kind", func(d *workflow.Definition) { d.Steps[2].Kind = "" }, "steps[2].kind"},
		{"TC-4/duplicate key", func(d *workflow.Definition) { d.Steps[2].Key = "model" }, "steps[2].key"},
		{"TC-4/empty key", func(d *workflow.Definition) { d.Steps[0].Key = "   " }, "steps[0].key"},
		{"TC-4/duplicate dependency", func(d *workflow.Definition) { d.Steps[2].DependsOn = []string{"model", "model"} }, "steps[2].depends_on[1]"},
		{"TC-4/empty dependency", func(d *workflow.Definition) { d.Steps[2].DependsOn = []string{""} }, "steps[2].depends_on[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDefinition()
			tc.mutate(&d)
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ValidateDefinition panicked: %v", r)
					}
				}()
				err = workflow.ValidateDefinition(d, nil)
			}()
			if err == nil {
				t.Fatal("invalid graph accepted")
			}
			requirePath(t, err, tc.path)
		})
	}

	t.Run("TC-4/exactly 64 steps is accepted", func(t *testing.T) {
		d := validDefinition()
		d.Steps = nil
		for i := 0; i < workflow.MaxSteps; i++ {
			step := workflow.StepDefinition{Key: fmt.Sprintf("s%d", i), Kind: workflow.StepKindNotify}
			if i > 0 {
				step.DependsOn = []string{fmt.Sprintf("s%d", i-1)}
			}
			d.Steps = append(d.Steps, step)
		}
		if err := workflow.ValidateDefinition(d, nil); err != nil {
			t.Fatalf("64-step chain rejected: %v", err)
		}
	})

	t.Run("TC-4/long cycle through many nodes is detected", func(t *testing.T) {
		d := validDefinition()
		d.Steps = nil
		for i := 0; i < 20; i++ {
			d.Steps = append(d.Steps, workflow.StepDefinition{Key: fmt.Sprintf("s%d", i), Kind: workflow.StepKindNotify,
				DependsOn: []string{fmt.Sprintf("s%d", (i+1)%20)}})
		}
		requirePath(t, workflow.ValidateDefinition(d, nil), "steps")
	})

	t.Run("TC-4/every independent error is reported", func(t *testing.T) {
		d := validDefinition()
		d.Steps[1].Kind = "bogus"
		d.Steps[2].DependsOn = []string{"ghost"}
		err := workflow.ValidateDefinition(d, nil)
		requirePath(t, err, "steps[1].kind")
		requirePath(t, err, "steps[2].depends_on[0]")
		if strings.Contains(err.Error(), "panic") {
			t.Fatalf("unexpected error text: %v", err)
		}
	})
}

// TC-5 (REQ-2): unsupported capabilities, unregistered handlers and bad
// runtime policy are rejected before publish.
func TestWorkflowConstraintsRejectUnsupportedCapability(t *testing.T) {
	supported := []string{workflow.CapabilityInProcess}
	cases := []struct {
		name   string
		mutate func(*workflow.Definition)
		path   string
	}{
		{"TC-5/unsupported workflow capability", func(d *workflow.Definition) { d.WorkerCapabilities = []string{"gpu"} }, "worker_capabilities[0]"},
		{"TC-5/unsupported step capability", func(d *workflow.Definition) {
			d.Steps[1].WorkerCapabilities = []string{workflow.CapabilityInProcess, "remote_shell"}
		}, "steps[1].worker_capabilities[1]"},
		{"TC-5/empty capability", func(d *workflow.Definition) { d.WorkerCapabilities = []string{""} }, "worker_capabilities[0]"},
		{"TC-5/duplicate capability", func(d *workflow.Definition) {
			d.WorkerCapabilities = []string{workflow.CapabilityInProcess, workflow.CapabilityInProcess}
		}, "worker_capabilities[1]"},
		{"TC-5/unregistered notification handler", func(d *workflow.Definition) { d.NotificationHandlers = []string{"slack.message"} }, "notification_handlers[0]"},
		{"TC-5/approval handler used as notification", func(d *workflow.Definition) { d.NotificationHandlers = []string{workflow.ApprovalHandlerBehavior} }, "notification_handlers[0]"},
		{"TC-5/unregistered approval handler", func(d *workflow.Definition) { d.ApprovalHandlers = []string{"http.webhook"} }, "approval_handlers[0]"},
		{"TC-5/notification handler used as approval", func(d *workflow.Definition) { d.ApprovalHandlers = []string{workflow.NotificationHandlerTelegram} }, "approval_handlers[0]"},
		{"TC-5/zero step timeout", func(d *workflow.Definition) { d.StepTimeout = 0 }, "step_timeout"},
		{"TC-5/negative step timeout", func(d *workflow.Definition) { d.StepTimeout = -time.Second }, "step_timeout"},
		{"TC-5/zero lease", func(d *workflow.Definition) { d.LeaseDuration = 0 }, "lease"},
		{"TC-5/lease shorter than timeout+30s", func(d *workflow.Definition) { d.LeaseDuration = d.StepTimeout + 29*time.Second }, "lease"},
		{"TC-5/lease shorter than timeout", func(d *workflow.Definition) { d.LeaseDuration = time.Minute }, "lease"},
		{"TC-5/attempts zero", func(d *workflow.Definition) { d.Retry = workflow.RetryPolicy{MaxAttempts: 0} }, "retry.max_attempts"},
		{"TC-5/attempts six", func(d *workflow.Definition) {
			d.Retry = workflow.RetryPolicy{MaxAttempts: 6, Delays: []time.Duration{1, 1, 1, 1, 1}}
		}, "retry.max_attempts"},
		{"TC-5/negative attempts", func(d *workflow.Definition) { d.Retry = workflow.RetryPolicy{MaxAttempts: -1} }, "retry.max_attempts"},
		{"TC-5/too few delays", func(d *workflow.Definition) { d.Retry.Delays = d.Retry.Delays[:3] }, "retry.delays"},
		{"TC-5/too many delays", func(d *workflow.Definition) { d.Retry.Delays = append(d.Retry.Delays, time.Second) }, "retry.delays"},
		{"TC-5/zero delay", func(d *workflow.Definition) { d.Retry.Delays[2] = 0 }, "retry.delays[2]"},
		{"TC-5/negative delay", func(d *workflow.Definition) { d.Retry.Delays[0] = -5 * time.Second }, "retry.delays[0]"},
		{"TC-5/negative execution deadline", func(d *workflow.Definition) { d.ExecutionDeadline = -time.Second }, "execution_deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDefinition()
			tc.mutate(&d)
			err := workflow.ValidateDefinition(d, supported)
			if err == nil {
				t.Fatal("invalid constraint accepted")
			}
			requirePath(t, err, tc.path)
		})
	}

	t.Run("TC-5/only registered names are accepted", func(t *testing.T) {
		d := validDefinition()
		d.WorkerCapabilities = []string{workflow.CapabilityInProcess}
		d.Steps[0].WorkerCapabilities = []string{workflow.CapabilityInProcess}
		d.NotificationHandlers = []string{workflow.NotificationHandlerTelegram}
		d.ApprovalHandlers = []string{workflow.ApprovalHandlerBehavior}
		d.LeaseDuration = d.StepTimeout + 30*time.Second
		d.ExecutionDeadline = time.Hour
		if err := workflow.ValidateDefinition(d, supported); err != nil {
			t.Fatalf("registered constraints rejected: %v", err)
		}
		if workflow.CapabilityInProcess != "in_process" || workflow.NotificationHandlerTelegram != "telegram.message" ||
			workflow.ApprovalHandlerBehavior != "behavior.action" {
			t.Fatal("registered names changed")
		}
	})

	t.Run("TC-5/single attempt needs no delays", func(t *testing.T) {
		d := validDefinition()
		d.Retry = workflow.RetryPolicy{MaxAttempts: 1}
		if err := workflow.ValidateDefinition(d, supported); err != nil {
			t.Fatalf("one attempt rejected: %v", err)
		}
	})

	t.Run("TC-5/nil registry skips only the capability check", func(t *testing.T) {
		d := validDefinition()
		d.WorkerCapabilities = []string{"gpu"}
		if err := workflow.ValidateDefinition(d, nil); err != nil {
			t.Fatalf("nil registry should skip capability support: %v", err)
		}
		d.NotificationHandlers = []string{"slack.message"}
		requirePath(t, workflow.ValidateDefinition(d, nil), "notification_handlers[0]")
		d.NotificationHandlers = nil
		requirePath(t, workflow.ValidateDefinition(d, []string{}), "worker_capabilities[0]")
	})
}
