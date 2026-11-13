package clirun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFixBrief1FollowUpRetriesOnceAndPreservesMainResult(t *testing.T) {
	// TC-R5 (REQ-C10).
	t.Run("TC-R5/follow-up failure is bounded and separate", func(t *testing.T) {
		dir := t.TempDir()
		attempts := filepath.Join(dir, "attempts")
		path := fakeExecutable(t, "if [ \"${1-}\" = cleanup ]; then printf x >> "+attempts+"; printf 'PRIVATE FOLLOW-UP' >&2; exit 1; fi\nprintf 'kept answer'")
		runner, err := New(Config{Path: path, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		res, err := runner.Run(context.Background(), Call{FollowUp: func([]byte) []string { return []string{"cleanup"} }})
		if err != nil || string(res.Stdout) != "kept answer" || !errors.Is(res.FollowUpError, ErrFollowUp) {
			t.Fatalf("result=%#v err=%v", res, err)
		}
		raw, err := os.ReadFile(attempts)
		if err != nil || len(raw) != 2 {
			t.Fatalf("attempts=%q err=%v", raw, err)
		}
		if res.FollowUpError.Error() != ErrFollowUp.Error() {
			t.Fatalf("follow-up output leaked: %v", res.FollowUpError)
		}
	})
}
