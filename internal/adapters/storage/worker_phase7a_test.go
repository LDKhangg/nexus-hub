package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w7Env is a worker store on the throwaway DB with a controllable clock.
type w7Env struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	s     *WorkerStore
	clock time.Time
}

func newW7(t *testing.T) *w7Env {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	s, err := NewWorkerStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	e := &w7Env{ctx: ctx, pool: pool, s: s, clock: time.Now().UTC().Truncate(time.Second)}
	s.now = func() time.Time { return e.clock }
	return e
}

func (e *w7Env) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *w7Env) enroll(t *testing.T, ws, name string, caps ...string) (behavior.WorkerDevice, string) {
	t.Helper()
	code, _, err := e.s.CreateEnrollCode(e.ctx, ws, "owner")
	if err != nil {
		t.Fatal(err)
	}
	d, tok, err := e.s.Enroll(e.ctx, code, name, caps)
	if err != nil {
		t.Fatalf("enroll %s: %v", name, err)
	}
	return d, tok
}

func (e *w7Env) job(t *testing.T, ws string, caps []string, payload string) behavior.WorkerJob {
	t.Helper()
	var p json.RawMessage
	if payload != "" {
		p = json.RawMessage(payload)
	}
	j, err := e.s.CreateJob(e.ctx, ws, 0, "step", caps, p)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return j
}

func (e *w7Env) lease(t *testing.T, d behavior.WorkerDevice) (behavior.WorkerLease, bool) {
	t.Helper()
	l, ok, err := e.s.LeaseJob(e.ctx, d)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return l, ok
}

func (e *w7Env) mustLease(t *testing.T, d behavior.WorkerDevice) behavior.WorkerLease {
	t.Helper()
	l, ok := e.lease(t, d)
	if !ok {
		t.Fatalf("device %s got no job", d.Name)
	}
	return l
}

