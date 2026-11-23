package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TC-27 (REQ-13): exercise run(), not just buildTools. Settings and dirs are
// in the authorized test DB; the real child has only the two app env variables
// plus PATH for git. Missing master key must warn but still serve HTTP.
func TestApproved_RealServerTwoEnvStartupAndMissingKeyWarning(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil || url == "" {
		t.Fatal("dedicated TEST_DATABASE_URL required")
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Port != 5434 || cfg.ConnConfig.Database != "nexus_storage_test" || len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatal("unsafe test database target")
	}
	bin := filepath.Join(t.TempDir(), "server")
	build := exec.Command("go", "build", "-o", bin, ".")
	if _, err := build.CombinedOutput(); err != nil {
		t.Fatal("server build failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal("test DB connection failed")
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(762150021)`); err != nil {
		t.Fatal("cannot acquire schema lock")
	}
	if _, err := admin.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal("cannot reset authorized schema")
	}
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	if len(files) == 0 {
		t.Fatal("missing migrations")
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := admin.Exec(ctx, up); err != nil {
			t.Fatal("migration failed")
		}
	}
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "key_present", true: "missing_key"}[missing], func(t *testing.T) {
			base := t.TempDir()
			key := filepath.Join(base, "master.key")
			if !missing {
				if err := os.WriteFile(key, bytes.Repeat([]byte{7}, 32), 0600); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			listener.Close()
			for name, value := range map[string]string{"listen_addr": addr, "repos_dir": filepath.Join(base, "repos"), "tmp_dir": filepath.Join(base, "tmp")} {
				if _, err := admin.Exec(ctx, `INSERT INTO app_settings(key,value) VALUES($1,to_jsonb($2::text)) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, name, value); err != nil {
					t.Fatal("cannot seed server settings")
				}
			}
			var output bytes.Buffer
			cmd := exec.Command(bin)
			cmd.Env = []string{"DATABASE_URL=" + url, "NEXUS_MASTER_KEY_FILE=" + key, "PATH=" + os.Getenv("PATH")}
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal("server child failed to start")
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			joined := false
			defer func() {
				if !joined {
					_ = cmd.Process.Kill()
					<-done
				}
			}()
			client := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
			defer client.CloseIdleConnections()
			ready := false
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := client.Get("http://" + addr + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						ready = true
						break
					}
				}
				select {
				case <-done:
					joined = true
					t.Fatal("server exited before HTTP readiness")
				default:
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("real server did not start HTTP")
			}
			// TC-F-9/10 (REQ-4): health is public for GET and HEAD, but readiness
			// details are absent from the public listener.
			head, err := client.Head("http://" + addr + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			head.Body.Close()
			if head.StatusCode != http.StatusOK {
				t.Fatalf("HEAD /healthz status=%d", head.StatusCode)
			}
			private, err := client.Get("http://" + addr + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			private.Body.Close()
			if private.StatusCode != http.StatusNotFound {
				t.Fatalf("public /readyz status=%d", private.StatusCode)
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Error("SIGTERM shutdown failed")
				}
			case <-time.After(12 * time.Second):
				t.Fatal("server failed to await workers on shutdown")
			}
			if strings.Contains(output.String(), url) {
				t.Error("startup printed DB credentials")
			}
			warn := strings.Contains(output.String(), `"check":"master_key"`)
			if warn != missing {
				t.Error("missing-master-key warning does not match real startup state")
			}
			// TC-F-3/15 (REQ-1, REQ-5): real startup, request, summary and
			// shutdown records all use one JSON envelope.
			summary := false
			started := 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Errorf("non-JSON server lifecycle line %q: %v", line, err)
					continue
				}
				if record["msg"] == "readiness summary" {
					summary = true
				}
				if record["msg"] == "http server started" {
					started++
					if record["component"] != "http" || record["addr"] != addr {
						t.Errorf("HTTP start record shape: %#v", record)
					}
				}
			}
			if !summary {
				t.Error("startup readiness summary absent")
			}
			if started != 1 {
				t.Errorf("HTTP start records=%d output=%s", started, output.String())
			}
		})
	}
}
