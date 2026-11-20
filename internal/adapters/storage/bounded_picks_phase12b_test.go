package storage

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 12 fix-A2 (D-A-1 class): a bounded pick (LIMIT + FOR UPDATE SKIP
// LOCKED) must touch exactly min(max_rows, unlocked qualifying) rows, in
// order, and leave every other row alone. The IN (SELECT ... LIMIT ...)
// form re-evaluated its subquery and touched more rows than max_rows.

// p12bLock holds row locks in another transaction until the test releases it.
func p12bLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	rows.Close()
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

func p12bIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func p12bSameIDs(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
}

// TC-B-45..49 (D-A-1 class, fix-A2)
func TestPhase12bBoundedPicksNeverExceedLimit(t *testing.T) {
	e := newWFEnv(t)
	ctx, pool := e.ctx, e.pool
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integrationA, workspaceA)
	va := e.enabledVersion(workspaceA, "picks-a")
	ended := p12bExec(e, workspaceA, va.ID, "picks:ended")
	live := p12bExec(e, workspaceA, va.ID, "picks:live")
	late := p12bExec(e, workspaceA, va.ID, "picks:late")
	p12bSetState(e, ended, "COMPLETED")
	p12bSetState(e, live, "RUNNING")
	mustExec(t, ctx, pool, `UPDATE workflow_executions SET deadline_at=now()-interval '1 hour' WHERE id=$1`, late)
	now := time.Now()

	const total = 30
	const limit = 10

	// drain runs the pick three ways and checks the arithmetic:
	//   1. limit rows from a clean set (earliest first);
	//   2. with all but 5 qualifying rows locked elsewhere: exactly 5;
	//   3. unlocked again: the rest, then 0.
	type pickCase struct {
		name string
		// call runs the query and returns the touched ids.
		call func(max int) []int64
		// qualifying lists the qualifying ids in the query's order.
		qualifying []int64
		// lockSQL locks the rows with ids = ANY($1) in another tx.
		lockSQL string
		// untouched is checked unchanged by check().
		check func(t *testing.T, touched map[int64]bool)
	}

	run := func(t *testing.T, c pickCase) {
		t.Helper()
		if len(c.qualifying) != total {
			t.Fatalf("setup: %d qualifying rows", len(c.qualifying))
		}
		touched := map[int64]bool{}
		note := func(label string, ids []int64, want []int64) {
			t.Helper()
			// RETURNING order is not part of the contract; the picked set is.
			ids = slices.Sorted(slices.Values(ids))
			p12bSameIDs(t, label, ids, want)
			for _, id := range ids {
				if touched[id] {
					t.Fatalf("%s: row %d touched twice", label, id)
				}
				touched[id] = true
			}
		}
		// 1. exactly limit rows, the first ones in order.
		note("first pick", c.call(limit), c.qualifying[:limit])
		// 2. lock 15 of the remaining 20 (leave 5 unlocked, the last ones).
		lockIDs := c.qualifying[limit : limit+15]
		release := p12bLock(t, ctx, pool, c.lockSQL, lockIDs)
		note("pick with locked rows", c.call(limit), c.qualifying[limit+15:])
		// A pick while everything left is locked touches nothing.
		if got := c.call(limit); len(got) != 0 {
			t.Fatalf("pick of locked rows touched %v", got)
		}
		release()
		// 3. unlocked: the remaining 15 in order, with a limit above the rest.
		note("pick after release", c.call(100), c.qualifying[limit:limit+15])
		if got := c.call(100); len(got) != 0 {
			t.Fatalf("empty pick touched %v", got)
		}
		c.check(t, touched)
	}

	t.Run("ExpireClickUpDrafts", func(t *testing.T) {
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,expires_at)
			SELECT $1,$2,-1001,'q'||g,'b',repeat('a',64),CASE WHEN g%2=0 THEN 'pending' ELSE 'failed' END,'assistant',$3::timestamptz+g*interval '1 second'
			FROM generate_series(1,$4) g`, workspaceA, integrationA, now.Add(-2*time.Hour), total)
		// Non-qualifying: future expiry, posting, posted.
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,expires_at) VALUES
			($1,$2,-1001,'n-future','b',repeat('a',64),'pending','assistant',now()+interval '1 hour'),
			($1,$2,-1001,'n-posting','b',repeat('a',64),'posting','assistant',now()-interval '1 hour')`, workspaceA, integrationA)
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,expires_at,finished_at)
			VALUES($1,$2,-1001,'n-posted',NULL,repeat('a',64),'posted','assistant',now()-interval '1 hour',now())`, workspaceA, integrationA)
		s, err := NewClickUpDraftStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		before := p12bIDs(t, ctx, pool, `SELECT id FROM clickup_drafts WHERE task_id LIKE 'n-%' ORDER BY id`)
		run(t, pickCase{
			name: "expire",
			call: func(max int) []int64 {
				rows, err := s.ExpireDrafts(ctx, time.Now(), max)
				if err != nil {
					t.Fatal(err)
				}
				var ids []int64
				for _, r := range rows {
					ids = append(ids, r.ID)
				}
				return ids
			},
			qualifying: p12bIDs(t, ctx, pool, `SELECT id FROM clickup_drafts WHERE task_id LIKE 'q%' ORDER BY expires_at`),
			lockSQL:    `SELECT id FROM clickup_drafts WHERE id = ANY($1) FOR UPDATE`,
			check: func(t *testing.T, touched map[int64]bool) {
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE state='expired' AND body IS NULL AND finished_at IS NOT NULL`); n != total {
					t.Fatalf("expired = %d", n)
				}
				p12bSameIDs(t, "non-qualifying rows", p12bIDs(t, ctx, pool, `SELECT id FROM clickup_drafts WHERE task_id LIKE 'n-%' AND state IN ('pending','posting','posted') ORDER BY id`), before)
			},
		})
	})

	t.Run("ReclaimStaleClickUpDraftPosts", func(t *testing.T) {
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,expires_at,updated_at)
			SELECT $1,$2,-1002,'r'||g,'b',repeat('a',64),'posting','assistant',now()+interval '1 day',$3::timestamptz+g*interval '1 second'
			FROM generate_series(1,$4) g`, workspaceA, integrationA, now.Add(-3*time.Hour), total)
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,expires_at,updated_at) VALUES
			($1,$2,-1002,'m-fresh','b',repeat('a',64),'posting','assistant',now()+interval '1 day',now()),
			($1,$2,-1002,'m-pending','b',repeat('a',64),'pending','assistant',now()+interval '1 day',now()-interval '5 hours')`, workspaceA, integrationA)
		s, _ := NewClickUpDraftStore(pool)
		cutoff := time.Now().Add(-30 * time.Minute)
		run(t, pickCase{
			name: "reclaim",
			call: func(max int) []int64 {
				rows, err := s.ReclaimStalePosts(ctx, cutoff, max)
				if err != nil {
					t.Fatal(err)
				}
				var ids []int64
				for _, r := range rows {
					ids = append(ids, r.ID)
				}
				return ids
			},
			qualifying: p12bIDs(t, ctx, pool, `SELECT id FROM clickup_drafts WHERE task_id LIKE 'r%' ORDER BY updated_at`),
			lockSQL:    `SELECT id FROM clickup_drafts WHERE id = ANY($1) FOR UPDATE`,
			check: func(t *testing.T, touched map[int64]bool) {
				// Reclaimed rows stay posting with a fresh updated_at; the others are unchanged.
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE task_id LIKE 'r%' AND state='posting' AND updated_at > now()-interval '1 minute'`); n != total {
					t.Fatalf("reclaimed rows with a fresh updated_at = %d", n)
				}
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE task_id='m-fresh' AND state='posting'`); n != 1 {
					t.Fatal("fresh posting draft changed")
				}
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE task_id='m-pending' AND state='pending' AND updated_at < now()-interval '4 hours'`); n != 1 {
					t.Fatal("pending draft touched")
				}
			},
		})
	})

	t.Run("CancelEndedExecutionClickUpDrafts", func(t *testing.T) {
		// 15 on the ended execution, 15 on the one past its deadline.
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,execution_id,expires_at)
			SELECT $1,$2,-1003,'c'||g,'b',repeat('a',64),CASE WHEN g%3=0 THEN 'failed' ELSE 'pending' END,'orchestrator',CASE WHEN g%2=0 THEN $3::bigint ELSE $4::bigint END,now()+interval '1 day'
			FROM generate_series(1,$5) g`, workspaceA, integrationA, ended, late, total)
		// Non-qualifying: live execution, assistant source, posting.
		mustExec(t, ctx, pool, `INSERT INTO clickup_drafts(workspace_id,integration_id,chat_id,task_id,body,body_sha256,state,source,execution_id,expires_at) VALUES
			($1,$2,-1003,'k-live','b',repeat('a',64),'pending','orchestrator',$3,now()+interval '1 day'),
			($1,$2,-1003,'k-assistant','b',repeat('a',64),'pending','assistant',$4,now()+interval '1 day'),
			($1,$2,-1003,'k-posting','b',repeat('a',64),'posting','orchestrator',$4,now()+interval '1 day')`, workspaceA, integrationA, live, ended)
		s, _ := NewClickUpDraftStore(pool)
		run(t, pickCase{
			name: "cancel ended",
			call: func(max int) []int64 {
				rows, err := s.CancelEndedExecutionDrafts(ctx, time.Now(), max)
				if err != nil {
					t.Fatal(err)
				}
				var ids []int64
				for _, r := range rows {
					ids = append(ids, r.ID)
				}
				return ids
			},
			qualifying: p12bIDs(t, ctx, pool, `SELECT id FROM clickup_drafts WHERE task_id LIKE 'c%' ORDER BY id`),
			lockSQL:    `SELECT id FROM clickup_drafts WHERE id = ANY($1) FOR UPDATE`,
			check: func(t *testing.T, touched map[int64]bool) {
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE task_id LIKE 'c%' AND state='cancelled' AND reason_code='execution_ended' AND body IS NULL`); n != total {
					t.Fatalf("cancelled = %d", n)
				}
				if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE task_id LIKE 'k-%' AND state IN ('pending','posting') AND body IS NOT NULL`); n != 3 {
					t.Fatalf("non-qualifying drafts changed (%d of 3 intact)", n)
				}
			},
		})
	})

	t.Run("ExpireEndedExecutionPolicyGates", func(t *testing.T) {
		mustExec(t, ctx, pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id)
			SELECT $1,'policy_gate','gate-nonce-'||lpad(g::text,10,'0'),now()+interval '1 hour','github.comment','r-'||g,'',CASE WHEN g%2=0 THEN $2::bigint ELSE $3::bigint END
			FROM generate_series(1,$4) g`, workspaceA, ended, late, total)
		mustExec(t, ctx, pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id) VALUES
			($1,'policy_gate','live-gate-nonce-000001',now()+interval '1 hour','github.comment','live','',$2)`, workspaceA, live)
		mustExec(t, ctx, pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id,decision,decided_at) VALUES
			($1,'policy_gate','done-gate-nonce-000001',now()+interval '1 hour','github.comment','done','',$2,'approved',now())`, workspaceA, ended)
		s, err := NewPolicyStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		run(t, pickCase{
			name: "gates",
			call: func(max int) []int64 {
				rows, err := s.ExpireEndedExecutionGates(ctx, time.Now(), max)
				if err != nil {
					t.Fatal(err)
				}
				var ids []int64
				for _, r := range rows {
					ids = append(ids, r.ID)
				}
				return ids
			},
			qualifying: p12bIDs(t, ctx, pool, `SELECT id FROM approvals WHERE kind='policy_gate' AND resource_key ~ '^r-[0-9]+$' ORDER BY id`),
			lockSQL:    `SELECT id FROM approvals WHERE id = ANY($1) FOR UPDATE`,
			check: func(t *testing.T, touched map[int64]bool) {
				if n := e.count(`SELECT count(*) FROM approvals WHERE kind='policy_gate' AND resource_key ~ '^r-[0-9]+$' AND decision='expired' AND decided_by='system'`); n != total {
					t.Fatalf("expired gates = %d", n)
				}
				if n := e.count(`SELECT count(*) FROM approvals WHERE resource_key='live' AND decision IS NULL`); n != 1 {
					t.Fatal("gate of a live execution expired")
				}
				if n := e.count(`SELECT count(*) FROM approvals WHERE resource_key='done' AND decision='approved'`); n != 1 {
					t.Fatal("decided gate changed")
				}
			},
		})
	})

	t.Run("AssignDigestEntries", func(t *testing.T) {
		batch := func() int64 {
			var id int64
			if err := pool.QueryRow(ctx, `INSERT INTO behavior_digest_batches(workspace_id,integration_id,chat_id) VALUES($1,$2,5) RETURNING id`, workspaceA, integrationA).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		mine, old := batch(), batch()
		mustExec(t, ctx, pool, `INSERT INTO behavior_runs(workspace_id,behavior_id,behavior_version_id,version,trigger_kind,subject,revision,correlation_id)
			SELECT $1,$2,$3,$4,'x','digest-'||lpad(g::text,3,'0'),'r',gen_random_uuid() FROM generate_series(1,$5) g`, workspaceA, va.BehaviorID, va.ID, va.Version, total+15)
		runs := p12bIDs(t, ctx, pool, `SELECT id FROM behavior_runs WHERE subject LIKE 'digest-%' ORDER BY id`)
		entry := func(run int64, chat int64, due time.Time, batchID *int64) {
			t.Helper()
			mustExec(t, ctx, pool, `INSERT INTO behavior_digest_entries(workspace_id,run_id,msg_key,integration_id,chat_id,due_at,batch_id) VALUES($1,$2,'digest',$3,$4,$5,$6)`,
				workspaceA, run, integrationA, chat, due, batchID)
		}
		for _, r := range runs[:total] {
			entry(r, 5, now.Add(-time.Hour), nil) // qualifying, chat 5
		}
		for _, r := range runs[total : total+5] {
			entry(r, 6, now.Add(-time.Hour), nil) // another chat
		}
		for _, r := range runs[total+5 : total+10] {
			entry(r, 5, now.Add(time.Hour), nil) // not due yet
		}
		for _, r := range runs[total+10:] {
			entry(r, 5, now.Add(-time.Hour), &old) // already batched
		}
		q := sqlc.New(pool)
		call := func(max int) []int64 {
			rows, err := q.AssignDigestEntries(ctx, sqlc.AssignDigestEntriesParams{
				BatchID: mine, WorkspaceID: mustDBID(t, workspaceA), IntegrationID: mustDBID(t, integrationA), ChatID: 5, AtTime: ts(time.Now()), MaxRows: int32(max),
			})
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, r := range rows {
				ids = append(ids, r.RunID)
			}
			return ids
		}
		// Rows come back in run order (ORDER BY run_id, msg_key) only as a
		// set; sort the returned ids to compare with the expected order.
		sorted := func(max int) []int64 {
			ids := call(max)
			for i := 1; i < len(ids); i++ {
				for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
					ids[j], ids[j-1] = ids[j-1], ids[j]
				}
			}
			return ids
		}
		entryIDs := p12bIDs(t, ctx, pool, `SELECT e.id FROM behavior_digest_entries e WHERE e.chat_id=5 AND e.batch_id IS NULL AND e.due_at < now() ORDER BY e.run_id`)
		if len(entryIDs) != total {
			t.Fatalf("setup: %d qualifying entries", len(entryIDs))
		}
		run(t, pickCase{
			name:       "digest",
			call:       sorted,
			qualifying: runs[:total],
			// The lock query takes entry ids, so map run ids to them in SQL.
			lockSQL: `SELECT id FROM behavior_digest_entries WHERE run_id = ANY($1) FOR UPDATE`,
			check: func(t *testing.T, touched map[int64]bool) {
				if n := e.count(`SELECT count(*) FROM behavior_digest_entries WHERE batch_id=` + strconv.FormatInt(mine, 10)); n != total {
					t.Fatalf("assigned = %d", n)
				}
				if n := e.count(`SELECT count(*) FROM behavior_digest_entries WHERE chat_id=6 AND batch_id IS NULL`); n != 5 {
					t.Fatal("another chat's entries changed")
				}
				if n := e.count(`SELECT count(*) FROM behavior_digest_entries WHERE chat_id=5 AND due_at > now() AND batch_id IS NULL`); n != 5 {
					t.Fatal("not-yet-due entries changed")
				}
				if n := e.count(`SELECT count(*) FROM behavior_digest_entries WHERE batch_id=` + strconv.FormatInt(old, 10)); n != 5 {
					t.Fatal("already batched entries moved")
				}
			},
		})
	})
}