func (e *w7Env) events(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT kind || ':' || reason_code FROM worker_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func hasEvent(evs []string, want string) bool {
	for _, e := range evs {
		if e == want {
			return true
		}
	}
	return false
}

func (e *w7Env) jobState(t *testing.T, id string) string {
	t.Helper()
	var st string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM worker_jobs WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func okResult(status string) behavior.WorkerResult {
	return behavior.WorkerResult{Status: status, Summary: "done", TestOutcome: "passed"}
}

func w7I(v int64) *int64         { return &v }
func w7T(v time.Time) *time.Time { return &v }

// ---------------- enrollment (REQ-1) ----------------

// T-1 (REQ-1): a code is single use.
func TestWorker7a_EnrollCodeSingleUse(t *testing.T) {
	e := newW7(t)
	code, _, err := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Enroll(e.ctx, code, "laptop", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Enroll(e.ctx, code, "second", nil); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("reused code: %v", err)
	}
	// concurrent redemption: exactly one wins
	code2, _, _ := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := e.s.Enroll(e.ctx, code2, fmt.Sprintf("dev%d", i), nil); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("code redeemed %d times concurrently", wins)
	}
}

// T-2 (REQ-1): an expired code is refused.
func TestWorker7a_EnrollCodeExpires(t *testing.T) {
	e := newW7(t)
	code, expires, err := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if d := expires.Sub(e.clock); d < 9*time.Minute || d > 10*time.Minute+time.Second {
		t.Fatalf("code lifetime %v, want 10 min", d)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE worker_enroll_codes SET expires_at = now() - interval '1 second'`)
	if _, _, err := e.s.Enroll(e.ctx, code, "laptop", nil); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("expired code accepted: %v", err)
	}
}

// T-3 (REQ-1): a human types the code in lower case, without dashes, with spaces.
func TestWorker7a_EnrollCodeNormalized(t *testing.T) {
	e := newW7(t)
	for i, mangle := range []func(string) string{
		strings.ToLower,
		func(s string) string { return strings.ReplaceAll(s, "-", "") },
		func(s string) string { return "  " + strings.ReplaceAll(strings.ToLower(s), "-", " ") + "\n" },
	} {
		code, _, _ := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
		if _, _, err := e.s.Enroll(e.ctx, mangle(code), fmt.Sprintf("dev%d", i), nil); err != nil {
			t.Fatalf("variant %d refused: %v", i, err)
		}
	}
	for _, bad := range []string{"", "ABCD", "AAAA-BBBB-CCCC", strings.Repeat("A", 5000)} {
		if _, _, err := e.s.Enroll(e.ctx, bad, "x", nil); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("garbage code %q: %v", bad[:min(len(bad), 8)], err)
		}
	}
}

// T-4 (REQ-1): a name clash does not burn the code.
func TestWorker7a_NameClashDoesNotBurnCode(t *testing.T) {
	e := newW7(t)
	e.enroll(t, workspaceA, "laptop")
	code, _, _ := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	if _, _, err := e.s.Enroll(e.ctx, code, "laptop", nil); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("clash: %v", err)
	}
	if _, _, err := e.s.Enroll(e.ctx, code, "laptop2", nil); err != nil {
		t.Fatalf("code burned by a name clash: %v", err)
	}
	// same name in the other workspace is fine
	e.enroll(t, workspaceB, "laptop")
}

// T-6 (REQ-1): bad device names and capabilities are refused before the code is used.
func TestWorker7a_EnrollInputValidation(t *testing.T) {
	e := newW7(t)
	code, _, _ := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	for _, name := range []string{"", "   ", "my laptop", "máy-của-tôi", "pc-😀", "..", "-x", "a/b", strings.Repeat("n", 41), "x\ny"} {
		if _, _, err := e.s.Enroll(e.ctx, code, name, nil); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	for _, caps := range [][]string{{"Repo.Write"}, {"repo write"}, {"../x"}, make([]string, 33)} {
		if _, _, err := e.s.Enroll(e.ctx, code, "ok", caps); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("caps %v: %v", caps, err)
		}
	}
	if _, _, err := e.s.Enroll(e.ctx, code, "ok", []string{"repo.write", "build.go"}); err != nil {
		t.Fatalf("code burned by invalid input: %v", err)
	}
}

// T-5 (REQ-1, 13): the database holds only hashes of the token, code and lease token.
func TestWorker7a_OnlyHashesStored(t *testing.T) {
	e := newW7(t)
	code, _, _ := e.s.CreateEnrollCode(e.ctx, workspaceA, "owner")
	dev, token, err := e.s.Enroll(e.ctx, code, "laptop", []string{"repo.write"})
	if err != nil {
		t.Fatal(err)
	}
	e.job(t, workspaceA, nil, "")
	lease := e.mustLease(t, dev)
	norm := strings.ReplaceAll(code, "-", "")
	for table, secrets := range map[string][]string{
		"worker_devices": {token}, "worker_enroll_codes": {code, norm},
		"worker_jobs": {lease.LeaseToken, token}, "worker_events": {token, lease.LeaseToken, code},
	} {
		for _, secret := range secrets {
			var n int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` r WHERE r::text LIKE '%'||$1||'%'`, secret).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s holds a plaintext secret (n=%d err=%v)", table, n, err)
			}
		}
	}
	if !strings.HasPrefix(token, "nhw_") || len(token) < 40 || !strings.HasPrefix(lease.LeaseToken, "nhl_") {
		t.Fatalf("token shapes: %q %q", token, lease.LeaseToken)
	}
}

// T-7 (REQ-1, 13): revoke kills the token at once; garbage tokens never match.
func TestWorker7a_RevokeAndGarbageTokens(t *testing.T) {
	e := newW7(t)
	dev, token := e.enroll(t, workspaceA, "laptop")
	got, err := e.s.Authenticate(e.ctx, token)
	if err != nil || got.ID != dev.ID || got.WorkspaceID != workspaceA {
		t.Fatalf("auth: %+v %v", got, err)
	}
	for _, bad := range []string{"", " ", "nhw_", "nhw_garbage", token + "x", token[:len(token)-1], strings.ToUpper(token), strings.Repeat("a", 300), "' OR 1=1 --"} {
		if _, err := e.s.Authenticate(e.ctx, bad); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("token %q authenticated: %v", bad[:min(len(bad), 12)], err)
		}
	}
	if err := e.s.RevokeDevice(e.ctx, workspaceA, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Authenticate(e.ctx, token); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if err := e.s.RevokeDevice(e.ctx, workspaceA, "laptop"); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if err := e.s.RevokeDevice(e.ctx, workspaceA, "nobody"); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("unknown revoke: %v", err)
	}
	if err := e.s.RevokeDevice(e.ctx, workspaceA, "  "); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("empty revoke: %v", err)
	}
	// replaced laptop re-enrolls under the same name; the old token stays dead.
	_, token2 := e.enroll(t, workspaceA, "laptop")
	if _, err := e.s.Authenticate(e.ctx, token2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Authenticate(e.ctx, token); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatal("old token revived")
	}
	evs := e.events(t)
	if !hasEvent(evs, "enrolled:") || !hasEvent(evs, "revoked:owner_revoked") {
		t.Fatalf("audit: %v", evs)
	}
	list, _ := e.s.ListDevices(e.ctx, workspaceA)
	if len(list) != 2 || list[0].RevokedAt != nil || list[1].RevokedAt == nil {
		t.Fatalf("list order/revoked: %+v", list)
	}
}

// T-8 (REQ-13): another workspace cannot revoke or see a device.
func TestWorker7a_RevokeIsWorkspaceScoped(t *testing.T) {
	e := newW7(t)
	_, token := e.enroll(t, workspaceA, "laptop")
	if err := e.s.RevokeDevice(e.ctx, workspaceB, "laptop"); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cross-workspace revoke: %v", err)
	}
	if _, err := e.s.Authenticate(e.ctx, token); err != nil {
		t.Fatal("device lost its token")
	}
	if l, _ := e.s.ListDevices(e.ctx, workspaceB); len(l) != 0 {
		t.Fatalf("workspace B sees devices: %+v", l)
	}
}

// ---------------- lease protocol (REQ-3, 4) ----------------

// T-10 (REQ-3): concurrent polls never lease a job twice.
func TestWorker7a_ConcurrentLeaseNeverDoubles(t *testing.T) {
	for _, jobs := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d_jobs", jobs), func(t *testing.T) {
			e := newW7(t)
			var devs []behavior.WorkerDevice
			for i := 0; i < 6; i++ {
				d, _ := e.enroll(t, workspaceA, fmt.Sprintf("dev%d", i))
				devs = append(devs, d)
			}
			for i := 0; i < jobs; i++ {
				e.job(t, workspaceA, nil, "")
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			var mu sync.Mutex
			leased := map[string]int{}
			for _, d := range devs {
				wg.Add(1)
				go func(d behavior.WorkerDevice) {
					defer wg.Done()
					<-start
					l, ok, err := e.s.LeaseJob(e.ctx, d)
					if err != nil {
						t.Errorf("lease error: %v", err)
						return
					}
					if ok {
						mu.Lock()
						leased[l.Job.ID]++
						mu.Unlock()
					}
				}(d)
			}
			close(start)
			wg.Wait()
			if len(leased) != jobs {
				t.Fatalf("leased %d distinct jobs, want %d", len(leased), jobs)
			}
			for id, n := range leased {
				if n != 1 {
					t.Fatalf("job %s leased %d times", id, n)
				}
			}
			var active int
			e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_jobs WHERE state='LEASED'`).Scan(&active)
			if active != jobs {
				t.Fatalf("db shows %d leased", active)
			}
		})
	}
}

