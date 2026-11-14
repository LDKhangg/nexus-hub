package workflow

import (
	"context"
	"time"
)

// ReconcileStore repairs queue invariants in storage-owned transactions.
type ReconcileStore interface {
	ReconcileJobs(context.Context, time.Time) error
}

// Reconciler repairs expired leases and missing wakeups. It is safe to invoke
// at startup and periodically; live leases, waits and terminal executions are
// outside the storage candidate set.
type Reconciler struct {
	store ReconcileStore
}

func NewReconciler(store ReconcileStore) *Reconciler { return &Reconciler{store: store} }

func (r *Reconciler) Reconcile(ctx context.Context, at time.Time) error {
	if r == nil || r.store == nil || at.IsZero() {
		return ErrInvalidInput
	}
	return r.store.ReconcileJobs(ctx, at)
}
