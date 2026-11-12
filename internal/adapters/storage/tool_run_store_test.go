package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newToolRunDB(t *testing.T) (context.Context, *pgxpool.Pool, *ToolRunStore, int64) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'clickup')`, integrationA, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'task','t1')`, threadA, workspaceA, integrationA)
	var job int64
	if err := pool.QueryRow(ctx, `INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at) VALUES($1,$2,$3,now()) RETURNING id`, workspaceA, integrationA, threadA).Scan(&job); err != nil {
		t.Fatal(err)
	}
	s, err := NewToolRunStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	var _ tools.ToolRunStore = s
	return ctx, pool, s, job
}

func start(job int64, tool string, tier tenant.Tier, key string) tools.RunStart {
	return tools.RunStart{WorkspaceID: workspaceA, JobID: job, Step: 1, Tool: tool, Tier: tier, Repo: "o/r", Account: "personal", Key: key, InputRedacted: json.RawMessage(`"{\"n\":1}"`)}
}

func TestToolRunStore_WriteIdempotency(t *testing.T) {
	ctx, _, s, job := newToolRunDB(t)
	id, prior, err := s.Begin(ctx, start(job, "github_pr_comment", tenant.TierWrite, "k1"))
	if err != nil || prior != nil || id == 0 {
		t.Fatalf("%d %+v %v", id, prior, err)
	}
	// While started, a repeat is ambiguous.
	if _, prior, err := s.Begin(ctx, start(job, "github_pr_comment", tenant.TierWrite, "k1")); err != nil || prior == nil || prior.Status != tools.StatusUnknownOutcome {
		t.Fatalf("started: %+v %v", prior, err)
	}
	if err := s.Finish(ctx, tools.RunFinish{RunID: id, Status: tools.StatusOK, DurationMS: 5, OutputBytes: 10, Summary: "commented #1"}); err != nil {
		t.Fatal(err)
	}
	if _, prior, err := s.Begin(ctx, start(job, "github_pr_comment", tenant.TierWrite, "k1")); err != nil || prior == nil || prior.Status != tools.StatusOK || prior.Summary != "commented #1" {
		t.Fatalf("ok replay: %+v %v", prior, err)
	}
	// timeout and unknown_outcome block re-execution; error does not.
	for _, st := range []string{tools.StatusUnknownOutcome, tools.StatusTimeout} {
		key := "k-" + st
		id, _, _ := s.Begin(ctx, start(job, "git_push", tenant.TierWrite, key))
		_ = s.Finish(ctx, tools.RunFinish{RunID: id, Status: st, ErrorCode: st})
		if _, prior, _ := s.Begin(ctx, start(job, "git_push", tenant.TierWrite, key)); prior == nil || prior.Status != tools.StatusUnknownOutcome {
			t.Fatalf("%s: %+v", st, prior)
		}
	}
	id, _, _ = s.Begin(ctx, start(job, "git_push", tenant.TierWrite, "k-err"))
	_ = s.Finish(ctx, tools.RunFinish{RunID: id, Status: tools.StatusError, ErrorCode: "conflict"})
	if _, prior, err := s.Begin(ctx, start(job, "git_push", tenant.TierWrite, "k-err")); err != nil || prior != nil {
		t.Fatalf("error must allow retry: %+v %v", prior, err)
	}
}

func TestToolRunStore_ReadsNotDeduped(t *testing.T) {
	ctx, pool, s, job := newToolRunDB(t)
	for i := 0; i < 3; i++ {
		id, prior, err := s.Begin(ctx, start(job, "code_search", tenant.TierRead, "same"))
		if err != nil || prior != nil {
			t.Fatalf("%+v %v", prior, err)
		}
		_ = s.Finish(ctx, tools.RunFinish{RunID: id, Status: tools.StatusOK})
	}
	if n := countRows(t, ctx, pool, "tool_runs"); n != 3 {
		t.Fatalf("rows %d", n)
	}
}

func TestToolRunStore_FinishDenyDryRun(t *testing.T) {
	ctx, pool, s, job := newToolRunDB(t)
	if err := s.Deny(ctx, start(job, "github_pr_merge", tenant.TierDangerous, "kd"), "dangerous_disabled"); err != nil {
		t.Fatal(err)
	}
	var st, code string
	if err := pool.QueryRow(ctx, `SELECT status, error_code FROM tool_runs WHERE idempotency_key='kd'`).Scan(&st, &code); err != nil || st != "denied" || code != "dangerous_disabled" {
		t.Fatalf("%s %s %v", st, code, err)
	}
	// A denial never blocks a later allowed attempt.
	if _, prior, err := s.Begin(ctx, start(job, "github_pr_merge", tenant.TierDangerous, "kd")); err != nil || prior != nil {
		t.Fatalf("%+v %v", prior, err)
	}
	id, _, _ := s.Begin(ctx, start(job, "github_pr_comment", tenant.TierWrite, "kdry"))
	if err := s.Finish(ctx, tools.RunFinish{RunID: id, Status: tools.StatusDryRun, Summary: "DRY RUN"}); err != nil {
		t.Fatal(err)
	}
	if _, prior, _ := s.Begin(ctx, start(job, "github_pr_comment", tenant.TierWrite, "kdry")); prior != nil {
		t.Fatal("dry run must not dedupe a real run")
	}
	// Finish only applies to started rows.
	if err := s.Finish(ctx, tools.RunFinish{RunID: id, Status: tools.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM tool_runs WHERE id=$1`, id).Scan(&st); err != nil || st != "dry_run" {
		t.Fatalf("finished row overwritten: %s", st)
	}
	var input string
	if err := pool.QueryRow(ctx, `SELECT input_redacted::text FROM tool_runs WHERE id=$1`, id).Scan(&input); err != nil || input == "" {
		t.Fatalf("input %q %v", input, err)
	}
}