// T-11 (REQ-4): only a device whose profile covers the job's capabilities may lease it.
func TestWorker7a_CapabilityMatch(t *testing.T) {
	e := newW7(t)
	weak, _ := e.enroll(t, workspaceA, "weak", "repo.write")
	strong, _ := e.enroll(t, workspaceA, "strong", "repo.write", "build.go", "executor.claude")
	bare, _ := e.enroll(t, workspaceA, "bare")
	j := e.job(t, workspaceA, []string{"repo.write", "build.go"}, "")
	if _, ok := e.lease(t, weak); ok {
		t.Fatal("device missing build.go got the job")
	}
	if _, ok := e.lease(t, bare); ok {
		t.Fatal("device without capabilities got the job")
	}
	if e.jobState(t, j.ID) != behavior.WorkerJobWaiting {
		t.Fatal("job left WAITING_FOR_WORKER")
	}
	l := e.mustLease(t, strong)
	if l.Job.ID != j.ID {
		t.Fatal("wrong job")
	}
	// a job with no requirements goes to a device with no capabilities
	e.job(t, workspaceA, nil, "")
	if _, ok := e.lease(t, bare); !ok {
		t.Fatal("job without requirements not leasable by a bare device")
	}
	if _, ok := e.lease(t, weak); ok {
		// nothing else waits
		t.Fatal("unexpected job")
	}
}

// T-12 (REQ-3): one active job per device; oldest first; free again after the result.
func TestWorker7a_OneActiveJobPerDeviceFIFO(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	first := e.job(t, workspaceA, nil, "")
	second := e.job(t, workspaceA, nil, "")
	l1 := e.mustLease(t, d)
	if l1.Job.ID != first.ID || l1.Job.Attempt != 1 {
		t.Fatalf("not FIFO / attempt: %+v", l1.Job)
	}
	// Fix Brief A1 (R-1): a poll while the job is only LEASED re-issues the
	// same job with a new token; it never hands out the second job.
	re := e.mustLease(t, d)
	if re.Job.ID != first.ID || re.Job.Attempt != 1 || re.LeaseToken == l1.LeaseToken {
		t.Fatalf("busy device did not get its own job re-issued: %+v", re.Job)
	}
	l1 = re
	if _, err := e.s.Heartbeat(e.ctx, d, first.ID, l1.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.lease(t, d); ok {
		t.Fatal("device leased a second job while busy")
	}
	if _, _, err := e.s.ReportResult(e.ctx, d, first.ID, l1.LeaseToken, "k1", okResult("succeeded")); err != nil {
		t.Fatal(err)
	}
	l2 := e.mustLease(t, d)
	if l2.Job.ID != second.ID {
		t.Fatal("second job not leased after the first finished")
	}
}

// T-13 (REQ-13): a device only sees jobs of its own workspace.
func TestWorker7a_WorkspaceIsolation(t *testing.T) {
	e := newW7(t)
	devA, _ := e.enroll(t, workspaceA, "laptop")
	devB, _ := e.enroll(t, workspaceB, "laptop")
	foreign := e.job(t, workspaceB, nil, "")
	if _, ok := e.lease(t, devA); ok {
		t.Fatal("workspace A device leased a workspace B job")
	}
	lB := e.mustLease(t, devB)
	if _, err := e.s.Heartbeat(e.ctx, devA, foreign.ID, lB.LeaseToken); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cross-workspace heartbeat: %v", err)
	}
	if _, _, err := e.s.ReportResult(e.ctx, devA, foreign.ID, lB.LeaseToken, "k", okResult("succeeded")); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cross-workspace result: %v", err)
	}
	if _, err := e.s.GetJob(e.ctx, workspaceA, foreign.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("GetJob across workspaces: %v", err)
	}
	if _, err := e.s.CancelJob(e.ctx, workspaceA, foreign.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("CancelJob across workspaces: %v", err)
	}
	if e.jobState(t, foreign.ID) != behavior.WorkerJobLeased {
		t.Fatal("foreign request changed the job")
	}
}

// T-14 (REQ-3, 12): stale, foreign and garbage leases are refused and audited.
func TestWorker7a_HeartbeatRejections(t *testing.T) {
	e := newW7(t)
	d1, _ := e.enroll(t, workspaceA, "one")
	d2, _ := e.enroll(t, workspaceA, "two")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d1)

	if _, err := e.s.Heartbeat(e.ctx, d1, j.ID, "nhl_wrong"); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("stale token: %v", err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d1, j.ID, ""); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d2, j.ID, l.LeaseToken); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("other device with the right token: %v", err)
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000", ""} {
		if _, err := e.s.Heartbeat(e.ctx, d1, id, l.LeaseToken); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("job id %q: %v", id, err)
		}
	}
	evs := e.events(t)
	if !hasEvent(evs, "rejected:stale_lease") || !hasEvent(evs, "rejected:foreign_job") {
		t.Fatalf("rejections not audited: %v", evs)
	}
	// the legit heartbeat extends the lease
	e.advance(5 * time.Minute)
	job, err := e.s.Heartbeat(e.ctx, d1, j.ID, l.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != behavior.WorkerJobRunning && job.State != behavior.WorkerJobLeased {
		t.Fatalf("state %s", job.State)
	}
	if want := e.clock.Add(behavior.WorkerLeaseDuration); job.LeaseExpiresAt == nil || job.LeaseExpiresAt.Sub(want).Abs() > time.Second {
		t.Fatalf("lease not extended: %v want %v", job.LeaseExpiresAt, want)
	}
}

