package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const memoryUsage = `usage: memory <command> --workspace-id <uuid> [args]
  list [--behavior <name>] [--all] [--limit N]   remembered decisions, newest first
                                                 (--all includes forgotten; N: 1-100, default 50)
  show --id N                                    one decision in full
  forget --id N                                  hide a decision from memory_search (kept for audit)
Decisions recorded from an undone auto write ("auto_undone") are listed but
never returned by memory_search, and neither are other decisions about the
same item.`

func decisionJSON(d behavior.RememberedDecision, full bool) map[string]any {
	q, a := d.Question, d.Answer
	if !full {
		q, a = oneLineCap(q, 160), oneLineCap(a, 160)
	}
	out := map[string]any{
		"id": d.ID, "locator": d.Locator(), "topic": d.Topic, "question": q, "answer": a, "scope": d.Scope,
		"source": d.Source, "created_at": d.CreatedAt.UTC().Format(time.RFC3339), "forgotten": d.ForgottenAt != nil,
	}
	if full {
		out["run_id"], out["item_id"] = d.RunID, d.ItemID
		if d.ForgottenAt != nil {
			out["forgotten_at"] = d.ForgottenAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func oneLineCap(s string, n int) string {
	r := []rune(s)
	for i, c := range r {
		if c == '\n' || c == '\r' || c == '\t' {
			r[i] = ' '
		}
	}
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func memoryCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(memoryUsage)
	}
	sub := args[0]
	f := commandFlags("memory " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	id := f.Int64("id", 0, "decision ID")
	name := f.String("behavior", "", "behavior name")
	all := f.Bool("all", false, "include forgotten")
	limit := f.Int("limit", 50, "number of decisions")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New(memoryUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	switch sub {
	case "list":
		if set["id"] {
			return errors.New(memoryUsage)
		}
		if *name != "" && !behavior.NameRe.MatchString(*name) {
			return errors.New("invalid behavior name")
		}
		if *limit < 1 || *limit > 100 {
			return errors.New(memoryUsage)
		}
	case "show", "forget":
		if *id <= 0 || set["behavior"] || set["all"] || set["limit"] {
			return errors.New(memoryUsage)
		}
	default:
		return errors.New(memoryUsage)
	}
	store, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	switch sub {
	case "list":
		list, err := store.ListDecisionsFor(ctx, *ws, *name, *all, *limit, 0)
		if err != nil {
			return errors.New("database operation failed")
		}
		for _, d := range list {
			if err := enc.Encode(decisionJSON(d, false)); err != nil {
				return err
			}
		}
		return nil
	case "show":
		d, err := store.GetDecision(ctx, *ws, *id)
		if errors.Is(err, behavior.ErrNotFound) {
			return errors.New("decision not found")
		}
		if err != nil {
			return errors.New("database operation failed")
		}
		return enc.Encode(decisionJSON(d, true))
	default: // forget
		err := store.ForgetDecision(ctx, *ws, *id)
		if errors.Is(err, behavior.ErrNotFound) {
			return errors.New("decision not found or already forgotten")
		}
		if err != nil {
			return errors.New("database operation failed")
		}
		return enc.Encode(map[string]any{"id": *id, "forgotten": true})
	}
}
