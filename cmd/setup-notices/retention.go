package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

func retentionCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: retention <show|run> [--workspace-id UUID] [--dry-run]")
	}
	fs := commandFlags("retention " + args[0])
	workspaceID := fs.String("workspace-id", "", "limit retention to one workspace")
	dryRun := fs.Bool("dry-run", false, "simulate deletions; exact only below five batches per group")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return errors.New("invalid retention arguments")
	}
	if args[0] != "show" && args[0] != "run" {
		return errors.New("usage: retention <show|run> [--workspace-id UUID] [--dry-run]")
	}
	if args[0] == "show" && *dryRun {
		return errors.New("--dry-run is only valid with retention run")
	}
	if err := validateRetentionWorkspace(ctx, pool, *workspaceID); err != nil {
		return err
	}
	settingsStore, err := storage.NewSettingsStore(pool)
	if err != nil {
		return errors.New("database operation failed")
	}
	configured, err := settings.Load(ctx, settingsStore)
	if err != nil {
		return errors.New("database operation failed")
	}
	rules := retentionRules(configured)
	store, err := storage.NewRetentionStore(pool)
	if err != nil {
		return errors.New("database operation failed")
	}
	now := time.Now().UTC()
	if args[0] == "show" {
		counts, err := store.CountRetention(ctx, *workspaceID, rules, now)
		if err != nil {
			return errors.New("database operation failed")
		}
		fmt.Fprintf(out, "enabled=%t\n", configured.RetentionEnabled)
		fmt.Fprintf(out, "message_logs_days=%d\n", rules.MessageLogsDays)
		fmt.Fprintf(out, "tool_runs_days=%d\n", rules.ToolRunsDays)
		fmt.Fprintf(out, "policy_decisions_days=%d\n", rules.PolicyDecisionsDays)
		fmt.Fprintf(out, "workflow_days=%d\n", rules.WorkflowDays)
		fmt.Fprintf(out, "outbound_days=%d\n", rules.OutboundDays)
		fmt.Fprintln(out, "candidates now (dry-run is exact only below five batches per group)")
		writeRetentionCounts(out, counts)
		return nil
	}
	if *dryRun {
		result, err := store.SweepRetentionDryRun(ctx, *workspaceID, rules, now, 500)
		if err != nil {
			if errors.Is(err, storage.ErrRetentionBusy) {
				return errors.New("busy, try again")
			}
			return errors.New("database operation failed")
		}
		writeRetentionResult(out, result)
		return nil
	}
	result, err := store.SweepRetention(ctx, *workspaceID, rules, now, 500)
	if err != nil {
		return errors.New("database operation failed")
	}
	writeRetentionResult(out, result)
	return nil
}

func writeRetentionResult(out io.Writer, result behavior.RetentionResult) {
	writeRetentionCounts(out, result.Counts)
	if result.Partial {
		fmt.Fprintln(out, "status=partial (cap reached)")
	}
	for _, issue := range result.Issues {
		fmt.Fprintf(out, "%s_code=%s\n", issue.Table, issue.Code)
		fmt.Fprintf(out, "%s_issue_count=%d\n", issue.Table, issue.Count)
	}
}

func validateRetentionWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID string) error {
	if workspaceID == "" {
		return nil
	}
	if !tenant.ValidUUID(workspaceID) {
		return errors.New("workspace-id must be a UUID")
	}
	store, err := storage.NewWorkflowStore(pool)
	if err != nil {
		return errors.New("database operation failed")
	}
	if err := store.CheckWorkspace(ctx, workspaceID); err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return errors.New("workspace not found")
		}
		return errors.New("database operation failed")
	}
	return nil
}

func retentionRules(configured settings.Settings) behavior.RetentionRules {
	return behavior.RetentionRules{
		MessageLogsDays: configured.RetentionMessageLogsDays, ToolRunsDays: configured.RetentionToolRunsDays,
		PolicyDecisionsDays: configured.RetentionPolicyDecisionsDays, WorkflowDays: configured.RetentionWorkflowDays,
		OutboundDays: configured.RetentionOutboundDays,
	}
}

func writeRetentionCounts(out io.Writer, counts map[string]int64) {
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		fmt.Fprintf(out, "%s=%d\n", table, counts[table])
	}
}
