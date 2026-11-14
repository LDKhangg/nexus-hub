package workflow_test

import (
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

var allStates = []workflow.State{
	workflow.StateReceived, workflow.StateTriaging, workflow.StatePlanning, workflow.StateQueued,
	workflow.StateRunning, workflow.StateEvaluating, workflow.StateWaitingForUser,
	workflow.StateWaitingForApproval, workflow.StateCompleted, workflow.StateFailed, workflow.StateCancelled,
}

// legalEdge is the plan §3.3 matrix written independently of CanTransition.
func legalEdge(from, to workflow.State) bool {
	if from == to {
		return true // idempotent no-op, never history
	}
	active := map[workflow.State]bool{workflow.StateTriaging: true, workflow.StatePlanning: true, workflow.StateRunning: true, workflow.StateEvaluating: true}
	waiting := map[workflow.State]bool{workflow.StateWaitingForUser: true, workflow.StateWaitingForApproval: true}
	terminal := map[workflow.State]bool{workflow.StateCompleted: true, workflow.StateFailed: true, workflow.StateCancelled: true}
	switch {
	case terminal[from]:
		return false
	case from == workflow.StateReceived:
		return to == workflow.StateQueued || to == workflow.StateFailed || to == workflow.StateCancelled
	case from == workflow.StateQueued:
		return active[to] || to == workflow.StateFailed || to == workflow.StateCancelled
	case active[from]:
		return to == workflow.StateQueued || waiting[to] || terminal[to]
	case waiting[from]:
		return to == workflow.StateQueued || to == workflow.StateFailed || to == workflow.StateCancelled
	}
	return false
}

// TC-10 (REQ-4): every one of the 121 state pairs follows the matrix.
func TestExecutionTransitionMatrix(t *testing.T) {
	if len(allStates) != 11 {
		t.Fatalf("vocabulary size %d", len(allStates))
	}
	for _, from := range allStates {
		for _, to := range allStates {
			want := legalEdge(from, to)
			if got := workflow.CanTransition(from, to); got != want {
				t.Errorf("TC-10: CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}

	t.Run("TC-10/terminal states never become active again", func(t *testing.T) {
		for _, from := range []workflow.State{workflow.StateCompleted, workflow.StateFailed, workflow.StateCancelled} {
			if !from.IsTerminal() || from.IsActive() || from.IsWaiting() {
				t.Errorf("%s classification wrong", from)
			}
			for _, to := range allStates {
				if to != from && workflow.CanTransition(from, to) {
					t.Errorf("terminal exit %s -> %s allowed", from, to)
				}
			}
		}
		if workflow.CanTransition(workflow.StateCompleted, workflow.StateRunning) {
			t.Error("COMPLETED -> RUNNING allowed")
		}
	})

	t.Run("TC-10/unknown states are rejected", func(t *testing.T) {
		for _, bad := range []workflow.State{"", "running", "DONE", "COMPLETED "} {
			if bad.Valid() {
				t.Errorf("%q is valid", bad)
			}
			if workflow.CanTransition(bad, workflow.StateQueued) || workflow.CanTransition(workflow.StateQueued, bad) || workflow.CanTransition(bad, bad) {
				t.Errorf("transition with unknown state %q allowed", bad)
			}
		}
	})

	t.Run("TC-10/state classifications", func(t *testing.T) {
		for _, s := range allStates {
			if !s.Valid() {
				t.Errorf("%s not valid", s)
			}
			n := 0
			for _, b := range []bool{s.IsActive(), s.IsWaiting(), s.IsTerminal()} {
				if b {
					n++
				}
			}
			if n > 1 {
				t.Errorf("%s has overlapping classifications", s)
			}
		}
	})
}
