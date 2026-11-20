package storage

import (
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

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
