package worker

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// T-7B-1 (REQ-6, R-B-4; flake finding for TestJob_InstructionAppliedOnceAndAcked):
// the hub resends an instruction until it is acked, and the heartbeat loop
// hands every reply to takeInstructions while appendFollowUps is writing the
// same instruction to the file. Once an instruction has been taken for the
// executor, a resend must never queue it again, so the file holds it once.
func TestJob_InstructionNotRequeuedWhileBeingApplied(t *testing.T) {
	const rounds = 40
	requeued := 0
	for i := 0; i < rounds; i++ {
		j, err := CreateJournal(t.TempDir(), LeaseRecord{JobID: "job-race", Attempt: 1, LeaseToken: "lt", HeartbeatIntervalSeconds: 30, LeasedAt: time.Now()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		wt := t.TempDir()
		if err := os.MkdirAll(filepath.Join(wt, privateDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, privateDirName, instructionFileName), []byte("# Task\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := &jobRun{j: j}
		r.ws.Worktree = wt
		in := behavior.WorkerInstruction{ID: "v1", Text: "also add docs"}
		r.takeInstructions([]behavior.WorkerInstruction{in}) // arrives with the first heartbeat

		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // the heartbeat loop: the hub has not seen the ack yet and resends
			defer wg.Done()
			for !stop.Load() {
				r.takeInstructions([]behavior.WorkerInstruction{in})
			}
		}()
		if _, err := r.appendFollowUps(); err != nil {
			t.Fatal(err)
		}
		stop.Store(true)
		wg.Wait()
		// The executor finished its first run; the next batch is taken now.
		if _, err := r.appendFollowUps(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(wt, privateDirName, instructionFileName))
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(data), "also add docs"); n != 1 {
			requeued++
			if requeued == 1 {
				t.Errorf("round %d: instruction written %d times:\n%s", i, n, data)
			}
		}
		j.Close()
	}
	if requeued > 0 {
		t.Fatalf("instruction was applied twice in %d of %d rounds (resend queued it again while appendFollowUps was writing it)", requeued, rounds)
	}
}
