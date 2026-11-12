package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