// T-15 (REQ-7): the same idempotency key returns the first result; another key conflicts.
func TestWorker7a_ResultIdempotency(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	first := behavior.WorkerResult{Status: "succeeded", Summary: "first", PRURL: "https://github.com/o/r/pull/1", TestOutcome: "passed"}
	job, dup, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "key-1", first)
	if err != nil || dup || job.State != behavior.WorkerJobSucceeded {
		t.Fatalf("first: %+v dup=%v err=%v", job, dup, err)
	}
	other := behavior.WorkerResult{Status: "failed", Summary: "second"}
	job, dup, err = e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "key-1", other)
	if err != nil || !dup || job.Result == nil || job.Result.Summary != "first" || job.State != behavior.WorkerJobSucceeded {
		t.Fatalf("replay must return the first result: %+v dup=%v err=%v", job, dup, err)
	}
	if _, _, err = e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "key-2", other); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("different key for a finished job: %v", err)
	}
	got, _ := e.s.GetJob(e.ctx, workspaceA, j.ID)
	if got.Result.Summary != "first" || got.State != behavior.WorkerJobSucceeded {
		t.Fatalf("result overwritten: %+v", got)
	}
	evs := e.events(t)
	if !hasEvent(evs, "rejected:result_conflict") {
		t.Fatalf("conflict not audited: %v", evs)
	}
	n := 0
	for _, ev := range evs {
		if strings.HasPrefix(ev, "result_") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d result events for one result", n)
	}
	// concurrent double upload: one result recorded
	j2 := e.job(t, workspaceA, nil, "")
	l2 := e.mustLease(t, d)
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, dup, err := e.s.ReportResult(e.ctx, d, j2.ID, l2.LeaseToken, "same", okResult("succeeded"))
			if err == nil && !dup {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fresh != 1 {
		t.Fatalf("%d fresh results from concurrent retries", fresh)
	}
}

// T-16 (REQ-7): a result with a stale lease token or bad body is refused.
func TestWorker7a_ResultRejections(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, "nhl_stale", "k", okResult("succeeded")); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "bad key!", okResult("succeeded")); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad key: %v", err)
	}
	if e.jobState(t, j.ID) != behavior.WorkerJobLeased {
		t.Fatal("rejected result changed the job")
	}
}

// T-36 (REQ-7): the result contract is enforced by the store too.
func TestWorker7a_ResultContractAtStore(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	bad := []behavior.WorkerResult{
		{Status: "succeeded", Summary: strings.Repeat("ệ", 2001)},
		{Status: "succeeded", PRURL: "https://evil.example/o/r/pull/1"},
		{Status: "succeeded", PRURL: "http://github.com/o/r/pull/1"},
		{Status: "nope"},
		{Status: "failed", ReasonCode: "free text reason"},
		{Status: "succeeded", Branch: "../../main"},
	}
	for i, r := range bad {
		if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, fmt.Sprintf("k%d", i), r); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("bad result %d accepted: %v", i, err)
		}
	}
	good := behavior.WorkerResult{Status: "needs_input", ReasonCode: "diff_too_large", Summary: strings.Repeat("ệ", 2000), Usage: behavior.WorkerUsage{InputTokens: 5}}
	job, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "good", good)
	if err != nil || job.State != behavior.WorkerJobNeedsInput {
		t.Fatalf("good result: %+v %v", job, err)
	}
	if got, _ := e.s.GetJob(e.ctx, workspaceA, j.ID); got.Result == nil || len([]rune(got.Result.Summary)) != 2000 {
		t.Fatalf("stored summary mangled: %+v", got.Result)
	}
}

// T-17 (REQ-7): progress is deduplicated by seq and bounded.
func TestWorker7a_ProgressDedupe(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	p := func(seq int64, phase string) behavior.WorkerProgress {
		return behavior.WorkerProgress{Seq: seq, Phase: phase, Counts: behavior.WorkerProgressCounts{FilesChanged: seq}}
	}
	job, dup, err := e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, p(1, "cloning"))
	if err != nil || dup || job.Progress == nil || job.Progress.Phase != "cloning" {
		t.Fatalf("seq1: %+v dup=%v %v", job.Progress, dup, err)
	}
	if _, dup, _ = e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, p(1, "editing")); !dup {
		t.Fatal("replayed seq not flagged duplicate")
	}
	job, dup, _ = e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, p(3, "testing"))
	if dup || job.Progress.Phase != "testing" {
		t.Fatalf("seq3: %+v", job.Progress)
	}
	job, dup, _ = e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, p(2, "editing"))
	if !dup || job.Progress.Phase != "testing" {
		t.Fatalf("an older seq overwrote newer progress: %+v dup=%v", job.Progress, dup)
	}
	for _, bad := range []behavior.WorkerProgress{p(0, "cloning"), p(1, "reading your ssh keys"), {Seq: 9, Phase: "editing", Counts: behavior.WorkerProgressCounts{Turns: -1}}} {
		if _, _, err := e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, bad); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("bad progress accepted: %+v %v", bad, err)
		}
	}
	if _, _, err := e.s.ReportProgress(e.ctx, d, j.ID, "nhl_stale", p(9, "editing")); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("stale progress: %v", err)
	}
}

// T-18 (REQ-9): cancellation by job state.
func TestWorker7a_Cancel(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	waiting := e.job(t, workspaceA, nil, "")
	got, err := e.s.CancelJob(e.ctx, workspaceA, waiting.ID)
	if err != nil || got.State != behavior.WorkerJobCancelled || got.FinishedAt == nil {
		t.Fatalf("cancel waiting: %+v %v", got, err)
	}
	if _, ok := e.lease(t, d); ok {
		t.Fatal("cancelled job leased")
	}
	if _, err := e.s.CancelJob(e.ctx, workspaceA, waiting.ID); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("cancel finished: %v", err)
	}
	if _, err := e.s.CancelJob(e.ctx, workspaceA, "garbage"); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cancel garbage id: %v", err)
	}

	running := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	if _, err := e.s.CancelJob(e.ctx, workspaceA, running.ID); err != nil {
		t.Fatal(err)
	}
	hb, err := e.s.Heartbeat(e.ctx, d, running.ID, l.LeaseToken)
	if err != nil || hb.State != behavior.WorkerJobCancelling {
		t.Fatalf("heartbeat must show CANCELLING (state kept, not resurrected): %+v %v", hb.State, err)
	}
	pj, _, err := e.s.ReportProgress(e.ctx, d, running.ID, l.LeaseToken, behavior.WorkerProgress{Seq: 1, Phase: "editing"})
	if err != nil || pj.State != behavior.WorkerJobCancelling {
		t.Fatalf("progress after cancel: %+v %v", pj.State, err)
	}
	res, _, err := e.s.ReportResult(e.ctx, d, running.ID, l.LeaseToken, "c1", behavior.WorkerResult{Status: "cancelled", ReasonCode: "owner_cancelled"})
	if err != nil || res.State != behavior.WorkerJobCancelled {
		t.Fatalf("cancelled result: %+v %v", res.State, err)
	}
	// device is free again
	e.job(t, workspaceA, nil, "")
	e.mustLease(t, d)
	evs := e.events(t)
	for _, want := range []string{"cancelled:owner_cancelled", "cancel_requested:owner_cancelled", "result_cancelled:owner_cancelled"} {
		if !hasEvent(evs, want) {
			t.Fatalf("missing event %s in %v", want, evs)
		}
	}
}

