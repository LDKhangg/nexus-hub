package workflow

import "context"

// Store owns workflow transactions; the SDK-free domain package only defines
// command orchestration and validation.
type Store interface {
	StartExecution(context.Context, StartRequest) (Execution, error)
	HandleSignal(context.Context, Signal) error
	CancelExecution(context.Context, Command) (Execution, error)
	ResumeExecution(context.Context, Command) (Execution, error)
	RetryExecution(context.Context, Command) (Execution, error)
}

type engine struct{ store Store }

// NewEngine creates the stable workflow command seam.
func NewEngine(store Store) Engine { return &engine{store: store} }

func (e *engine) Start(ctx context.Context, req StartRequest) (Execution, error) {
	if e.store == nil {
		return Execution{}, ErrInvalidInput
	}
	return e.store.StartExecution(ctx, req)
}

func (e *engine) HandleSignal(ctx context.Context, signal Signal) error {
	if e.store == nil {
		return ErrInvalidInput
	}
	return e.store.HandleSignal(ctx, signal)
}

func (e *engine) Cancel(ctx context.Context, command Command) (Execution, error) {
	if e.store == nil {
		return Execution{}, ErrInvalidInput
	}
	return e.store.CancelExecution(ctx, command)
}

func (e *engine) Resume(ctx context.Context, command Command) (Execution, error) {
	if e.store == nil {
		return Execution{}, ErrInvalidInput
	}
	return e.store.ResumeExecution(ctx, command)
}

func (e *engine) Retry(ctx context.Context, command Command) (Execution, error) {
	if e.store == nil {
		return Execution{}, ErrInvalidInput
	}
	return e.store.RetryExecution(ctx, command)
}
