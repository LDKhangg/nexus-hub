package workflow

import "testing"

// TC-K2f (REQ-11): WAITING_FOR_WORKER is a waiting state like the other two: it holds no lease, is not
// terminal or active, and moves like them.
func TestP10State_WaitingForWorkerBehavesLikeAWait(t *testing.T) {
	s := StateWaitingForWorker
	if string(s) != "WAITING_FOR_WORKER" || !s.Valid() || !s.IsWaiting() || s.IsTerminal() || s.IsActive() {
		t.Fatalf("state %q valid=%v waiting=%v terminal=%v active=%v", s, s.Valid(), s.IsWaiting(), s.IsTerminal(), s.IsActive())
	}
	for _, from := range []State{StateRunning, StateEvaluating, StatePlanning} {
		if !CanTransition(from, s) {
			t.Errorf("%s -> %s refused", from, s)
		}
	}
	for _, to := range []State{StateQueued, StateCancelled, StateFailed} {
		if !CanTransition(s, to) {
			t.Errorf("%s -> %s refused", s, to)
		}
	}
	for _, to := range []State{StateRunning, StateCompleted, StateWaitingForUser, StateReceived} {
		if CanTransition(s, to) {
			t.Errorf("%s -> %s accepted", s, to)
		}
	}
	if CanTransition(StateReceived, s) || CanTransition(StateQueued, s) || CanTransition(StateCompleted, s) {
		t.Error("a start, queue or terminal state moved into WAITING_FOR_WORKER")
	}
	// The other waits keep their meaning.
	for _, w := range []State{StateWaitingForUser, StateWaitingForApproval} {
		if !w.IsWaiting() || !w.Valid() {
			t.Errorf("%s changed", w)
		}
	}
	if State("WAITING_FOR_NOBODY").Valid() {
		t.Error("an unknown state is valid")
	}
}