// T-19 (REQ-3): the sweeper interrupts expired leases; cancelling jobs become cancelled.
func TestWorker7a_SweeperLeaseExpiry(t *testing.T) {
	e := newW7(t)
	d1, _ := e.enroll(t, workspaceA, "one")
	d2, _ := e.enroll(t, workspaceA, "two")
	a := e.job(t, workspaceA, nil, "")
	b := e.job(t, workspaceA, nil, "")
	la := e.mustLease(t, d1)
	e.mustLease(t, d2)
	e.advance(9 * time.Minute)
	if n, err := e.s.ExpireLeases(e.ctx); err != nil || n != 0 {
		t.Fatalf("expired early: %d %v", n, err)
	}
	if _, err := e.s.CancelJob(e.ctx, workspaceA, b.ID); err != nil {
		t.Fatal(err)
	}
	// keep job a alive for d1 with a heartbeat at +9 min, let b lapse
	if _, err := e.s.Heartbeat(e.ctx, d1, a.ID, la.LeaseToken); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Minute)
	n, err := e.s.ExpireLeases(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d (%v), want only the cancelling job", n, err)
	}
	if st := e.jobState(t, b.ID); st != behavior.WorkerJobCancelled {
		t.Fatalf("CANCELLING expiry -> %s", st)
	}
	e.advance(9 * time.Minute)
	if n, _ := e.s.ExpireLeases(e.ctx); n != 1 {
		t.Fatalf("job a not expired: %d", n)
	}
	if st := e.jobState(t, a.ID); st != behavior.WorkerJobInterrupted {
		t.Fatalf("expiry -> %s", st)
	}
	if n, _ := e.s.ExpireLeases(e.ctx); n != 0 {
		t.Fatal("sweeper is not idempotent")
	}
	if !hasEvent(e.events(t), "lease_lost:lease_expired") {
		t.Fatalf("no lease_lost event: %v", e.events(t))
	}
	// device is no longer busy and may take a new job
	e.job(t, workspaceA, nil, "")
	e.mustLease(t, d1)
}

// T-20 (REQ-2): a device without contact for 90 s is marked offline.
func TestWorker7a_DeviceOffline(t *testing.T) {
	e := newW7(t)
	dev, token := e.enroll(t, workspaceA, "laptop")
	if _, err := e.s.Authenticate(e.ctx, token); err != nil { // contact
		t.Fatal(err)
	}
	var state string
	e.pool.QueryRow(e.ctx, `SELECT state FROM worker_devices WHERE id=$1`, dev.ID).Scan(&state)
	if state != "online" {
		t.Fatalf("after contact: %s", state)
	}
	e.advance(60 * time.Second)
	if n, _ := e.s.MarkDevicesOffline(e.ctx); n != 0 {
		t.Fatal("marked offline after 60 s")
	}
	e.advance(31 * time.Second)
	n, err := e.s.MarkDevicesOffline(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("offline sweep: %d %v", n, err)
	}
	e.pool.QueryRow(e.ctx, `SELECT state FROM worker_devices WHERE id=$1`, dev.ID).Scan(&state)
	if state != "offline" || !hasEvent(e.events(t), "offline:no_contact") {
		t.Fatalf("state=%s events=%v", state, e.events(t))
	}
	if n, _ := e.s.MarkDevicesOffline(e.ctx); n != 0 {
		t.Fatal("offline event repeated")
	}
	// network change: the next authenticated call brings it back, no re-enrollment
	if _, err := e.s.Authenticate(e.ctx, token); err != nil {
		t.Fatal(err)
	}
	e.pool.QueryRow(e.ctx, `SELECT state FROM worker_devices WHERE id=$1`, dev.ID).Scan(&state)
	if state != "online" {
		t.Fatalf("not back online: %s", state)
	}
}

func interrupted(t *testing.T, e *w7Env) (behavior.WorkerDevice, behavior.WorkerJob, behavior.WorkerLease) {
	t.Helper()
	d, _ := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, "")
	l := e.mustLease(t, d)
	e.advance(11 * time.Minute)
	if n, _ := e.s.ExpireLeases(e.ctx); n != 1 {
		t.Fatal("setup: lease did not expire")
	}
	return d, j, l
}

// T-21 (REQ-15): late heartbeat, progress or result from the same device within 24 h resumes the job.
func TestWorker7a_LateContactResumes(t *testing.T) {
	t.Run("heartbeat", func(t *testing.T) {
		e := newW7(t)
		d, j, l := interrupted(t, e)
		e.advance(2 * time.Hour)
		got, err := e.s.Heartbeat(e.ctx, d, j.ID, l.LeaseToken)
		if err != nil || got.State != behavior.WorkerJobRunning {
			t.Fatalf("%+v %v", got.State, err)
		}
		if !hasEvent(e.events(t), "resumed:") {
			t.Fatalf("no resumed event: %v", e.events(t))
		}
		if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "k", okResult("succeeded")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("progress", func(t *testing.T) {
		e := newW7(t)
		d, j, l := interrupted(t, e)
		got, _, err := e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, behavior.WorkerProgress{Seq: 1, Phase: "testing"})
		if err != nil || got.State != behavior.WorkerJobRunning {
			t.Fatalf("%+v %v", got.State, err)
		}
	})
	t.Run("result", func(t *testing.T) {
		e := newW7(t)
		d, j, l := interrupted(t, e)
		e.advance(23 * time.Hour)
		got, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "late", okResult("succeeded"))
		if err != nil || got.State != behavior.WorkerJobSucceeded {
			t.Fatalf("%+v %v", got.State, err)
		}
		evs := e.events(t)
		if !hasEvent(evs, "resumed:") || !hasEvent(evs, "result_succeeded:") {
			t.Fatalf("events %v", evs)
		}
	})
}

