package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var auditToolRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// auditTool prints a stored tool name only if it has the shape of a tool
// name; older rows may hold whatever the LLM sent.
func auditTool(v any) any {
	if s, ok := v.(string); ok && auditToolRe.MatchString(s) {
		return s
	}
	return "invalid"
}

// auditRepo prints a stored repo only if it is a valid owner/name.
func auditRepo(v any) any {
	s, ok := v.(string)
	if ok && (s == "" || tenant.ValidRepo(s)) {
		return s
	}
	return "invalid"
}

// auditFilters are applied to free-text audit columns before printing.
var auditFilters = map[string]map[string]func(any) any{
	"tool_runs": {"tool": auditTool, "repo": auditRepo},
}

// inspect prints delivery and agent state. It never selects message
// content, payloads, tool inputs/outputs, configs or ciphertext.
func inspect(ctx context.Context, pool *pgxpool.Pool, out io.Writer) error {
	enc := json.NewEncoder(out)
	sections := []struct {
		table, sql string
		cols       []string
	}{
		{"notification_outbox", `SELECT id, status, attempts, COALESCE(last_error_code,''), next_attempt_at FROM notification_outbox ORDER BY id DESC LIMIT 100`,
			[]string{"job_id", "status", "attempts", "error_code", "next_attempt_at"}},
		{"agent_jobs", `SELECT id, status, attempts, COALESCE(last_error_code,''), run_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens FROM agent_jobs ORDER BY id DESC LIMIT 100`,
			[]string{"job_id", "status", "attempts", "error_code", "run_at", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"}},
		{"tool_runs", `SELECT id, agent_job_id, step, tool, tier, COALESCE(repo,''), COALESCE(account,''), status, COALESCE(error_code,''), COALESCE(duration_ms,0) FROM tool_runs ORDER BY id DESC LIMIT 200`,
			[]string{"run_id", "job_id", "step", "tool", "tier", "repo", "account", "status", "error_code", "duration_ms"}},
		{"outbound_actions", `SELECT id, kind, status, attempts, COALESCE(last_error_code,''), COALESCE(provider_message_id,0) FROM outbound_actions ORDER BY id DESC LIMIT 100`,
			[]string{"action_id", "kind", "status", "attempts", "error_code", "provider_message_id"}},
		{"credentials", `SELECT workspace_id::text, name, kind, updated_at FROM credentials ORDER BY workspace_id, name`,
			[]string{"workspace_id", "name", "kind", "updated_at"}},
	}
	for _, sec := range sections {
		rows, err := pool.Query(ctx, sec.sql)
		if err != nil {
			return errors.New("inspect failed; check database migrations")
		}
		if err := emitRows(rows, sec.table, sec.cols, enc); err != nil {
			return err
		}
	}
	return nil
}

func emitRows(rows pgx.Rows, table string, cols []string, enc *json.Encoder) error {
	defer rows.Close()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return errors.New("inspect decode failed")
		}
		row := map[string]any{"table": table}
		for i, c := range cols {
			if t, ok := vals[i].(time.Time); ok {
				row[c] = t.UTC()
				continue
			}
			if f := auditFilters[table][c]; f != nil {
				row[c] = f(vals[i])
				continue
			}
			row[c] = vals[i]
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	if rows.Err() != nil {
		return errors.New("inspect failed")
	}
	return nil
}
