package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Fix B regressions (review B) for the alert rows of migration 00040:
// R-B-1 bounded retries, R-B-3 indexes and migration, R-B-4 the database
// rate cap and summaries, and the obsolete close. One database for all
// subtests; fixed times relative to base; no sleeps.

type p12bAlertRow struct {
	status     string
	attempts   int
	next, sent *time.Time
	lastCode   *string
	summarised *time.Time
}

func sameInstant(got *time.Time, want time.Time) bool {
	return got != nil && got.Equal(want)
}

func TestPhase12bFixBAlertRows(t *testing.T) {
	e := newWFEnv(t)
	ctx := e.ctx
	ops, err := NewOpsStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) time.Time { return base.Add(-d) }
	va := e.enabledVersion(workspaceA, "fixb-a")
	vb := e.enabledVersion(workspaceB, "fixb-b")
	n := 0
	ex := func(ws string, v int64) int64 {
		n++
		return p12bExec(e, ws, v, fmt.Sprintf("fixb:%d", n))
	}
	row := func(ws string, id int64, st behavior.AlertState) p12bAlertRow {
		t.Helper()
		var r p12bAlertRow
		err := e.pool.QueryRow(ctx, `SELECT status, attempts, next_attempt_at, sent_at, last_error_code, summarised_at
			FROM workflow_alerts WHERE workspace_id=$1 AND execution_id=$2 AND state=$3`, ws, id, string(st)).
			Scan(&r.status, &r.attempts, &r.next, &r.sent, &r.lastCode, &r.summarised)
		if err != nil {
			t.Fatalf("row %d %s: %v", id, st, err)
		}
		return r
	}
	alert := func(ws string, id int64, st behavior.AlertState) behavior.WorkflowAlert {
		return behavior.WorkflowAlert{WorkspaceID: ws, ExecutionID: id, State: st, Since: ago(time.Minute), ErrorCode: "failed"}
	}
	claim := func(a behavior.WorkflowAlert, at time.Time) {
		t.Helper()
		if ok, err := ops.ClaimAlert(ctx, a, at); err != nil || !ok {
			t.Fatalf("claim %d %s: %v %v", a.ExecutionID, a.State, ok, err)
		}
	}

	// R-B-1: attempts 1..5 with the documented backoff, then failed for good.
	t.Run("attempts walk the backoff and end failed", func(t *testing.T) {
		id := ex(workspaceA, va.ID)
		p12bSetState(e, id, "FAILED")
		a := alert(workspaceA, id, behavior.AlertFailed)
		claim(a, base)
		r := row(workspaceA, id, a.State)
		if r.status != "pending" || r.attempts != 0 || !sameInstant(r.next, base.Add(behavior.AlertClaimLease)) || r.sent != nil {
			t.Fatalf("new claim = %+v", r)
		}
		if ok, err := ops.ClaimAlert(ctx, a, base.Add(time.Hour)); err != nil || ok {
			t.Fatalf("second claim: %v %v", ok, err)
		}
		if ok, err := ops.SuppressAlert(ctx, a, base.Add(time.Hour)); err != nil || ok {
			t.Fatalf("suppress of an existing row: %v %v", ok, err)
		}
		if r2 := row(workspaceA, id, a.State); r2.status != "pending" || r2.attempts != 0 {
			t.Fatalf("existing row changed: %+v", r2)
		}
		if due, _ := ops.ListAlertRetries(ctx, base.Add(behavior.AlertClaimLease-time.Second), 20); len(due) != 0 {
			t.Fatalf("not due yet, listed: %+v", due)
		}
		due, err := ops.ListAlertRetries(ctx, base.Add(behavior.AlertClaimLease), 20)
		if err != nil || len(due) != 1 || due[0].ExecutionID != id || due[0].Attempts != 0 || due[0].State != behavior.AlertFailed || due[0].BehaviorName != "fixb-a" || due[0].Obsolete {
			t.Fatalf("due retries = %+v %v", due, err)
		}
		want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
		at := base
		for i := 0; i < behavior.AlertMaxAttempts; i++ {
			cur := a
			cur.Attempts = i
			d, ok := behavior.AlertRetryDelay(i + 1)
			var next time.Time
			if i < len(want) {
				if !ok || d != want[i] {
					t.Fatalf("delay after attempt %d = %v %v, want %v", i+1, d, ok, want[i])
				}
				next = at.Add(d)
			} else if ok {
				t.Fatalf("attempt %d must be the last", i+1)
			}
			if err := ops.MarkAlertAttemptFailed(ctx, cur, "send_timeout", next); err != nil {
				t.Fatal(err)
			}
			r := row(workspaceA, id, a.State)
			if r.attempts != i+1 || r.lastCode == nil || *r.lastCode != "send_timeout" {
				t.Fatalf("after attempt %d: %+v", i+1, r)
			}
			if i < len(want) {
				if r.status != "pending" || !sameInstant(r.next, next) {
					t.Fatalf("after attempt %d: %+v, want pending next %v", i+1, r, next)
				}
				// A duplicate of an attempt already recorded changes nothing.
				if err := ops.MarkAlertAttemptFailed(ctx, cur, "send_failed", next.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if again := row(workspaceA, id, a.State); again.attempts != i+1 || !sameInstant(again.next, next) {
					t.Fatalf("stale attempt applied: %+v", again)
				}
				at = next
			} else if r.status != "failed" || r.next != nil || r.sent != nil {
				t.Fatalf("after the last attempt: %+v, want failed without next", r)
			}
		}
		if due, _ := ops.ListAlertRetries(ctx, base.Add(1000*time.Hour), 20); len(due) != 0 {
			t.Fatalf("a failed alert is retried: %+v", due)
		}
		// Never sent after failing.
		if err := ops.MarkAlertSent(ctx, a, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if r := row(workspaceA, id, a.State); r.status != "failed" || r.sent != nil {
			t.Fatalf("failed alert marked sent: %+v", r)
		}
		errs, err := ops.Errors(ctx, "", ago(24*time.Hour))
		if err != nil || errs.FailedAlerts != 1 {
			t.Fatalf("failed alerts = %+v %v", errs, err)
		}
		if one, _ := ops.Errors(ctx, workspaceB, ago(24*time.Hour)); one.FailedAlerts != 0 {
			t.Fatalf("workspace B failed alerts = %d", one.FailedAlerts)
		}
	})

	t.Run("crash after claim is due after the lease; unsafe code is replaced", func(t *testing.T) {
		id := ex(workspaceA, va.ID)
		a := alert(workspaceA, id, behavior.AlertStuckRunning)
		claim(a, base)
		lease := behavior.AlertClaimLease
		if due, _ := ops.ListAlertRetries(ctx, base.Add(lease-time.Second), 20); len(due) != 0 {
			t.Fatalf("retried before the lease ended: %+v", due)
		}
		found := false
		due, _ := ops.ListAlertRetries(ctx, base.Add(lease), 20)
		for _, d := range due {
			found = found || (d.ExecutionID == id && d.Attempts == 0)
		}
		if !found {
			t.Fatalf("claimed-but-unsent alert not retried after %v: %+v", lease, due)
		}
		if err := ops.MarkAlertAttemptFailed(ctx, a, "Bad Code! with spaces", base.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if r := row(workspaceA, id, a.State); r.lastCode == nil || *r.lastCode != "send_failed" || r.attempts != 1 {
			t.Fatalf("unsafe code stored: %+v", r)
		}
	})

	t.Run("sent row keeps its time; a second mark changes nothing", func(t *testing.T) {
		id := ex(workspaceA, va.ID)
		a := alert(workspaceA, id, behavior.AlertStuckQueued)
		claim(a, base)
		if err := ops.MarkAlertSent(ctx, a, base.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		r := row(workspaceA, id, a.State)
		if r.status != "sent" || !sameInstant(r.sent, base.Add(10*time.Second)) || r.next != nil || r.attempts != 1 || r.lastCode != nil {
			t.Fatalf("sent row = %+v", r)
		}
		if err := ops.MarkAlertSent(ctx, a, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if r2 := row(workspaceA, id, a.State); !sameInstant(r2.sent, base.Add(10*time.Second)) {
			t.Fatalf("sent_at moved: %+v", r2)
		}
	})

	// R-B-4: the hourly cap counts delivered rows hub-wide, from the table.
	t.Run("sent count covers the last hour hub-wide", func(t *testing.T) {
		before, err := ops.CountAlertsSentSince(ctx, ago(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		a, b := ex(workspaceA, va.ID), ex(workspaceB, vb.ID)
		for _, c := range []struct {
			a  behavior.WorkflowAlert
			at time.Time
		}{
			{alert(workspaceA, a, behavior.AlertFailed), ago(30 * time.Minute)},
			{alert(workspaceB, b, behavior.AlertFailed), ago(10 * time.Minute)},
			{alert(workspaceA, a, behavior.AlertStuckRunning), ago(90 * time.Minute)},
		} {
			claim(c.a, c.at)
			if err := ops.MarkAlertSent(ctx, c.a, c.at); err != nil {
				t.Fatal(err)
			}
		}
		// Suppressed and pending rows never count.
		if ok, err := ops.SuppressAlert(ctx, alert(workspaceA, a, behavior.AlertStuckQueued), ago(5*time.Minute)); err != nil || !ok {
			t.Fatalf("suppress: %v %v", ok, err)
		}
		after, err := ops.CountAlertsSentSince(ctx, ago(time.Hour))
		if err != nil || after != before+2 {
			t.Fatalf("sent in the last hour: %d -> %d %v, want +2 (the 90-minute one is outside)", before, after, err)
		}
		if wide, _ := ops.CountAlertsSentSince(ctx, ago(2*time.Hour)); wide != before+3 {
			t.Fatalf("two-hour count = %d, want %d", wide, before+3)
		}
	})

	t.Run("summaries are built from suppressed rows once", func(t *testing.T) {
		a, b := ex(workspaceA, va.ID), ex(workspaceB, vb.ID)
		sup := func(ws string, id int64, st behavior.AlertState, at time.Time) {
			t.Helper()
			if ok, err := ops.SuppressAlert(ctx, alert(ws, id, st), at); err != nil || !ok {
				t.Fatalf("suppress: %v %v", ok, err)
			}
		}
		sup(workspaceA, a, behavior.AlertFailed, ago(3*time.Hour))
		sup(workspaceA, a, behavior.AlertStuckRunning, ago(2*time.Hour))
		sup(workspaceA, a, behavior.AlertApprovalOverdue, ago(90*time.Minute))
		sup(workspaceB, b, behavior.AlertFailed, ago(80*time.Minute))
		sup(workspaceB, b, behavior.AlertStuckQueued, ago(10*time.Minute))
		if r := row(workspaceA, a, behavior.AlertFailed); r.status != "suppressed" || r.next != nil || r.sent != nil || r.summarised != nil {
			t.Fatalf("suppressed row = %+v", r)
		}
		// Earlier tests left no suppressed rows of A except the one of the
		// rate subtest (stuck_queued, 5 minutes old); count what is new.
		get := func(before time.Time) map[string]behavior.AlertSummary {
			t.Helper()
			list, err := ops.ListAlertSummaries(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			m := map[string]behavior.AlertSummary{}
			for _, s := range list {
				m[s.WorkspaceID] = s
			}
			return m
		}
		all := get(base)
		if all[workspaceA].Count != 4 || !all[workspaceA].OldestAt.Equal(ago(3*time.Hour)) ||
			all[workspaceB].Count != 2 || !all[workspaceB].OldestAt.Equal(ago(80*time.Minute)) {
			t.Fatalf("summaries = %+v", all)
		}
		// Only rows claimed before the cut are counted and marked.
		if err := ops.MarkAlertsSummarised(ctx, workspaceA, ago(100*time.Minute), base); err != nil {
			t.Fatal(err)
		}
		if r := row(workspaceA, a, behavior.AlertFailed); !sameInstant(r.summarised, base) || r.status != "suppressed" {
			t.Fatalf("old row after summary = %+v", r)
		}
		if r := row(workspaceA, a, behavior.AlertApprovalOverdue); r.summarised != nil {
			t.Fatalf("newer row marked: %+v", r)
		}
		mid := get(base)
		if mid[workspaceA].Count != 2 || !mid[workspaceA].OldestAt.Equal(ago(90*time.Minute)) || mid[workspaceB].Count != 2 {
			t.Fatalf("after partial summary = %+v", mid)
		}
		// A cut before a row's claim leaves it out of the list.
		if early := get(ago(85 * time.Minute)); early[workspaceB].Count != 0 || early[workspaceA].Count != 1 {
			t.Fatalf("early cut = %+v", early)
		}
		if err := ops.MarkAlertsSummarised(ctx, workspaceA, base, base); err != nil {
			t.Fatal(err)
		}
		done := get(base)
		if _, ok := done[workspaceA]; ok || done[workspaceB].Count != 2 {
			t.Fatalf("after full summary = %+v (workspace B must stay)", done)
		}
		if err := ops.MarkAlertsSummarised(ctx, "x", base, base); err == nil {
			t.Error("MarkAlertsSummarised accepted a bad workspace id")
		}
	})

	// R-B-1: a row in any status hides its candidate; old rows never push a
	// new alert out of the batch.
	t.Run("candidates skip rows of every status and old rows do not block", func(t *testing.T) {
		fresh := ex(workspaceA, va.ID)
		p12bSetState(e, fresh, "FAILED")
		statuses := []string{"pending", "sent", "failed", "suppressed", "pending", "sent", "failed"}
		for i, s := range statuses {
			id := ex(workspaceA, va.ID)
			p12bSetState(e, id, "FAILED")
			mustExec(t, ctx, e.pool, `UPDATE workflow_executions SET finished_at=$2 WHERE id=$1`, id, ago(time.Duration(10+i)*time.Hour))
			a := alert(workspaceA, id, behavior.AlertFailed)
			switch s {
			case "suppressed":
				if ok, err := ops.SuppressAlert(ctx, a, ago(time.Duration(10+i)*time.Hour)); err != nil || !ok {
					t.Fatalf("suppress: %v %v", ok, err)
				}
			default:
				claim(a, ago(time.Duration(10+i)*time.Hour))
				switch s {
				case "sent":
					if err := ops.MarkAlertSent(ctx, a, ago(time.Duration(10+i)*time.Hour)); err != nil {
						t.Fatal(err)
					}
				case "failed":
					if err := ops.MarkAlertAttemptFailed(ctx, a, "send_failed", time.Time{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if r := row(workspaceA, id, behavior.AlertFailed); r.status != s {
				t.Fatalf("seed %d: status %q, want %q", i, r.status, s)
			}
		}
		q := behavior.AlertQuery{
			RunningBefore: ago(15 * time.Minute), QueuedBefore: ago(30 * time.Minute),
			ApprovalBefore: ago(5 * time.Minute), FailedSince: ago(24 * time.Hour), Limit: 3,
		}
		// The fresh failure is the newest: only the exclusion of old rows
		// lets it into a batch of three.
		list, err := ops.ListAlertCandidates(ctx, q)
		if err != nil || len(list) != 1 || list[0].ExecutionID != fresh || list[0].State != behavior.AlertFailed {
			t.Fatalf("candidates = %+v %v, want only execution %d", list, err, fresh)
		}
		// A stuck_queued candidate disappears once its row is obsolete.
		qid := ex(workspaceA, va.ID)
		p12bJob(e, qid, "pending", ago(2*time.Hour), ago(2*time.Hour))
		q.Limit = 10
		list, _ = ops.ListAlertCandidates(ctx, q)
		if len(list) != 2 {
			t.Fatalf("candidates before obsolete = %+v", list)
		}
		qa := alert(workspaceA, qid, behavior.AlertStuckQueued)
		claim(qa, ago(time.Hour))
		if err := ops.MarkAlertObsolete(ctx, qa); err != nil {
			t.Fatal(err)
		}
		if r := row(workspaceA, qid, qa.State); r.status != "obsolete" || r.next != nil || r.sent != nil {
			t.Fatalf("obsolete row = %+v", r)
		}
		list, _ = ops.ListAlertCandidates(ctx, q)
		if len(list) != 1 || list[0].ExecutionID != fresh {
			t.Fatalf("an obsolete row must still hide its candidate: %+v", list)
		}
	})

	t.Run("due retry flags obsolete only for ended stuck or overdue states", func(t *testing.T) {
		flag := func(id int64, st behavior.AlertState) bool {
			t.Helper()
			due, err := ops.ListAlertRetries(ctx, base.Add(24*time.Hour), 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range due {
				if d.ExecutionID == id && d.State == st {
					return d.Obsolete
				}
			}
			t.Fatalf("retry %d %s not listed", id, st)
			return false
		}
		run := ex(workspaceA, va.ID)
		p12bSetState(e, run, "RUNNING")
		p12bJob(e, run, "processing", ago(time.Hour), ago(time.Hour))
		ra := alert(workspaceA, run, behavior.AlertStuckRunning)
		claim(ra, base)
		if flag(run, ra.State) {
			t.Fatal("a running execution with a processing job is not obsolete")
		}
		p12bSetState(e, run, "COMPLETED")
		if !flag(run, ra.State) {
			t.Fatal("an ended execution must be obsolete")
		}
		wait := ex(workspaceA, va.ID)
		p12bSetState(e, wait, "WAITING_FOR_APPROVAL")
		wa := alert(workspaceA, wait, behavior.AlertApprovalOverdue)
		claim(wa, base)
		if flag(wait, wa.State) {
			t.Fatal("still waiting for approval is not obsolete")
		}
		p12bSetState(e, wait, "RUNNING")
		if !flag(wait, wa.State) {
			t.Fatal("no longer waiting for approval must be obsolete")
		}
		// A failed alert is never obsolete, and cannot be closed as such.
		fa := ex(workspaceA, va.ID)
		p12bSetState(e, fa, "COMPLETED")
		fal := alert(workspaceA, fa, behavior.AlertFailed)
		claim(fal, base)
		if flag(fa, fal.State) {
			t.Fatal("failed alerts are never obsolete")
		}
		if err := ops.MarkAlertObsolete(ctx, fal); err != nil {
			t.Fatal(err)
		}
		if r := row(workspaceA, fa, fal.State); r.status != "pending" {
			t.Fatalf("failed alert closed as obsolete: %+v", r)
		}
		// Closing the stuck one drops it from the retries.
		if err := ops.MarkAlertObsolete(ctx, ra); err != nil {
			t.Fatal(err)
		}
		due, _ := ops.ListAlertRetries(ctx, base.Add(24*time.Hour), 100)
		for _, d := range due {
			if d.ExecutionID == run && d.State == ra.State {
				t.Fatal("obsolete alert still listed for retry")
			}
		}
	})

	// R-B-3: indexes, EXPLAIN (advisory) and the 00040 Down and Up.
	t.Run("migration 00040 indexes and down up", func(t *testing.T) {
		defs := map[string]string{}
		rows, err := e.pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname='public' AND tablename IN ('workflow_alerts','workflow_executions')`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatal(err)
			}
			defs[name] = def
		}
		rows.Close()
		if d := defs["workflow_executions_failed_finished_idx"]; !strings.Contains(d, "finished_at") || !strings.Contains(d, "FAILED") {
			t.Fatalf("failed-scan index = %q", d)
		}
		for _, name := range []string{"workflow_alerts_retry_idx", "workflow_alerts_unsummarised_idx", "workflow_alerts_created_idx"} {
			if defs[name] == "" {
				t.Errorf("index %s missing", name)
			}
		}
		// EXPLAIN is a hint only: the planner may prefer another plan.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
		var plan strings.Builder
		if r, err := tx.Query(ctx, `EXPLAIN SELECT id FROM workflow_executions WHERE state='FAILED' AND finished_at >= now() - interval '24 hours'`); err == nil {
			for r.Next() {
				var line string
				_ = r.Scan(&line)
				plan.WriteString(line + "\n")
			}
			r.Close()
		}
		_ = tx.Rollback(ctx)
		if !strings.Contains(plan.String(), "workflow_executions_failed_finished_idx") {
			t.Logf("failed scan did not choose the partial index (advisory):\n%s", plan.String())
		}

		hasCol := func(col string) bool {
			t.Helper()
			var c int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='workflow_alerts' AND column_name=$1`, col).Scan(&c); err != nil {
				t.Fatal(err)
			}
			return c == 1
		}
		var total, pending int
		if err := e.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='pending') FROM workflow_alerts`).Scan(&total, &pending); err != nil || pending == 0 || total == pending {
			t.Fatalf("need pending and other rows before Down: %d/%d %v", pending, total, err)
		}
		mustExec(t, ctx, e.pool, downSection(t, "00040_workflow_alert_retries.sql"))
		for _, col := range []string{"status", "attempts", "next_attempt_at", "last_error_code", "since_at", "error_code", "created_at", "summarised_at"} {
			if hasCol(col) {
				t.Errorf("column %s still there after Down", col)
			}
		}
		var nullable string
		if err := e.pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema='public' AND table_name='workflow_alerts' AND column_name='sent_at'`).Scan(&nullable); err != nil || nullable != "NO" {
			t.Fatalf("sent_at after Down: nullable=%q %v, want the 00039 shape (NOT NULL)", nullable, err)
		}
		var left int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_alerts WHERE sent_at IS NOT NULL`).Scan(&left); err != nil || left != total-pending {
			t.Fatalf("rows after Down = %d %v, want %d (pending claims deleted)", left, err, total-pending)
		}
		var idx int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname IN ('workflow_executions_failed_finished_idx','workflow_alerts_retry_idx','workflow_alerts_unsummarised_idx','workflow_alerts_created_idx')`).Scan(&idx); err != nil || idx != 0 {
			t.Fatalf("indexes after Down: %d %v", idx, err)
		}
		mustExec(t, ctx, e.pool, upSection(t, "00040_workflow_alert_retries.sql"))
		var sent int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_alerts WHERE status='sent' AND sent_at IS NOT NULL AND next_attempt_at IS NULL`).Scan(&sent); err != nil || sent != left {
			t.Fatalf("rows after Up = %d %v, want %d kept as sent", sent, err, left)
		}
		if !hasCol("status") || !hasCol("summarised_at") {
			t.Error("columns missing after Up")
		}
	})
}