// T-22 (REQ-15): after 24 h, or from another device, late contact is rejected and recorded.
func TestWorker7a_LateContactRejected(t *testing.T) {
	e := newW7(t)
	d, j, l := interrupted(t, e)
	other, _ := e.enroll(t, workspaceA, "other")
	if _, err := e.s.Heartbeat(e.ctx, other, j.ID, l.LeaseToken); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("other device resumed: %v", err)
	}
	if _, _, err := e.s.ReportResult(e.ctx, other, j.ID, l.LeaseToken, "k", okResult("succeeded")); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("other device result: %v", err)
	}
	e.advance(24*time.Hour + time.Minute)
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l.LeaseToken); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("heartbeat after 24 h: %v", err)
	}
	if _, _, err := e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, behavior.WorkerProgress{Seq: 1, Phase: "testing"}); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("progress after 24 h: %v", err)
	}
	if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "k", okResult("succeeded")); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("result after 24 h: %v", err)
	}
	if st := e.jobState(t, j.ID); st != behavior.WorkerJobInterrupted {
		t.Fatalf("late contact changed the job: %s", st)
	}
	evs := e.events(t)
	if !hasEvent(evs, "rejected:late_resume") || !hasEvent(evs, "rejected:foreign_job") {
		t.Fatalf("rejections not recorded: %v", evs)
	}
}

// T-23 (REQ-3): an interrupted job is not re-leased to anyone; a resume cannot make a device hold two jobs.
func TestWorker7a_ResumeVsBusyAndNoDoubleLease(t *testing.T) {
	e := newW7(t)
	d, j, l := interrupted(t, e)
	other, _ := e.enroll(t, workspaceA, "other")
	if _, ok := e.lease(t, other); ok {
		t.Fatal("interrupted job leased to another device")
	}
	// the owner's device takes a different job meanwhile
	j2 := e.job(t, workspaceA, nil, "")
	l2 := e.mustLease(t, d)
	if l2.Job.ID != j2.ID {
		t.Fatal("expected the new job")
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l.LeaseToken); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("resume while busy: %v", err)
	}
	if !hasEvent(e.events(t), "rejected:device_busy") {
		t.Fatalf("device_busy not recorded: %v", e.events(t))
	}
	if _, _, err := e.s.ReportResult(e.ctx, d, j2.ID, l2.LeaseToken, "k", okResult("succeeded")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Heartbeat(e.ctx, d, j.ID, l.LeaseToken); err != nil {
		t.Fatalf("resume once free: %v", err)
	}
}

// T-18 (REQ-9): the owner cancels an interrupted job at once.
func TestWorker7a_CancelInterrupted(t *testing.T) {
	e := newW7(t)
	_, jj, _ := interrupted(t, e)
	got, err := e.s.CancelJob(e.ctx, workspaceA, jj.ID)
	if err != nil || got.State != behavior.WorkerJobCancelled {
		t.Fatalf("cancel interrupted: %+v %v", got.State, err)
	}
}

// ---------------- telemetry (REQ-18, 19) ----------------

// T-30 (REQ-18): out-of-range snapshots are rejected and nothing is stored.
func TestWorker7a_TelemetryRangeRejected(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	const gib = int64(1) << 30
	bad := []behavior.WorkerTelemetry{
		{State: "dead"},
		{State: "idle", CPUCores: w7I(0)},
		{State: "idle", ClaudeSessionLeftPct: w7I(101)},
		{State: "idle", ClaudeWeeklyLeftPct: w7I(-5)},
		{State: "idle", RAMUsedBytes: w7I(2 * gib), RAMTotalBytes: w7I(gib)},
		{State: "idle", DiskUsedBytes: w7I(-1)},
		{State: "idle", Executor: "rm -rf"},
		{State: "idle", Counters: behavior.WorkerCounters{BusySeconds: -1}},
	}
	for i, b := range bad {
		if err := e.s.RecordTelemetry(e.ctx, d, b); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("snapshot %d accepted: %v", i, err)
		}
	}
	var n int
	e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM worker_telemetry)+(SELECT count(*) FROM worker_telemetry_hourly)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows stored from rejected snapshots", n)
	}
}

