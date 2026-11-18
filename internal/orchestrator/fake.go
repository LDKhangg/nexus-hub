package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// ErrFakeExhausted is returned when no scripted response is left for a key.
var ErrFakeExhausted = errors.New("orchestrator: fake runtime has no scripted response")

// FakeReply is one scripted answer: a JSON value, or an error.
type FakeReply struct {
	JSON    json.RawMessage
	Usage   FakeUsage
	CostUSD float64
	Err     error
}

// FakeUsage is the token usage a scripted reply reports.
type FakeUsage struct{ Input, Output int64 }

// FakeRuntime is a deterministic AIRuntime for tests: one FIFO queue of
// replies per key (the schema name, or a step key set with Script). It
// records every request and uses no randomness.
type FakeRuntime struct {
	mu       sync.Mutex
	queues   map[string][]FakeReply
	requests []StructuredRequest
	keyFor   func(StructuredRequest) string
}

// NewFakeRuntime returns an empty FakeRuntime keyed by SchemaName.
func NewFakeRuntime() *FakeRuntime {
	return &FakeRuntime{queues: map[string][]FakeReply{}}
}

// KeyBy overrides how a request maps to a queue key (for example by step).
func (f *FakeRuntime) KeyBy(fn func(StructuredRequest) string) *FakeRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyFor = fn
	return f
}

// Script appends replies to the queue for key.
func (f *FakeRuntime) Script(key string, replies ...FakeReply) *FakeRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[key] = append(f.queues[key], replies...)
	return f
}

// Requests returns a copy of the recorded requests, oldest first.
func (f *FakeRuntime) Requests() []StructuredRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StructuredRequest(nil), f.requests...)
}

// Remaining returns how many scripted replies are still queued for key.
func (f *FakeRuntime) Remaining(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queues[key])
}

// Generate records the request and pops the next reply for its key.
func (f *FakeRuntime) Generate(ctx context.Context, req StructuredRequest) (StructuredResponse, error) {
	if err := ctx.Err(); err != nil {
		return StructuredResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	key := req.SchemaName
	if f.keyFor != nil {
		key = f.keyFor(req)
	}
	q := f.queues[key]
	if len(q) == 0 {
		return StructuredResponse{}, ErrFakeExhausted
	}
	reply := q[0]
	f.queues[key] = q[1:]
	if reply.Err != nil {
		return StructuredResponse{}, reply.Err
	}
	resp := StructuredResponse{JSON: reply.JSON, CostUSD: reply.CostUSD}
	resp.Usage.Input, resp.Usage.Output = reply.Usage.Input, reply.Usage.Output
	return resp, nil
}
