package behavior

import (
	"context"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/obs"
)

const DefaultRetentionBatch int32 = 500

// RetentionRules are the configured maximum ages. Zero keeps that category forever.
type RetentionRules struct {
	MessageLogsDays     int64
	ToolRunsDays        int64
	PolicyDecisionsDays int64
	WorkflowDays        int64
	OutboundDays        int64
}

// RetentionIssue reports a database constraint without exposing row content.
type RetentionIssue struct {
	Table string
	Code  string
	Count int64
}

// RetentionPartialReason identifies the bound that stopped a dry run.
type RetentionPartialReason string

const (
	RetentionPartialCap      RetentionPartialReason = "cap"
	RetentionPartialDeadline RetentionPartialReason = "deadline"
)

// RetentionResult contains table counts and recoverable constraint failures.
type RetentionResult struct {
	Counts        map[string]int64
	Issues        []RetentionIssue
	Partial       bool
	PartialReason RetentionPartialReason
}

// RetentionStore counts and deletes configured old data.
type RetentionStore interface {
	CountRetention(context.Context, string, RetentionRules, time.Time) (map[string]int64, error)
	SweepRetention(context.Context, string, RetentionRules, time.Time, int32) (RetentionResult, error)
}

// RetentionMetrics records deleted rows with a closed table label.
type RetentionMetrics interface {
	RetentionDeleted(string, int64)
}

// Retention runs one pass immediately and then hourly until cancellation.
type Retention struct {
	Store     RetentionStore
	Metrics   RetentionMetrics
	Enabled   bool
	Rules     RetentionRules
	Interval  time.Duration
	BatchSize int32
	Now       func() time.Time
}

// Run implements the server service lifecycle.
func (r *Retention) Run(ctx context.Context) error {
	if r.Store == nil {
		return errors.New("retention requires store")
	}
	interval := r.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	batch := r.BatchSize
	if batch <= 0 {
		batch = 500
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	if !r.Enabled {
		<-ctx.Done()
		return nil
	}
	r.runPass(ctx, now().UTC(), batch)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case at := <-ticker.C:
			r.runPass(ctx, at.UTC(), batch)
		}
	}
}

func (r *Retention) runPass(ctx context.Context, now time.Time, batch int32) {
	result, err := r.Store.SweepRetention(ctx, "", r.Rules, now, batch)
	if err != nil {
		obs.Logger(ctx).Warn("retention pass failed", "error_code", "database_error")
		return
	}
	for table, count := range result.Counts {
		if r.Metrics != nil {
			r.Metrics.RetentionDeleted(table, count)
		}
	}
	obs.Logger(ctx).Info("retention pass completed", "counts", result.Counts, "issues", result.Issues, "partial", result.Partial, "partial_reason", result.PartialReason)
}

func formatRetentionCount(n int64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