// T-31 (REQ-18): latest snapshot per device, hourly rollup, 30 day retention.
func TestWorker7a_TelemetryLatestRollupPrune(t *testing.T) {
	e := newW7(t)
	d, _ := e.enroll(t, workspaceA, "laptop")
	e.clock = e.clock.Truncate(time.Hour).Add(10 * time.Minute)
	const gib = int64(1) << 30
	snap := func(cpu float64, sessionLeft, disk int64, jobs int64) behavior.WorkerTelemetry {
		f := cpu
		return behavior.WorkerTelemetry{State: "idle", CPULoad: &f, ClaudeSessionLeftPct: w7I(sessionLeft),
			DiskUsedBytes: w7I(100*gib - disk), DiskTotalBytes: w7I(100 * gib), Counters: behavior.WorkerCounters{Jobs: jobs}}
	}
	if err := e.s.RecordTelemetry(e.ctx, d, snap(0.5, 80, 50*gib, 1)); err != nil {
		t.Fatal(err)
	}
	e.advance(10 * time.Minute)
	if err := e.s.RecordTelemetry(e.ctx, d, snap(1.5, 60, 40*gib, 2)); err != nil {
		t.Fatal(err)
	}
	recs, err := e.s.ListTelemetry(e.ctx, workspaceA)
	if err != nil || len(recs) != 1 || *recs[0].Snapshot.CPULoad != 1.5 || *recs[0].Snapshot.ClaudeSessionLeftPct != 60 || recs[0].Snapshot.Counters.Jobs != 2 {
		t.Fatalf("latest snapshot: %+v %v", recs, err)
	}
	if other, _ := e.s.ListTelemetry(e.ctx, workspaceB); len(other) != 0 {
		t.Fatal("telemetry leaks across workspaces")
	}
	var samples int
	var cpuMax float64
	var diskMin int64
	var sessMin int16
	if err := e.pool.QueryRow(e.ctx, `SELECT samples, cpu_load_max, disk_free_min_bytes, claude_session_left_min FROM worker_telemetry_hourly`).Scan(&samples, &cpuMax, &diskMin, &sessMin); err != nil {
		t.Fatalf("one hourly row expected: %v", err)
	}
	if samples != 2 || cpuMax != 1.5 || diskMin != 40*gib || sessMin != 60 {
		t.Fatalf("rollup samples=%d cpuMax=%v diskMin=%d sessMin=%d", samples, cpuMax, diskMin, sessMin)
	}
	e.advance(time.Hour)
	if err := e.s.RecordTelemetry(e.ctx, d, snap(0.1, 90, 60*gib, 3)); err != nil {
		t.Fatal(err)
	}
	var rows int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_telemetry_hourly`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("next hour must make a new rollup row, have %d", rows)
	}
	e.advance(31 * 24 * time.Hour)
	if n, err := e.s.PruneTelemetry(e.ctx); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	// the snapshot itself holds no paths, hostnames or process lists
	var raw string
	e.pool.QueryRow(e.ctx, `SELECT snapshot::text FROM worker_telemetry`).Scan(&raw)
	for _, banned := range []string{"/home", "hostname", "process", "ip"} {
		if strings.Contains(strings.ToLower(raw), "\""+banned) {
			t.Fatalf("snapshot has %q: %s", banned, raw)
		}
	}
}

// T-32 (REQ-19): telemetry thresholds block assignment; the job waits.
func TestWorker7a_TelemetryThresholdsBlockAssignment(t *testing.T) {
	const gib = int64(1) << 30
	e := newW7(t)
	n := 0
	try := func(name string, snap *behavior.WorkerTelemetry, wantLease bool) {
		t.Helper()
		n++
		d, _ := e.enroll(t, workspaceA, fmt.Sprintf("dev%d", n))
		if snap != nil {
			if err := e.s.RecordTelemetry(e.ctx, d, *snap); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		j := e.job(t, workspaceA, nil, "")
		_, ok := e.lease(t, d)
		if ok != wantLease {
			t.Fatalf("%s: leased=%v want %v", name, ok, wantLease)
		}
		if !ok && e.jobState(t, j.ID) != behavior.WorkerJobWaiting {
			t.Fatalf("%s: blocked job left waiting state", name)
		}
		if !ok {
			if _, err := e.s.CancelJob(e.ctx, workspaceA, j.ID); err != nil { // keep the queue empty
				t.Fatal(err)
			}
		}
	}
	future := e.clock.Add(2 * time.Hour)
	past := e.clock.Add(-time.Minute)
	idle := func(f func(*behavior.WorkerTelemetry)) *behavior.WorkerTelemetry {
		v := &behavior.WorkerTelemetry{State: "idle"}
		f(v)
		return v
	}
	try("no telemetry yet", nil, true)
	try("session 9 left", idle(func(v *behavior.WorkerTelemetry) {
		v.ClaudeSessionLeftPct = w7I(9)
		v.ClaudeSessionResetAt = w7T(future)
	}), false)
	try("session 10 left", idle(func(v *behavior.WorkerTelemetry) { v.ClaudeSessionLeftPct = w7I(10) }), true)
	try("weekly 3 left", idle(func(v *behavior.WorkerTelemetry) { v.ClaudeWeeklyLeftPct = w7I(3); v.ClaudeWeeklyResetAt = w7T(future) }), false)
	try("session 0 reset passed", idle(func(v *behavior.WorkerTelemetry) { v.ClaudeSessionLeftPct = w7I(0); v.ClaudeSessionResetAt = w7T(past) }), true)
	try("disk 4 GiB free", idle(func(v *behavior.WorkerTelemetry) { v.DiskUsedBytes = w7I(96 * gib); v.DiskTotalBytes = w7I(100 * gib) }), false)
	try("disk 5 GiB free", idle(func(v *behavior.WorkerTelemetry) { v.DiskUsedBytes = w7I(95 * gib); v.DiskTotalBytes = w7I(100 * gib) }), true)
	try("ram 91 pct", idle(func(v *behavior.WorkerTelemetry) { v.RAMUsedBytes = w7I(91); v.RAMTotalBytes = w7I(100) }), false)
	try("ram 90 pct", idle(func(v *behavior.WorkerTelemetry) { v.RAMUsedBytes = w7I(90); v.RAMTotalBytes = w7I(100) }), true)
	try("everything unknown", idle(func(v *behavior.WorkerTelemetry) {}), true)

	// settings change the thresholds
	e.s.SetAssignmentThresholds(behavior.WorkerThresholds{MinClaudeLeftPct: 50, MinDiskFreeBytes: 5 * gib, MaxRAMUsedPct: 90})
	try("session 40 left, min 50", idle(func(v *behavior.WorkerTelemetry) {
		v.ClaudeSessionLeftPct = w7I(40)
		v.ClaudeSessionResetAt = w7T(future)
	}), false)
	e.s.SetAssignmentThresholds(behavior.DefaultWorkerThresholds)

	// a blocked device recovers when its next snapshot is fine
	d, _ := e.enroll(t, workspaceA, "recovering")
	low := behavior.WorkerTelemetry{State: "idle", ClaudeSessionLeftPct: w7I(2), ClaudeSessionResetAt: w7T(future)}
	e.s.RecordTelemetry(e.ctx, d, low)
	j := e.job(t, workspaceA, nil, "")
	if _, ok := e.lease(t, d); ok {
		t.Fatal("blocked device leased")
	}
	e.s.RecordTelemetry(e.ctx, d, behavior.WorkerTelemetry{State: "idle", ClaudeSessionLeftPct: w7I(80)})
	if l := e.mustLease(t, d); l.Job.ID != j.ID {
		t.Fatal("recovered device did not take the waiting job")
	}
}

// T-32 (REQ-19): a blocked device does not strand the job; a healthy one takes it.
func TestWorker7a_BlockedDeviceDoesNotStrandJob(t *testing.T) {
	e := newW7(t)
	sick, _ := e.enroll(t, workspaceA, "sick")
	well, _ := e.enroll(t, workspaceA, "well")
	e.s.RecordTelemetry(e.ctx, sick, behavior.WorkerTelemetry{State: "idle", RAMUsedBytes: w7I(99), RAMTotalBytes: w7I(100)})
	e.job(t, workspaceA, nil, "")
	if _, ok := e.lease(t, sick); ok {
		t.Fatal("RAM-starved device leased")
	}
	e.mustLease(t, well)
}

// T-33 (REQ-13): a job payload never carries a secret.
func TestWorker7a_PayloadSecretRefusal(t *testing.T) {
	e := newW7(t)
	for _, p := range []string{
		`{"github_token":"x"}`, `{"Authorization":"Bearer x"}`, `{"api-key":"x"}`, `{"x_api_key":"x"}`,
		`{"password":"x"}`, `{"ctx":{"deep":{"client_secret":"x"}}}`, `{"list":[{"ACCESS_KEY":"x"}]}`, `{"private_key":"x"}`,
		`{"cookie":"x"}`, `{"Token":"x"}`,
	} {
		if _, err := e.s.CreateJob(e.ctx, workspaceA, 0, "s", nil, json.RawMessage(p)); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("secret payload accepted: %s (%v)", p, err)
		}
	}
	for _, p := range []string{`[]`, `"x"`, `1`, `null`, `{`, `{"a":"` + strings.Repeat("x", behavior.WorkerMaxPayloadBytes) + `"}`} {
		if _, err := e.s.CreateJob(e.ctx, workspaceA, 0, "s", nil, json.RawMessage(p)); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("bad payload accepted: %.30s (%v)", p, err)
		}
	}
	if _, err := e.s.CreateJob(e.ctx, workspaceA, 0, "s", []string{"Bad Cap"}, nil); !errors.Is(err, behavior.ErrInvalid) {
		t.Errorf("bad capability accepted: %v", err)
	}
	if _, err := e.s.CreateJob(e.ctx, "not-a-uuid", 0, "s", nil, nil); !errors.Is(err, behavior.ErrInvalid) {
		t.Errorf("bad workspace accepted: %v", err)
	}
	ok := `{"repo":"o/r","task_id":"FC-1","branch":"nexus/FC-1-x","instruction":"sửa lỗi đăng nhập 🙂","max_tokens":5,"token_count":3}`
	j, err := e.s.CreateJob(e.ctx, workspaceA, 0, "s", []string{"repo.write"}, json.RawMessage(ok))
	if err != nil || j.State != behavior.WorkerJobWaiting {
		t.Fatalf("normal payload refused: %v", err)
	}
}

// T-33b (REQ-13): secret-looking names that do not end in the exact listed
// words must be refused as well (R-9: a payload never carries a secret).
func TestWorker7a_PayloadSecretLookingKeyVariants(t *testing.T) {
	e := newW7(t)
	for _, p := range []string{`{"secret_key":"x"}`, `{"token_value":"x"}`} {
		if _, err := e.s.CreateJob(e.ctx, workspaceA, 0, "s", nil, json.RawMessage(p)); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("secret-looking key accepted: %s", p)
		}
	}
}

var eventCodeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// T-35 (REQ-12): audit rows hold fixed codes only, never text from payloads or results.
func TestWorker7a_AuditHoldsCodesOnly(t *testing.T) {
	e := newW7(t)
	d, token := e.enroll(t, workspaceA, "laptop")
	j := e.job(t, workspaceA, nil, `{"instruction":"INSTRUCTION-TEXT-QQQ"}`)
	l := e.mustLease(t, d)
	e.s.ReportProgress(e.ctx, d, j.ID, l.LeaseToken, behavior.WorkerProgress{Seq: 1, Phase: "editing"})
	e.s.Heartbeat(e.ctx, d, j.ID, "nhl_bad")
	if _, _, err := e.s.ReportResult(e.ctx, d, j.ID, l.LeaseToken, "k", behavior.WorkerResult{Status: "failed", ReasonCode: "tests_failed", Summary: "SUMMARY-TEXT-QQQ", Branch: "nexus/x"}); err != nil {
		t.Fatal(err)
	}
	e.s.RevokeDevice(e.ctx, workspaceA, "laptop")
	rows, err := e.pool.Query(e.ctx, `SELECT kind, reason_code, e::text FROM worker_events e`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, reason, raw string
		if err := rows.Scan(&kind, &reason, &raw); err != nil {
			t.Fatal(err)
		}
		n++
		if !eventCodeRE.MatchString(kind) || (reason != "" && !eventCodeRE.MatchString(reason)) {
			t.Fatalf("non-code event %q/%q", kind, reason)
		}
		for _, banned := range []string{"INSTRUCTION-TEXT", "SUMMARY-TEXT", token, l.LeaseToken, "nexus/x"} {
			if strings.Contains(raw, banned) {
				t.Fatalf("event row carries %q", banned)
			}
		}
	}
	if n < 6 {
		t.Fatalf("expected the whole lifecycle audited, got %d rows", n)
	}
	evs, _ := e.s.ListEvents(e.ctx, workspaceA, 50)
	for _, ev := range evs {
		if ev.DeviceID == "" && ev.JobID == "" {
			t.Fatalf("event without subject: %+v", ev)
		}
	}
	if list, _ := e.s.ListEvents(e.ctx, workspaceB, 50); len(list) != 0 {
		t.Fatal("events leak across workspaces")
	}
}
