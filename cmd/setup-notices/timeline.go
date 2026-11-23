package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const executionUsage = "usage: execution timeline <WF-n|id> --workspace-id <uuid>"

// timelineMaxRows bounds each source read for one timeline.
const timelineMaxRows = 2000

// timelineEntry is one dated line. order breaks ties between entries of
// the same time (execution, events, jobs, waits, steps).
type timelineEntry struct {
	at    time.Time
	order int
	seq   int64
	text  string
}

// executionCommand runs `execution timeline` (Phase 12 REQ-9): the
// execution's lifecycle events, step summaries, jobs (attempts, retries,
// error codes, dead letters) and waits (approvals and their decisions) in
// time order. Times, ids, states, kinds and codes only: step keys are
// shown as their position and kind, never message text, prompts or tool
// input and output.
func executionCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "timeline" {
		return errors.New(executionUsage)
	}
	f := commandFlags("execution timeline")
	ws := f.String("workspace-id", "", "workspace UUID")
	pos, err := parseInterspersed(f, args[1:])
	if err != nil || !tenant.ValidUUID(*ws) || len(pos) != 1 {
		return errors.New(executionUsage)
	}
	id, ok := workflow.ParseRef(pos[0])
	if !ok {
		id, err = strconv.ParseInt(pos[0], 10, 64)
	}
	if err != nil || id <= 0 {
		return errors.New("execution reference must be WF-<positive integer> or a positive integer")
	}
	store, err := storage.NewWorkflowStore(pool)
	if err != nil {
		return errors.New("database operation failed")
	}
	if err := store.CheckWorkspace(ctx, *ws); err != nil {
		if errors.Is(err, workflow.ErrNotFound) {
			return errors.New("workspace not found")
		}
		return errors.New("database operation failed")
	}
	d, err := store.GetExecutionDetail(ctx, *ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	steps, err := store.ListSteps(ctx, *ws, id)
	if err != nil {
		return workflowStoreErr(err)
	}
	stepName := map[string]string{}
	for _, s := range steps {
		stepName[s.Key] = fmt.Sprintf("#%d(%s)", s.Position, workflowStepKind(s.Kind))
	}
	step := func(key string) string {
		if key == "" {
			return ""
		}
		if n, ok := stepName[key]; ok {
			return n
		}
		return "#?(unknown)"
	}
	var entries []timelineEntry
	add := func(at time.Time, order int, seq int64, parts ...string) {
		if at.IsZero() {
			return
		}
		var kept []string
		for _, p := range parts {
			if p != "" {
				kept = append(kept, p)
			}
		}
		entries = append(entries, timelineEntry{at: at.UTC(), order: order, seq: seq, text: strings.Join(kept, " ")})
	}
	add(d.CreatedAt, 0, 0, "execution", "created", "state="+workflowState(d.State))
	if d.StartedAt != nil {
		add(*d.StartedAt, 0, 1, "execution", "started")
	}
	if d.FinishedAt != nil {
		add(*d.FinishedAt, 0, 2, "execution", "finished", "state="+workflowState(d.State), kv("reason", workflowReason(d.Reason)))
	}

	// Lifecycle events, every page up to the bound.
	scanned := 0
	var after int64
	for scanned < timelineMaxRows {
		events, next, err := store.ListEvents(ctx, *ws, id, after, workflow.MaxListLimit)
		if err != nil {
			return workflowStoreErr(err)
		}
		for _, e := range events {
			transition := ""
			if e.FromState != "" || e.ToState != "" {
				transition = workflowState(e.FromState) + "->" + workflowState(e.ToState)
				if e.FromState == "" {
					transition = "->" + workflowState(e.ToState)
				}
			}
			attempt := ""
			if e.Attempt != 0 {
				attempt = "attempt=" + strconv.Itoa(e.Attempt)
			}
			add(e.CreatedAt, 1, e.Sequence, "event", workflowEventKind(e.Kind), transition, kv("step", step(e.StepKey)), attempt, kv("reason", workflowReason(e.Reason)))
		}
		scanned += len(events)
		if next == 0 || len(events) == 0 {
			break
		}
		after = next
	}

	// Jobs: one line when created, one with the last status, attempts and
	// error codes (a retry shows as attempts > 1 with last_error).
	scanned = 0
	var before int64
	for scanned < timelineMaxRows {
		jobs, next, err := store.ListJobs(ctx, workflow.JobQuery{WorkspaceID: *ws, ExecutionID: id, BeforeID: before, Limit: workflow.MaxListLimit})
		if err != nil {
			return workflowStoreErr(err)
		}
		for _, j := range jobs {
			ref := "job=" + strconv.FormatInt(j.ID, 10)
			add(j.CreatedAt, 2, j.ID, "job", ref, "queued", kv("step", step(j.StepKey)))
			retries := ""
			if j.Attempts > 1 {
				retries = "retries=" + strconv.Itoa(j.Attempts-1)
			}
			add(j.UpdatedAt, 2, j.ID, "job", ref, "status="+workflowJobStatus(j.Status), kv("step", step(j.StepKey)),
				fmt.Sprintf("attempts=%d/%d", j.Attempts, j.MaxAttempts), retries,
				kv("last_error", workflowCode(j.LastErrorCode)), kv("dead_letter", workflowCode(j.DeadLetterReason)))
		}
		scanned += len(jobs)
		if next == 0 || len(jobs) == 0 {
			break
		}
		before = next
	}

	// Waits and approvals.
	for _, w := range d.Waits {
		ref := "wait=" + strconv.FormatInt(w.ID, 10)
		approval := ""
		if w.ApprovalID != 0 {
			approval = "approval=" + strconv.FormatInt(w.ApprovalID, 10)
		}
		add(w.CreatedAt, 3, w.ID, "wait", ref, "opened", "kind="+workflowWaitKind(w.Kind), kv("step", step(w.StepKey)), approval)
		if w.DecidedAt != nil {
			add(*w.DecidedAt, 3, w.ID, "wait", ref, "decided", kv("decision", workflowDecision(w.Decision)), approval)
		}
		if w.ClosedAt != nil {
			add(*w.ClosedAt, 3, w.ID, "wait", ref, "closed")
		}
		if w.ClosedAt == nil && w.DecidedAt == nil && w.ExpiresAt != nil {
			add(*w.ExpiresAt, 3, w.ID, "wait", ref, "expires")
		}
	}

	// Step summaries at their last update.
	for _, s := range steps {
		add(s.UpdatedAt, 4, int64(s.Position), "step", step(s.Key), "state="+workflowStepState(s.State), "attempts="+strconv.Itoa(s.Attempts))
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.seq < b.seq
	})
	fmt.Fprintf(out, "execution %s state=%s%s\n", workflow.FormatRef(id), workflowState(d.State), prefixed(" reason=", workflowReason(d.Reason)))
	for _, e := range entries {
		fmt.Fprintf(out, "%s  %s\n", e.at.Format(time.RFC3339), e.text)
	}
	return nil
}

// kv renders key=value, or nothing when value is empty.
func kv(key, value string) string {
	if value == "" {
		return ""
	}
	return key + "=" + value
}

func prefixed(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}
