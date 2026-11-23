package behavior

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"time"
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
		log.Print("retention: pass code=database_error")
		return
	}
	keys := make([]string, 0, len(result.Counts))
	for table, count := range result.Counts {
		keys = append(keys, table)
		if r.Metrics != nil {
			r.Metrics.RetentionDeleted(table, count)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+len(result.Issues))
	for _, table := range keys {
		parts = append(parts, table+"="+formatRetentionCount(result.Counts[table]))
	}
	for _, issue := range result.Issues {
		parts = append(parts, issue.Table+"_code="+issue.Code)
		parts = append(parts, issue.Table+"_issue_count="+formatRetentionCount(issue.Count))
	}
	log.Print("retention: pass " + strings.Join(parts, " "))
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
