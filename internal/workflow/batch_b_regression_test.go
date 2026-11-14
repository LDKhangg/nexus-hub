package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// uncertainAfterTimeout blocks until the worker's timer cancels the step,
// then reports that the side effect may already have happened.
func uncertainAfterTimeout(started chan<- struct{}) workflow.StepExecutor {
	return execFunc(func(ctx context.Context, _ workflow.StepContext) (workflow.StepResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return workflow.StepResult{Usage: workflow.Usage{InputTokens: 9}}, &workflow.StepError{Code: "telegram_send_unknown", Retryable: true, Uncertain: true}
	})
}

// TC-B01 (REQ-8, R2): an executor that reports uncertainty after the step
// timer or the execution deadline fired fails with side_effect_uncertain;
// the timeout path never retries a step whose side effect may have happened.
func TestWorkerUncertainWinsOverTimeout(t *testing.T) {
	def := defaultDefinition(t)
	t.Run("TC-B01/step timeout with retries left", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		short := def
		short.StepTimeout = time.Minute
		short.LeaseDuration = 2 * time.Minute
		store := newFakeJobStore(testJob(1, 1, short))
		started := make(chan struct{}, 1)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, uncertainAfterTimeout(started))
		done := runAsync(worker)
		<-started
		clock.waitForWaiters(t, 1)
		clock.Advance(time.Minute)
		awaitResult(t, done)
		c := onlyCall(t, store, 1)
		if c.method != "fail" || c.req.Reason != workflow.ReasonSideEffectUncertain {
			t.Fatalf("uncertain after step timeout: %s %q, want fail side_effect_uncertain", c.method, c.req.Reason)
		}
		if c.req.Usage.InputTokens != 9 {
			t.Fatalf("usage lost: %#v", c.req.Usage)
		}
	})
	t.Run("TC-B01/execution deadline", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		job := testJob(2, 1, def)
		deadline := clock.Now().Add(20 * time.Second)
		job.Execution.Deadline = &deadline
		store := newFakeJobStore(job)
		started := make(chan struct{}, 1)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, uncertainAfterTimeout(started))
		done := runAsync(worker)
		<-started
		clock.waitForWaiters(t, 1)
		clock.Advance(20 * time.Second)
		awaitResult(t, done)
		c := onlyCall(t, store, 2)
		if c.method != "fail" || c.req.Reason != workflow.ReasonSideEffectUncertain {
			t.Fatalf("uncertain after deadline: %s %q, want fail side_effect_uncertain", c.method, c.req.Reason)
		}
	})
	t.Run("TC-B01/plain retryable error after timeout still retries", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		short := def
		short.StepTimeout = time.Minute
		short.LeaseDuration = 2 * time.Minute
		store := newFakeJobStore(testJob(3, 1, short))
		started := make(chan struct{}, 1)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(ctx context.Context, _ workflow.StepContext) (workflow.StepResult, error) {
			started <- struct{}{}
			<-ctx.Done()
			return workflow.StepResult{}, &workflow.StepError{Code: "provider_busy", Retryable: true}
		}))
		done := runAsync(worker)
		<-started
		clock.waitForWaiters(t, 1)
		clock.Advance(time.Minute)
		awaitResult(t, done)
		if c := onlyCall(t, store, 3); c.method != "retry" || c.req.Reason != workflow.ReasonDeadlineExceeded {
			t.Fatalf("certain timeout: %s %q, want retry deadline_exceeded", c.method, c.req.Reason)
		}
	})
}

// TC-B02 (REQ-2, R4): step keys are bounded in bytes (the DB column bound),
// not runes, so a long Vietnamese key cannot slip past validation.
func TestWorkflowStepKeyByteBound(t *testing.T) {
	base := defaultDefinition(t)
	withKey := func(key string) workflow.Definition {
		d := base
		d.Steps = append([]workflow.StepDefinition(nil), base.Steps...)
		old := d.Steps[0].Key
		d.Steps[0].Key = key
		for i := range d.Steps {
			deps := append([]string(nil), d.Steps[i].DependsOn...)
			for j := range deps {
				if deps[j] == old {
					deps[j] = key
				}
			}
			d.Steps[i].DependsOn = deps
		}
		return d
	}
	if err := workflow.ValidateDefinition(withKey(strings.Repeat("k", workflow.MaxStepKeyBytes)), nil); err != nil {
		t.Fatalf("200-byte key rejected: %v", err)
	}
	for name, key := range map[string]string{
		"TC-B02/201 ascii bytes":      strings.Repeat("k", workflow.MaxStepKeyBytes+1),
		"TC-B02/vietnamese 101 runes": strings.Repeat("ữ", 101), // 3 bytes each: 303 bytes, 101 runes
		"TC-B02/emoji":                strings.Repeat("🙂", 51),  // 204 bytes
	} {
		t.Run(name, func(t *testing.T) {
			err := workflow.ValidateDefinition(withKey(key), nil)
			var ve workflow.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("long key accepted: %v", err)
			}
			found := false
			for _, e := range ve {
				found = found || e.Path == "steps[0].key"
			}
			if !found {
				t.Fatalf("no steps[0].key error: %v", err)
			}
		})
	}
}

// TC-B03 (REQ-13 abuse): the internal agent step kinds are reserved: a user
// definition cannot use them, the internal pipeline definition is a valid
// execution definition, and mixing internal and user kinds is refused.
func TestInternalStepKindsReserved(t *testing.T) {
	user := defaultDefinition(t)
	user.Steps = append(append([]workflow.StepDefinition(nil), user.Steps...),
		workflow.StepDefinition{Key: "tell", Kind: workflow.StepKindNotify, DependsOn: []string{user.Steps[0].Key}})
	if err := workflow.ValidateDefinition(user, nil); err != nil {
		t.Fatalf("base user definition invalid: %v", err)
	}
	pipeline := agent.PipelineDefinition()
	if err := workflow.ValidateExecutionDefinition(pipeline); err != nil {
		t.Fatalf("internal pipeline definition rejected: %v", err)
	}
	if err := workflow.ValidateDefinition(pipeline, nil); err == nil {
		t.Fatal("user validation accepted the internal pipeline definition")
	}
	for _, kind := range []string{workflow.StepKindAgentTriage, workflow.StepKindAgentInvestigate, workflow.StepKindAgentReport} {
		d := user
		d.Steps = append([]workflow.StepDefinition(nil), user.Steps...)
		d.Steps[0].Kind = kind
		if err := workflow.ValidateDefinition(d, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("user definition with %s: %v", kind, err)
		}
		// One internal kind turns validation internal: the user kinds left
		// in the definition must then be refused.
		if err := workflow.ValidateExecutionDefinition(d); err == nil {
			t.Fatalf("mixed internal/user definition with %s accepted", kind)
		}
	}
	// The pipeline keeps the legacy attempt budget and an attempt deadline.
	if pipeline.Retry.MaxAttempts != 5 || pipeline.StepTimeout != 30*time.Minute {
		t.Fatalf("pipeline budget: %+v", pipeline)
	}
}
