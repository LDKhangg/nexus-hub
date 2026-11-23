package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/obs"
)

// TC-F-2 (REQ-1): operational logs go to the configured log writer while
// command output remains on the caller's stdout writer.
func TestPhaseFCommandOutputRemainsOnStdout(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	var stderr, stdout bytes.Buffer
	obs.Setup("info", &stderr)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := run([]string{"master-key", "init", "--path", path}, &stdout, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("successful command wrote operational log: %s", stderr.String())
	}
	var result map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result["status"] != "created" || result["path"] != path {
		t.Fatalf("stdout=%q result=%v err=%v", stdout.String(), result, err)
	}
}
