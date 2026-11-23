package codetools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/execrunner"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

// TC-D-12 (REQ-14): every workspace command supplies its managed directory,
// and the size watchdog cancels a live producer and removes oversized data.
func TestPhase12dWorkspaceDirAndCloneCapRemainEnforced(t *testing.T) {
	t.Run("working directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "acme", "app")
		called := false
		w := &workspace{runner: approvedRunnerFunc(func(_ context.Context, c execrunner.Command) (execrunner.Result, error) {
			called = true
			if c.Dir != root {
				t.Fatalf("git Dir=%q want %q", c.Dir, root)
			}
			return execrunner.Result{}, nil
		}), remoteBase: githubRemoteBase}
		if _, err := w.run(context.Background(), root, nil, false, "status"); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("git runner was not called")
		}
	})

	t.Run("live cap", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "acme", "app")
		cancelled := make(chan struct{})
		w := &workspace{maxCloneBytes: 1 << 20, remoteBase: githubRemoteBase}
		w.runner = approvedRunnerFunc(func(ctx context.Context, c execrunner.Command) (execrunner.Result, error) {
			if !c.Network || c.Dir != base {
				t.Fatalf("network=%v Dir=%q want %q", c.Network, c.Dir, base)
			}
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "growing.pack"), make([]byte, 2<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			<-ctx.Done()
			close(cancelled)
			return execrunner.Result{}, ctx.Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_, err := w.runWatched(ctx, base, root, nil, "clone", "https://github.com/acme/app.git", "app")
		var toolErr *tools.ToolError
		if !errors.As(err, &toolErr) || toolErr.Code != "clone_too_large" {
			t.Fatalf("watchdog error=%v", err)
		}
		select {
		case <-cancelled:
		default:
			t.Fatal("watchdog returned without cancelling producer")
		}
		if ctx.Err() != nil {
			t.Fatal("independent deadline fired before watchdog")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("oversized root retained: %v", err)
		}
	})
}
