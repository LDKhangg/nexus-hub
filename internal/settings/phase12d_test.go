package settings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TC-D-11 (REQ-14): sandboxing is on by default, the production bwrap path is
// fixed, and settings updates accept only clean executable absolute paths.
func TestPhase12dSandboxSettingsDefaultAndPathValidation(t *testing.T) {
	s := Defaults()
	if !s.GitSandbox || s.BwrapPath != "/usr/bin/bwrap" {
		t.Fatalf("sandbox defaults: enabled=%v path=%q", s.GitSandbox, s.BwrapPath)
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(executable)
	if err := Validate("bwrap_path", raw); err != nil {
		t.Fatalf("valid bwrap path rejected: %v", err)
	}
	loaded, err := Load(context.Background(), mapStore{"bwrap_path": raw, "git_sandbox": json.RawMessage(`false`)})
	if err != nil || loaded.BwrapPath != executable || loaded.GitSandbox {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	nonExecutable := filepath.Join(dir, "nonexec")
	if err := os.WriteFile(nonExecutable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := []json.RawMessage{json.RawMessage(`"relative/bwrap"`), json.RawMessage(`"/tmp/../tmp/bwrap"`)}
	nonExecRaw, _ := json.Marshal(nonExecutable)
	missingRaw, _ := json.Marshal(filepath.Join(dir, "missing"))
	bad = append(bad, nonExecRaw, missingRaw)
	for _, value := range bad {
		if err := Validate("bwrap_path", value); err == nil {
			t.Errorf("invalid bwrap path accepted: %s", value)
		}
	}
}
