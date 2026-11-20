package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 12 B1 (REQ-9): `nh status` and `nh execution timeline`.

const p12bOtherWS = "019a6690-abcd-7000-8000-0000000000b2"

// p12bFields parses "  name   value" lines of the status report.
func p12bFields(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(line, "  ") {
			m[f[0]] = f[1]
		}
	}
	return m
}

var p12bTimeLine = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ  \S`)

// TC-B-13, TC-B-14, TC-B-15, TC-B-16, TC-B-17, TC-B-18 (REQ-9). One database
// for all subtests.
func TestPhase12bStatusAndTimeline(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ctx := f.ctx
	ops, err := storage.NewOpsStore(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Other')`, p12bOtherWS); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Seed: q1 and q2 queued (q1 due 10 minutes ago), failed1 failed and
	// alerted, running1; a worker device; LLM usage today.
	q1 := f.start(t, "p12b:q1", workflow.References{}, "s1", "s2")
	q2 := f.start(t, "p12b:q2", workflow.References{}, "s1")
	failed1 := f.start(t, "p12b:f1", workflow.References{}, "s1")
	running1 := f.start(t, "p12b:r1", workflow.References{}, "s1")
	exec(`UPDATE jobs SET due_at=now()-interval '10 minutes' WHERE execution_id=$1`, q1.ID)
	exec(`UPDATE jobs SET due_at=now()+interval '1 hour' WHERE execution_id=$1`, q2.ID)
	exec(`UPDATE jobs SET status='completed' WHERE execution_id IN ($1,$2)`, failed1.ID, running1.ID)
	exec(`UPDATE workflow_executions SET state='FAILED', finished_at=now(), reason_code='attempts_exhausted' WHERE id=$1`, failed1.ID)
	exec(`UPDATE workflow_executions SET state='RUNNING' WHERE id=$1`, running1.ID)
	exec(`INSERT INTO worker_devices(workspace_id,name,token_hash,state,last_seen_at) VALUES($1,'laptop',sha256('p12b'),'online',now()-interval '20 seconds')`, f.ws)
	today := time.Now().UTC()
	for _, r := range []domain.LLMUsageRecord{
		{WorkspaceID: f.ws, Day: today, Provider: "anthropic", Model: "claude-sonnet-5-5", InputTokens: 1200, OutputTokens: 300, CostUSD: 0.75},
		{WorkspaceID: f.ws, Day: today, Provider: "claude_cli", Model: "claude-sonnet-5-5", InputTokens: 40, OutputTokens: 4},
	} {
		if err := ops.AddLLMUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := ops.ClaimAlert(ctx, behavior.WorkflowAlert{WorkspaceID: f.ws, ExecutionID: failed1.ID, State: behavior.AlertFailed}, time.Now()); err != nil || !ok {
		t.Fatalf("claim alert: %v %v", ok, err)
	}

	t.Run("status hub-wide numbers", func(t *testing.T) {
		out := f.must("status")
		for _, want := range []string{"scope: hub", "executions:", "queue:", "workers:", "alerts:", "llm today ("} {
			if !strings.Contains(out, want) {
				t.Errorf("status misses %q:\n%s", want, out)
			}
		}
		m := p12bFields(out)
		if m["QUEUED"] != "2" || m["FAILED"] != "1" || m["RUNNING"] != "1" || m["COMPLETED"] != "0" {
			t.Errorf("execution counts wrong: %v", m)
		}
		// Every state of the closed list is printed, in order.
		last := -1
		for _, st := range []string{"RECEIVED", "TRIAGING", "PLANNING", "QUEUED", "RUNNING", "EVALUATING", "WAITING_FOR_USER", "WAITING_FOR_APPROVAL", "WAITING_FOR_WORKER", "COMPLETED", "FAILED", "CANCELLED"} {
			i := strings.Index(out, "  "+st+" ")
			if i < 0 || i < last {
				t.Fatalf("state %s missing or out of order:\n%s", st, out)
			}
			last = i
		}
		if m["pending_jobs"] != "2" {
			t.Errorf("pending_jobs = %q (q1 and q2)", m["pending_jobs"])
		}
		if age, _ := strconv.Atoi(m["oldest_due_age_s"]); age < 590 || age > 700 {
			t.Errorf("oldest_due_age_s = %q, want about 600", m["oldest_due_age_s"])
		}
		if m["devices"] != "1" || m["online"] != "1" {
			t.Errorf("workers wrong: %v", m)
		}
		if age, _ := strconv.Atoi(m["newest_heartbeat_age_s"]); age < 15 || age > 60 {
			t.Errorf("heartbeat age = %q", m["newest_heartbeat_age_s"])
		}
		if m["open"] != "1" {
			t.Errorf("open alerts = %q, want 1", m["open"])
		}
		if !strings.Contains(out, "anthropic claude-sonnet-5-5 input_tokens=1200 output_tokens=300 cost_usd=0.7500") ||
			!strings.Contains(out, "claude_cli claude-sonnet-5-5 input_tokens=40 output_tokens=4 cost_usd=0.0000") ||
			!strings.Contains(out, "total_cost_usd=0.7500") || !strings.Contains(out, "daily_budget_usd=off") {
			t.Errorf("llm lines wrong:\n%s", out)
		}
		if !strings.Contains(out, "("+today.Format("2006-01-02")+", UTC") {
			t.Errorf("llm day line wrong:\n%s", out)
		}
		// A budget shows up once set.
		f.must("settings", "set", "llm_daily_budget_usd", "2")
		if budget := f.must("status"); !strings.Contains(budget, "daily_budget_usd=2.0000 (per workspace)") {
			t.Errorf("budget line:\n%s", budget)
		}
	})

	t.Run("status workspace scope excludes other workspaces", func(t *testing.T) {
		mine := f.must("status", "--workspace-id", f.ws)
		if !strings.Contains(mine, "scope: workspace "+f.ws) || p12bFields(mine)["QUEUED"] != "2" {
			t.Errorf("own scope:\n%s", mine)
		}
		other := f.must("status", "--workspace-id", p12bOtherWS)
		m := p12bFields(other)
		if m["QUEUED"] != "0" || m["pending_jobs"] != "0" || m["devices"] != "0" || m["open"] != "0" || !strings.Contains(other, "  none") {
			t.Errorf("another workspace sees data:\n%s", other)
		}
		if !strings.Contains(other, "newest_heartbeat_age_s none") {
			t.Errorf("no heartbeat must read none:\n%s", other)
		}
	})

	t.Run("status errors are short and safe", func(t *testing.T) {
		for _, args := range [][]string{
			{"status", "--workspace-id", "not-a-uuid"},
			{"status", "extra"},
			{"status", "--workspace-id"},
			{"status", "--bogus"},
		} {
			out, err := f.run(args...)
			if err == nil || !strings.HasPrefix(err.Error(), "usage: status") {
				t.Errorf("%v: err=%v out=%q", args, err, out)
			}
		}
		_, err := f.run("status", "--workspace-id", "019a6690-abcd-7000-8000-0000000000ff")
		if err == nil || err.Error() != "workspace not found" {
			t.Errorf("unknown workspace: %v", err)
		}
	})

	t.Run("timeline shows order, retries, waits and codes only", func(t *testing.T) {
		tl := f.start(t, "p12b:timeline", workflow.References{}, "s1", "s2")
		exec(`UPDATE jobs SET attempts=3, max_attempts=5, last_error_code='step_timeout', updated_at=now() WHERE execution_id=$1`, tl.ID)
		var approval int64
		if err := f.pool.QueryRow(ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,decision,decided_at,decided_by) VALUES($1,'confirm_action','p12b-timeline-nonce-1',now()+interval '1 hour','approved',now(),'owner') RETURNING id`, f.ws).Scan(&approval); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO workflow_waits(workspace_id,execution_id,step_key,approval_id,wait_kind) VALUES($1,$2,'s1',$3,'approval')`, f.ws, tl.ID, approval)
		out := f.must("execution", "timeline", strconv.FormatInt(tl.ID, 10), "--workspace-id", f.ws)
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if !strings.HasPrefix(lines[0], fmt.Sprintf("execution WF-%d state=", tl.ID)) {
			t.Fatalf("header = %q", lines[0])
		}
		var times []time.Time
		for _, l := range lines[1:] {
			if !p12bTimeLine.MatchString(l) {
				t.Errorf("not a dated one-line entry: %q", l)
				continue
			}
			ts, err := time.Parse(time.RFC3339, l[:20])
			if err != nil {
				t.Fatal(err)
			}
			times = append(times, ts)
		}
		for i := 1; i < len(times); i++ {
			if times[i].Before(times[i-1]) {
				t.Errorf("timeline out of order at line %d:\n%s", i+1, out)
			}
		}
		for _, want := range []string{
			"execution created state=", "event ", "job job=", "queued step=#0(model)", "status=pending", "attempts=3/5", "retries=2", "last_error=step_timeout",
			"wait wait=", "opened kind=approval step=#0(model)", fmt.Sprintf("approval=%d", approval), "decided decision=approved", "step #0(model) state=", "step #1(notify) state=",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("timeline misses %q:\n%s", want, out)
			}
		}
		// The WF-<n> form and the bare number give the same report.
		if wf := f.must("execution", "timeline", fmt.Sprintf("WF-%d", tl.ID), "--workspace-id", f.ws); wf != out {
			t.Errorf("WF-n form differs:\n%s\nvs\n%s", wf, out)
		}
		// An execution without waits prints no wait lines.
		plain := f.must("execution", "timeline", strconv.FormatInt(q2.ID, 10), "--workspace-id", f.ws)
		if strings.Contains(plain, "wait wait=") {
			t.Errorf("wait lines without waits:\n%s", plain)
		}
	})

	t.Run("timeline never prints keys, subjects or secrets", func(t *testing.T) {
		secretKey := "step-sk-ant-api03-SECRETKEYVALUE"
		ex := f.start(t, "p12b:secret-trigger-ghp_TOKEN123", workflow.References{
			SourceKind: "clickup.comment", SourceID: "comment-ghp_SOURCETOKEN",
			Subject: "Tìm lỗi đăng nhập\nINJECTED LINE password=hunter2", OperationKey: "op:secret",
		}, secretKey, "second-step-xyz")
		exec(`UPDATE jobs SET last_error_code='Bad Code With Spaces!' WHERE execution_id=$1`, ex.ID)
		out := f.must("execution", "timeline", strconv.FormatInt(ex.ID, 10), "--workspace-id", f.ws)
		all := out + f.must("status") + f.must("status", "--workspace-id", f.ws)
		for _, bad := range []string{"sk-ant", "SECRETKEYVALUE", "ghp_", "hunter2", "INJECTED", "Tìm lỗi", "second-step-xyz", "Authorization", "Bearer", "password"} {
			if strings.Contains(all, bad) {
				t.Errorf("output leaks %q:\n%s", bad, out)
			}
		}
		if strings.Contains(out, "Bad Code") || !strings.Contains(out, "last_error=unknown") {
			t.Errorf("unsafe error code must print as unknown:\n%s", out)
		}
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n")[1:] {
			if !p12bTimeLine.MatchString(l) {
				t.Errorf("line breaks the one-line format: %q", l)
			}
		}
	})

	t.Run("timeline input errors and tenant isolation", func(t *testing.T) {
		usage := "usage: execution timeline"
		for _, args := range [][]string{
			{"execution"},
			{"execution", "history", "1", "--workspace-id", f.ws},
			{"execution", "timeline", "1"},
			{"execution", "timeline", "--workspace-id", f.ws},
			{"execution", "timeline", "1", "2", "--workspace-id", f.ws},
			{"execution", "timeline", "1", "--workspace-id", "bad"},
		} {
			_, err := f.run(args...)
			if err == nil || !strings.HasPrefix(err.Error(), usage) {
				t.Errorf("%v: %v", args, err)
			}
		}
		for _, ref := range []string{"abc", "WF-0", "0", "WF-x", "1.5"} {
			_, err := f.run("execution", "timeline", ref, "--workspace-id", f.ws)
			if err == nil || !strings.HasPrefix(err.Error(), "execution reference must be") {
				t.Errorf("ref %q: %v", ref, err)
			}
		}
		// Another workspace's execution is not found, and says nothing else.
		_, err := f.run("execution", "timeline", strconv.FormatInt(q1.ID, 10), "--workspace-id", p12bOtherWS)
		if err == nil || !strings.HasPrefix(err.Error(), "not_found:") {
			t.Errorf("cross-workspace read: %v", err)
		}
		if _, err := f.run("execution", "timeline", "999999", "--workspace-id", f.ws); err == nil || !strings.HasPrefix(err.Error(), "not_found:") {
			t.Errorf("unknown id: %v", err)
		}
		if _, err := f.run("execution", "timeline", "1", "--workspace-id", "019a6690-abcd-7000-8000-0000000000ff"); err == nil || err.Error() != "workspace not found" {
			t.Errorf("unknown workspace: %v", err)
		}
	})

	var _ context.Context = ctx
}
