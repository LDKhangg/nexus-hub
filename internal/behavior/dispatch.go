package behavior

import (
	"context"
	"log"
)

// RunContext is everything the engine knows about a run once its pinned
// version is resolved. It is handed to the Dispatcher and the Notifier.
type RunContext struct {
	Run Run
	// Doc is the run's pinned version with the profile defaults applied
	// (model, language, tone and budgets); behavior fields win.
	Doc     ResolvedDoc
	Profile Profile
	Trigger TriggerData
	// Context is what the run read (empty when it failed before reading).
	Context ContextSnapshot
}

// Dispatcher hands a run's classified items to the action library. Items
// arrive with status drafted, class and rule set. Dispatch may be called
// again for the same run after a crash or an error (the run is retried
// with the items already stored), so an implementation must be idempotent
// per (run, item, action). An error retries the run until its attempts are
// spent, then the run fails with dispatch_failed.
type Dispatcher interface {
	Dispatch(ctx context.Context, rc RunContext, items []Item) error
}

// Notifier tells the owner that a run failed. code is a short engine code
// such as invalid_output or token_budget; never content.
type Notifier interface {
	RunFailed(ctx context.Context, rc RunContext, code string) error
}

// NopDispatcher leaves items as they are (status drafted). It keeps the
// binary working until the action library is wired.
type NopDispatcher struct{}

func (NopDispatcher) Dispatch(context.Context, RunContext, []Item) error { return nil }

// LogNotifier logs the failure code with the run and correlation IDs.
type LogNotifier struct{}

func (LogNotifier) RunFailed(_ context.Context, rc RunContext, code string) error {
	log.Printf("behavior run %d corr=%s: failed (%s); no notifier wired", rc.Run.ID, rc.Run.CorrelationID, code)
	return nil
}
