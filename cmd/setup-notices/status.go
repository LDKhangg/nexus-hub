package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const statusUsage = "usage: status [--workspace-id <uuid>]   (hub-wide without --workspace-id)"

// statusStates is the report order of execution states.
var statusStates = []workflow.State{
	workflow.StateReceived, workflow.StateTriaging, workflow.StatePlanning, workflow.StateQueued, workflow.StateRunning,
	workflow.StateEvaluating, workflow.StateWaitingForUser, workflow.StateWaitingForApproval, workflow.StateWaitingForWorker,
	workflow.StateCompleted, workflow.StateFailed, workflow.StateCancelled,
}

// statusCommand prints the /metrics numbers as text (Phase 12 REQ-9):
// executions by state, queue depth and oldest due age, worker devices and
// the newest heartbeat age, open workflow alerts (a count), and today's
// LLM tokens and estimated cost.
// Counts, states, ages and codes only; never secrets or content.
func statusCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	f := commandFlags("status")
	ws := f.String("workspace-id", "", "workspace UUID")
	if f.Parse(args) != nil || f.NArg() != 0 || (*ws != "" && !tenant.ValidUUID(*ws)) {
		return errors.New(statusUsage)
	}
	store, err := storage.NewOpsStore(pool)
	if err != nil {
		return errors.New("database operation failed")
	}
	if *ws != "" {
		wf, err := storage.NewWorkflowStore(pool)
		if err != nil {
			return errors.New("database operation failed")
		}
		if err := wf.CheckWorkspace(ctx, *ws); err != nil {
			if errors.Is(err, workflow.ErrNotFound) {
				return errors.New("workspace not found")
			}
			return errors.New("database operation failed")
		}
	}
	now := time.Now().UTC()
	snap, err := store.Snapshot(ctx, *ws, now)
	if err != nil {
		return errors.New("database operation failed")
	}
	openAlerts, err := store.OpenAlertCount(ctx, *ws, now.Add(-behavior.AlertFailedLookback))
	if err != nil {
		return errors.New("database operation failed")
	}
	var budget float64
	if ss, err := storage.NewSettingsStore(pool); err == nil {
		if s, err := settings.Load(ctx, ss); err == nil {
			budget = s.LLMDailyBudgetUSD
		}
	}
	scope := "hub"
	if *ws != "" {
		scope = "workspace " + *ws
	}
	p := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	p("scope: %s", scope)
	p("time: %s", now.Format(time.RFC3339))
	p("executions:")
	known := map[string]bool{}
	for _, st := range statusStates {
		known[string(st)] = true
		p("  %-22s %d", string(st), snap.ExecutionsByState[string(st)])
	}
	var unknown int64
	for st, n := range snap.ExecutionsByState {
		if !known[st] {
			unknown += n
		}
	}
	if unknown > 0 {
		p("  %-22s %d", "UNKNOWN", unknown)
	}
	p("queue:")
	p("  pending_jobs           %d", snap.QueuePending)
	p("  oldest_due_age_s       %d", int64(snap.OldestDueAge/time.Second))
	p("workers:")
	p("  devices                %d", snap.WorkerDevices)
	p("  online                 %d", snap.WorkersOnline)
	if snap.NewestHeartbeatAge < 0 {
		p("  newest_heartbeat_age_s none")
	} else {
		p("  newest_heartbeat_age_s %d", int64(snap.NewestHeartbeatAge/time.Second))
	}
	p("alerts:")
	p("  open                   %d", openAlerts)
	p("llm today (%s, UTC; cost is an estimate):", snap.Day.Format("2006-01-02"))
	usage := append([]storage.LLMUsage(nil), snap.LLMUsage...)
	sort.Slice(usage, func(i, j int) bool {
		if usage[i].Provider != usage[j].Provider {
			return usage[i].Provider < usage[j].Provider
		}
		return usage[i].Model < usage[j].Model
	})
	var total float64
	if len(usage) == 0 {
		p("  none")
	}
	for _, u := range usage {
		total += u.CostUSD
		p("  %s %s input_tokens=%d output_tokens=%d cost_usd=%s", statusLabel(u.Provider), statusLabel(u.Model), u.InputTokens, u.OutputTokens, usd(u.CostUSD))
	}
	p("  total_cost_usd=%s", usd(total))
	if budget > 0 {
		p("  daily_budget_usd=%s (per workspace)", usd(budget))
	} else {
		p("  daily_budget_usd=off")
	}
	return nil
}

func usd(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// statusLabel passes a provider or model id of a safe shape (the ledger
// accepts only that shape) and replaces anything else.
func statusLabel(s string) string {
	if s == "" || len(s) > 128 {
		return "other"
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == ':' || r == '/' || r == '#' || r == '@'
		if !ok {
			return "other"
		}
	}
	return s
}
