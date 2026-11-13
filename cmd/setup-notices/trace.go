package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const traceUsage = `usage: trace --workspace-id <uuid> <run id | task id/URL | clickup:chat:<message id> | correlation id> [--item <key>] [--json]
  shows a behavior run: behavior and version, trigger, subject, revision,
  status, times, correlation ID, usage, run-level action outcomes (pull
  requests, review IDs, reaction), items and their action results;
  with --item, that item in full; --json prints the trace as JSON`

const runsUsage = `usage: runs --workspace-id <uuid> [--behavior <name>] [--limit N]
  lists recent behavior runs, newest first (N: 1-200, default 20); a
  chat-triggered run also shows its pull requests and run-level outcomes`

func traceLocation(ctx context.Context, store *storage.BehaviorStore, ws string) *time.Location {
	tz := behavior.DefaultProfile().Timezone
	if p, err := store.GetProfile(ctx, ws); err == nil && p.Timezone != "" {
		tz = p.Timezone
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

func traceCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	f := commandFlags("trace")
	ws := f.String("workspace-id", "", "workspace UUID")
	item := f.String("item", "", "item key")
	asJSON := f.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(f, args)
	if err != nil || !tenant.ValidUUID(*ws) || len(pos) != 1 || len(*item) > 200 {
		return errors.New(traceUsage)
	}
	store, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return err
	}
	t, err := behavior.BuildTrace(ctx, store, *ws, pos[0], *item)
	switch {
	case errors.Is(err, behavior.ErrInvalid):
		return errors.New(traceUsage)
	case errors.Is(err, behavior.ErrNotFound) && *item != "" && t.Run.ID != 0:
		return errors.New("the run has no item with that key")
	case errors.Is(err, behavior.ErrNotFound):
		return errors.New("run not found")
	case err != nil:
		return errors.New("database operation failed")
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(t)
	}
	_, err = io.WriteString(out, behavior.RenderTraceFull(t, traceLocation(ctx, store, *ws), *item != ""))
	return err
}

func runsCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	f := commandFlags("runs")
	ws := f.String("workspace-id", "", "workspace UUID")
	name := f.String("behavior", "", "behavior name")
	limit := f.Int("limit", 20, "number of runs")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New(runsUsage)
	}
	if *name != "" && !behavior.NameRe.MatchString(*name) {
		return errors.New("invalid behavior name")
	}
	if *limit < 1 || *limit > behavior.MaxRunListLimit {
		return errors.New(runsUsage)
	}
	store, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return err
	}
	list, err := store.ListRecentRuns(ctx, *ws, *name, *limit)
	if err != nil {
		return errors.New("database operation failed")
	}
	enc := json.NewEncoder(out)
	for _, r := range list {
		line := map[string]any{
			"id": r.ID, "behavior": r.Behavior, "version": r.Version, "behavior_version": r.Behavior + "@" + strconv.Itoa(r.Version),
			"trigger": r.TriggerKind, "subject": r.Subject, "status": r.Status, "error_code": r.ErrorCode,
			"correlation_id": r.CorrelationID,
			"items":          map[string]int{"auto": r.AutoItems, "notify": r.NotifyItems, "ask": r.AskItems},
			"created_at":     r.CreatedAt.UTC().Format(time.RFC3339),
		}
		// Chat-triggered runs: their pull requests and run-level outcomes
		// (identifiers and codes only; the message text is never stored).
		if len(r.PRs) > 0 {
			line["prs"] = r.PRs
		}
		if len(r.RunActions) > 0 {
			line["run_actions"] = r.RunActions
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return nil
}
