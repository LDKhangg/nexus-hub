package workflow

// IsTerminal reports whether an execution can no longer change state.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// IsActive reports whether work may currently be executing.
func (s State) IsActive() bool {
	switch s {
	case StateTriaging, StatePlanning, StateRunning, StateEvaluating:
		return true
	default:
		return false
	}
}

// IsWaiting reports whether an execution is durably blocked without a lease.
func (s State) IsWaiting() bool {
	return s == StateWaitingForUser || s == StateWaitingForApproval
}

// Valid reports whether s is in the complete persisted vocabulary.
func (s State) Valid() bool {
	switch s {
	case StateReceived, StateTriaging, StatePlanning, StateQueued, StateRunning,
		StateEvaluating, StateWaitingForUser, StateWaitingForApproval,
		StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// CanTransition implements the complete execution transition matrix. Equal
// states are accepted as idempotent no-ops and must not append history.
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	if from.IsTerminal() {
		return false
	}
	if to == StateCancelled || to == StateFailed {
		return true
	}
	switch {
	case from == StateReceived:
		return to == StateQueued
	case from == StateQueued:
		return to.IsActive()
	case from.IsActive():
		return to == StateQueued || to.IsWaiting() || to == StateCompleted
	case from.IsWaiting():
		return to == StateQueued
	default:
		return false
	}
}
