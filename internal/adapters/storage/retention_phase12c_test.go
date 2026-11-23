package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5"
)

type phase12cHookStarter struct {
	starter     retentionTxStarter
	calls       int
	beforeBegin func(int)
}

func (s *phase12cHookStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	s.calls++
	if s.beforeBegin != nil {
		s.beforeBegin(s.calls)
	}
	return s.starter.Begin(ctx)
}

// TC-C-3..11 (REQ-11): retention boundaries, protection rules, bounded draining,
// row-lock skipping, event immutability, child cleanup, and recoverable FKs.
// This function owns one database environment and therefore one schema lock.
func TestPhase12cRetentionStorageContract(t *testing.T) {
	e := newWFEnv(t)
	ctx, pool := e.ctx, e.pool
	store, err := NewRetentionStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rules := behavior.RetentionRules{MessageLogsDays: 30, ToolRunsDays: 90, PolicyDecisionsDays: 180, WorkflowDays: 90, OutboundDays: 30}
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram'),($3,$4,'telegram')`, integrationA, workspaceA, integrationB, workspaceB)

	versionA := e.enabledVersion(workspaceA, "retention-a")
	versionB := e.enabledVersion(workspaceB, "retention-b")
	finishOld := func(id int64) {
		t.Helper()
		mustExec(t, ctx, pool, `UPDATE jobs SET status='completed', updated_at=$2 WHERE execution_id=$1`, id, now.Add(-100*24*time.Hour))
		mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, id, now.Add(-100*24*time.Hour))
	}
	var eligible []int64
	for i := range 12 {
		id := p12bExec(e, workspaceA, versionA.ID, "retention:eligible:"+itoa(int64(i)))
		finishOld(id)
		eligible = append(eligible, id)
	}
	young := p12bExec(e, workspaceA, versionA.ID, "retention:young")
	mustExec(t, ctx, pool, `UPDATE jobs SET status='completed' WHERE execution_id=$1`, young)
	mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, young, now.Add(-24*time.Hour))
	live := p12bExec(e, workspaceA, versionA.ID, "retention:live")
	otherWorkspace := p12bExec(e, workspaceB, versionB.ID, "retention:other")
	finishOld(otherWorkspace)

	zeroCounts, err := store.CountRetention(ctx, workspaceA, behavior.RetentionRules{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for table, count := range zeroCounts {
		if count != 0 {
			t.Errorf("zero-day rule counted %s=%d", table, count)
		}
	}
	zeroSweep, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{}, now, 5)
	if err != nil || len(zeroSweep.Counts) != 0 {
		t.Fatalf("zero-day sweep = %+v, %v", zeroSweep, err)
	}

	counts, err := store.CountRetention(ctx, workspaceA, rules, now)
	if err != nil || counts["workflow_executions"] != 12 {
		t.Fatalf("workspace count = %v, %v", counts, err)
	}
	if global, err := store.CountRetention(ctx, "", rules, now); err != nil || global["workflow_executions"] != 13 {
		t.Fatalf("global count = %v, %v", global, err)
	}
	_, err = pool.Exec(ctx, `UPDATE execution_events SET kind='transition' WHERE execution_id=$1`, eligible[0])
	requirePGCode(t, err, "23514")
	_, err = pool.Exec(ctx, `DELETE FROM execution_events WHERE execution_id=$1`, eligible[0])
	requirePGCode(t, err, "23514")

	release := p12bLock(t, ctx, pool, `SELECT id FROM workflow_executions WHERE id=$1 FOR UPDATE`, eligible[0])
	result, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts["workflow_executions"] != 11 || e.count(`SELECT count(*) FROM workflow_executions WHERE id=`+itoa(eligible[0])) != 1 {
		t.Fatalf("locked bounded sweep = %+v", result)
	}
	release()
	result, err = store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 5)
	if err != nil || result.Counts["workflow_executions"] != 1 {
		t.Fatalf("released sweep = %+v, %v", result, err)
	}
	if e.count(`SELECT count(*) FROM execution_events WHERE execution_id = ANY($1)`, eligible) != 0 {
		t.Fatal("execution events survived workflow sweep")
	}
	if e.count(`SELECT count(*) FROM workflow_executions WHERE id IN (`+itoa(young)+`,`+itoa(live)+`,`+itoa(otherWorkspace)+`)`) != 3 {
		t.Fatal("young, live, or other-workspace execution was deleted")
	}

	// R-C-2 regression (REQ-11): one unexpected FK must not stop later eligible executions.
	blocked := p12bExec(e, workspaceA, versionA.ID, "retention:blocked")
	laterOne := p12bExec(e, workspaceA, versionA.ID, "retention:later-one")
	laterTwo := p12bExec(e, workspaceA, versionA.ID, "retention:later-two")
	for i, id := range []int64{blocked, laterOne, laterTwo} {
		finished := now.Add(time.Duration(-120+i*5) * 24 * time.Hour)
		mustExec(t, ctx, pool, `UPDATE jobs SET status='completed', updated_at=$2 WHERE execution_id=$1`, id, finished)
		mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, id, finished)
	}
	mustExec(t, ctx, pool, `CREATE TABLE retention_execution_reference(id bigint PRIMARY KEY REFERENCES workflow_executions(id))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS retention_execution_reference`) })
	mustExec(t, ctx, pool, `INSERT INTO retention_execution_reference(id) VALUES($1)`, blocked)
	blockedResult, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if blockedResult.Counts["workflow_executions"] != 2 || len(blockedResult.Issues) != 1 ||
		blockedResult.Issues[0].Table != "workflow_executions" || blockedResult.Issues[0].Code != "23503" || blockedResult.Issues[0].Count != 1 {
		t.Fatalf("recoverable workflow FK result = %+v", blockedResult)
	}
	if e.count(`SELECT count(*) FROM workflow_executions WHERE id=`+itoa(blocked)) != 1 ||
		e.count(`SELECT count(*) FROM workflow_executions WHERE id IN (`+itoa(laterOne)+`,`+itoa(laterTwo)+`)`) != 0 {
		t.Fatal("blocked execution was removed or later executions survived")
	}
	mustExec(t, ctx, pool, `DELETE FROM retention_execution_reference WHERE id=$1`, blocked)
	result, err = store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 1)
	if err != nil || result.Counts["workflow_executions"] != 1 {
		t.Fatalf("released blocked execution = %+v, %v", result, err)
	}

	parent := p12bExec(e, workspaceA, versionA.ID, "retention:parent")
	finishOld(parent)
	child := p12bExec(e, workspaceA, versionA.ID, "retention:child")
	mustExec(t, ctx, pool, `UPDATE workflow_executions SET parent_execution_id=$2 WHERE id=$1`, child, parent)
	openJob := p12bExec(e, workspaceA, versionA.ID, "retention:open-job")
	mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, openJob, now.Add(-100*24*time.Hour))
	if protected, err := store.CountRetention(ctx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now); err != nil || protected["workflow_executions"] != 0 {
		t.Fatalf("protected executions counted = %v, %v", protected, err)
	}

	toolOwner := p12bExec(e, workspaceA, versionA.ID, "retention:tool-owner")
	finishOld(toolOwner)
	var toolID int64
	if err := pool.QueryRow(ctx, `INSERT INTO tool_runs(workspace_id,workflow_execution_id,step,tool,tier,idempotency_key,status,created_at)
		VALUES($1,$2,1,'local_test','read','retention-tool','ok',$3) RETURNING id`, workspaceA, toolOwner, now.Add(-200*24*time.Hour)).Scan(&toolID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO policy_decisions(workspace_id,action,tier,effect,reason,actor_kind,tool_run_id,created_at)
		VALUES($1,'github.comment','write','allow','default_allow','workflow',$2,$3)`, workspaceA, toolID, now.Add(-200*24*time.Hour))
	toolOnly, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{ToolRunsDays: 90}, now, 5)
	if err != nil || toolOnly.Counts["tool_runs"] != 0 {
		t.Fatalf("referenced tool run deleted: %+v, %v", toolOnly, err)
	}
	policyOnly, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{PolicyDecisionsDays: 180}, now, 5)
	if err != nil || policyOnly.Counts["policy_decisions"] != 1 {
		t.Fatalf("policy window = %+v, %v", policyOnly, err)
	}
	toolOnly, err = store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{ToolRunsDays: 90}, now, 5)
	if err != nil || toolOnly.Counts["tool_runs"] != 1 {
		t.Fatalf("tool window = %+v, %v", toolOnly, err)
	}

	mustExec(t, ctx, pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES
		($1,$2,$3,'task','eligible'),($4,$2,$3,'task','summary'),($5,$2,$3,'task','outbox')`, threadA, workspaceA, integrationA, threadB, threadC)
	var eligibleMessage, summaryMessage, outboxMessage int64
	for _, row := range []struct {
		thread string
		id     *int64
	}{{threadA, &eligibleMessage}, {threadB, &summaryMessage}, {threadC, &outboxMessage}} {
		if err := pool.QueryRow(ctx, `INSERT INTO message_logs(thread_id,sender_role,content,created_at) VALUES($1,'system','redacted',$2) RETURNING id`, row.thread, now.Add(-40*24*time.Hour)).Scan(row.id); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, ctx, pool, `UPDATE hub_threads SET summary_through_message_id=$2 WHERE id=$1`, threadB, summaryMessage)
	var outboxID int64
	if err := pool.QueryRow(ctx, `INSERT INTO notification_outbox(workspace_id,source_integration_id,target_integration_id,event_key,event,message_id,status,updated_at)
		VALUES($1,$2,$2,'retention-event','{}',$3,'sent',$4) RETURNING id`, workspaceA, integrationA, outboxMessage, now.Add(-40*24*time.Hour)).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key,status,updated_at)
		VALUES($1,$2,'telegram_message','{}','retention-outbound','sent',$3)`, workspaceA, integrationA, now.Add(-40*24*time.Hour))
	mustExec(t, ctx, pool, `CREATE TABLE retention_test_reference(id bigint PRIMARY KEY REFERENCES notification_outbox(id))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS retention_test_reference`) })
	mustExec(t, ctx, pool, `INSERT INTO retention_test_reference(id) VALUES($1)`, outboxID)
	fkResult, err := store.SweepRetention(ctx, workspaceA, behavior.RetentionRules{MessageLogsDays: 30, OutboundDays: 30}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(fkResult.Issues) != 1 || fkResult.Issues[0].Table != "notification_outbox" ||
		fkResult.Issues[0].Code != "23503" || fkResult.Issues[0].Count != 1 {
		t.Fatalf("FK issue = %+v", fkResult.Issues)
	}
	if fkResult.Counts["outbound_actions"] != 1 || fkResult.Counts["message_logs"] != 1 {
		t.Fatalf("later groups did not continue: %+v", fkResult.Counts)
	}
	if e.count(`SELECT count(*) FROM message_logs WHERE id IN (`+itoa(summaryMessage)+`,`+itoa(outboxMessage)+`)`) != 2 {
		t.Fatal("summary or outbox-referenced message was deleted")
	}
}

// TC-C-FIX-1 (REQ-11): an execution protected after a failed batch is re-checked before retry.
func TestPhase12cRetentionRetryRepicksWorkflowEligibility(t *testing.T) {
	e := newWFEnv(t)
	ctx, pool := e.ctx, e.pool
	store, err := NewRetentionStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	version := e.enabledVersion(workspaceA, "retention-repick")
	target := p12bExec(e, workspaceA, version.ID, "retention:repick-target")
	blocked := p12bExec(e, workspaceA, version.ID, "retention:repick-blocked")
	deletable := p12bExec(e, workspaceA, version.ID, "retention:repick-deletable")
	for i, id := range []int64{target, blocked, deletable} {
		finished := now.Add(time.Duration(-130+i*10) * 24 * time.Hour)
		mustExec(t, ctx, pool, `UPDATE jobs SET status='completed', updated_at=$2 WHERE execution_id=$1`, id, finished)
		mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, id, finished)
	}
	mustExec(t, ctx, pool, `CREATE TABLE retention_repick_reference(id bigint PRIMARY KEY REFERENCES workflow_executions(id))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS retention_repick_reference`) })
	mustExec(t, ctx, pool, `INSERT INTO retention_repick_reference(id) VALUES($1)`, blocked)

	starter := &phase12cHookStarter{starter: pool}
	starter.beforeBegin = func(call int) {
		if call != 2 {
			return
		}
		mustExec(t, ctx, pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id)
			VALUES($1,'policy_gate','retention-repick-gate-0001',$2,'github.comment','retry-protection','',$3)`, workspaceA, now.Add(time.Hour), target)
	}
	result, err := store.sweepRetention(ctx, starter, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts["workflow_executions"] != 1 || len(result.Issues) != 1 ||
		result.Issues[0].Table != "workflow_executions" || result.Issues[0].Code != "23503" || result.Issues[0].Count != 1 {
		t.Fatalf("repick result = %+v", result)
	}
	if e.count(`SELECT count(*) FROM workflow_executions WHERE id IN (`+itoa(target)+`,`+itoa(blocked)+`)`) != 2 ||
		e.count(`SELECT count(*) FROM workflow_executions WHERE id=`+itoa(deletable)) != 0 {
		t.Fatal("retry deleted newly protected execution or kept eligible execution")
	}
}

