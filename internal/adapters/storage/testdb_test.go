package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	workspaceA   = "019a6690-abcd-7000-8000-000000000001"
	workspaceB   = "019a6690-abcd-7000-8000-000000000002"
	integrationA = "019a6690-abcd-7000-8000-000000000011"
	integrationB = "019a6690-abcd-7000-8000-000000000012"
	threadA      = "019a6690-abcd-7000-8000-000000000021"
	threadB      = "019a6690-abcd-7000-8000-000000000022"
	threadC      = "019a6690-abcd-7000-8000-000000000023"
	missingID    = "019a6690-abcd-7000-8000-000000000099"
)

// testDB touches only a dedicated loopback test instance, never the app DB.
// Every test owns the public schema under a cross-package advisory lock.
func testDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("integration test requires dedicated local TEST_DATABASE_URL")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 5434 || cfg.Database != "nexus_storage_test" || len(cfg.Fallbacks) != 0 {
		t.Fatal("TEST_DATABASE_URL must use loopback, database nexus_storage_test and no fallback hosts (use sslmode=disable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot connect to dedicated local test instance")
	}
	// The same key is used by cmd/setup-notices. Keep this connection alive
	// until the test pool closes; pooled connections cannot own this lock.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(762150021)"); err != nil {
		_ = admin.Close(ctx)
		t.Fatal("cannot acquire test schema lock")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = admin.Close(cleanup)
	})
	if _, err := admin.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal("cannot reset authorized test schema")
	}
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test pool configuration")
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, file, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../../../db/migrations/*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(migration), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, pool
}

func randomTestBytes(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func requirePGCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("want PostgreSQL code %s, got %v", code, err)
	}
}