// TC-C-FIX-2 (REQ-11): FK-blocked rows are isolated per group and do not starve later rows or groups.
func TestPhase12cRetentionForeignKeyIsolationAcrossGroups(t *testing.T) {
	e := newWFEnv(t)
	ctx, pool := e.ctx, e.pool
	store, err := NewRetentionStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integrationA, workspaceA)

	var outboxIDs []int64
	for i := range 3 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO notification_outbox(workspace_id,source_integration_id,target_integration_id,event_key,event,status,updated_at)
			VALUES($1,$2,$2,$3,'{}','sent',$4) RETURNING id`, workspaceA, integrationA, "retention-fk-outbox-"+itoa(int64(i)), now.Add(-40*24*time.Hour)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		outboxIDs = append(outboxIDs, id)
	}
	mustExec(t, ctx, pool, `INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key,status,updated_at)
		VALUES($1,$2,'telegram_message','{}','retention-fk-later-group','sent',$3)`, workspaceA, integrationA, now.Add(-40*24*time.Hour))

	mustExec(t, ctx, pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'task','retention-fk-messages')`, threadA, workspaceA, integrationA)
	var messageIDs []int64
	for range 3 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO message_logs(thread_id,sender_role,content,created_at) VALUES($1,'system','redacted',$2) RETURNING id`, threadA, now.Add(-40*24*time.Hour)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		messageIDs = append(messageIDs, id)
	}

	version := e.enabledVersion(workspaceA, "retention-fk-groups")
	var toolIDs []int64
	for i := range 3 {
		executionID := p12bExec(e, workspaceA, version.ID, "retention:fk-tool:"+itoa(int64(i)))
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO tool_runs(workspace_id,workflow_execution_id,step,tool,tier,idempotency_key,status,created_at)
			VALUES($1,$2,$3,'local_test','read',$4,'ok',$5) RETURNING id`, workspaceA, executionID, i+1, "retention-fk-tool-"+itoa(int64(i)), now.Add(-100*24*time.Hour)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		toolIDs = append(toolIDs, id)
	}

	mustExec(t, ctx, pool, `CREATE TABLE retention_notification_reference(id bigint PRIMARY KEY REFERENCES notification_outbox(id))`)
	mustExec(t, ctx, pool, `CREATE TABLE retention_message_reference(id bigint PRIMARY KEY REFERENCES message_logs(id))`)
	mustExec(t, ctx, pool, `CREATE TABLE retention_tool_reference(id bigint PRIMARY KEY REFERENCES tool_runs(id))`)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS retention_notification_reference, retention_message_reference, retention_tool_reference`)
	})
	mustExec(t, ctx, pool, `INSERT INTO retention_notification_reference(id) VALUES($1)`, outboxIDs[0])
	mustExec(t, ctx, pool, `INSERT INTO retention_message_reference(id) VALUES($1)`, messageIDs[0])
	mustExec(t, ctx, pool, `INSERT INTO retention_tool_reference(id) VALUES($1)`, toolIDs[0])

	rules := behavior.RetentionRules{MessageLogsDays: 30, ToolRunsDays: 90, OutboundDays: 30}
	result, err := store.SweepRetention(ctx, workspaceA, rules, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"notification_outbox": 2, "outbound_actions": 1, "message_logs": 2, "tool_runs": 2} {
		if result.Counts[table] != want {
			t.Errorf("%s count = %d, want %d; result=%+v", table, result.Counts[table], want, result)
		}
	}
	issues := map[string]behavior.RetentionIssue{}
	for _, issue := range result.Issues {
		issues[issue.Table] = issue
	}
	for _, table := range []string{"notification_outbox", "message_logs", "tool_runs"} {
		if issue := issues[table]; issue.Code != "23503" || issue.Count != 1 {
			t.Errorf("%s issue = %+v", table, issue)
		}
	}

	secondCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	second, err := store.SweepRetention(secondCtx, workspaceA, rules, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Issues) != 3 {
		t.Fatalf("second pass issues = %+v", second)
	}
	for table, count := range second.Counts {
		if count != 0 {
			t.Errorf("second pass deleted %s=%d", table, count)
		}
	}
}

// TC-C-FIX-3 (REQ-11): the 1000-row skip budget is shared across retention groups.
func TestPhase12cRetentionSharedSkipLimit(t *testing.T) {
	result := behavior.RetentionResult{Counts: map[string]int64{}}
	skipped := 0
	blockedBatch := func(total int64) retentionBatchFn {
		return func(_ context.Context, _ int32, skipIDs []int64, onlyID *int64) (map[string]int64, *behavior.RetentionIssue, []int64, error) {
			if onlyID != nil {
				return nil, &behavior.RetentionIssue{Code: "23503"}, []int64{*onlyID}, nil
			}
			if len(skipIDs) > 0 {
				return map[string]int64{}, nil, nil, nil
			}
			ids := make([]int64, total)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			return nil, &behavior.RetentionIssue{Code: "23503"}, ids, nil
		}
	}
	stop, err := runRetentionGroup(context.Background(), &result, "notification_outbox", 600, 0, &skipped, blockedBatch(600))
	if err != nil || stop {
		t.Fatalf("first group stop=%t err=%v result=%+v", stop, err, result)
	}
	stop, err = runRetentionGroup(context.Background(), &result, "message_logs", 401, 0, &skipped, blockedBatch(401))
	if err != nil || !stop || skipped != 1000 {
		t.Fatalf("shared limit stop=%t skipped=%d err=%v result=%+v", stop, skipped, err, result)
	}
	var limit, blocked int64
	for _, issue := range result.Issues {
		if issue.Code == "skip_limit" {
			limit += issue.Count
		} else if issue.Code == "23503" {
			blocked += issue.Count
		}
	}
	if limit != 1 || blocked != 1000 {
		t.Fatalf("issues = %+v", result.Issues)
	}
}

// TC-C-FIX-4 (REQ-11): dry-run rolls back, reports exact or partial counts, and fails busy instead of hanging.
func TestPhase12cRetentionDryRunBoundsAndBusyLock(t *testing.T) {
	e := newWFEnv(t)
	ctx, pool := e.ctx, e.pool
	store, err := NewRetentionStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integrationA, workspaceA)
	insertOutbound := func(prefix string, count int) {
		t.Helper()
		for i := range count {
			mustExec(t, ctx, pool, `INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key,status,updated_at)
				VALUES($1,$2,'telegram_message','{}',$3,'sent',$4)`, workspaceA, integrationA, prefix+itoa(int64(i)), now.Add(-40*24*time.Hour))
		}
	}
	rules := behavior.RetentionRules{OutboundDays: 30}
	insertOutbound("retention-dry-exact-", 3)
	dry, err := store.SweepRetentionDryRun(ctx, workspaceA, rules, now, 1)
	if err != nil || dry.Partial || dry.Counts["outbound_actions"] != 3 {
		t.Fatalf("exact dry-run = %+v, %v", dry, err)
	}
	if e.count(`SELECT count(*) FROM outbound_actions`) != 3 {
		t.Fatal("exact dry-run deleted rows")
	}
	real, err := store.SweepRetention(ctx, workspaceA, rules, now, 1)
	if err != nil || real.Counts["outbound_actions"] != dry.Counts["outbound_actions"] {
		t.Fatalf("real=%+v dry=%+v err=%v", real, dry, err)
	}

	insertOutbound("retention-dry-partial-", 6)
	partial, err := store.SweepRetentionDryRun(ctx, workspaceA, rules, now, 1)
	if err != nil || !partial.Partial || partial.Counts["outbound_actions"] != 5 {
		t.Fatalf("partial dry-run = %+v, %v", partial, err)
	}
	if e.count(`SELECT count(*) FROM outbound_actions`) != 6 {
		t.Fatal("partial dry-run deleted rows")
	}
	if _, err := store.SweepRetention(ctx, workspaceA, rules, now, 1); err != nil {
		t.Fatal(err)
	}

	version := e.enabledVersion(workspaceA, "retention-dry-busy")
	executionID := p12bExec(e, workspaceA, version.ID, "retention:dry-busy")
	mustExec(t, ctx, pool, `UPDATE jobs SET status='completed', updated_at=$2 WHERE execution_id=$1`, executionID, now.Add(-100*24*time.Hour))
	mustExec(t, ctx, pool, `UPDATE workflow_executions SET state='COMPLETED', finished_at=$2 WHERE id=$1`, executionID, now.Add(-100*24*time.Hour))
	release := p12bLock(t, ctx, pool, `SELECT id FROM execution_events WHERE execution_id=$1 ORDER BY id LIMIT 1 FOR UPDATE`, executionID)
	defer release()
	busyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = store.SweepRetentionDryRun(busyCtx, workspaceA, behavior.RetentionRules{WorkflowDays: 90}, now, 5)
	if !errors.Is(err, ErrRetentionBusy) {
		t.Fatalf("locked dry-run error = %v, want %v", err, ErrRetentionBusy)
	}
}
