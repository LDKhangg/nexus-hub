package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Journal record kinds.
const (
	recLease     = "lease"
	recWorkspace = "workspace"
	recProgress  = "progress"
	recResult    = "result"
	recAck       = "ack"
	recInstr     = "instr"
	recDone      = "done"
	recCleaned   = "cleaned"
)

const maxJournalLine = 256 << 10

var jobIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// LeaseRecord is the lease a job runs under. The lease token is this
// device's own credential for the job; the journal file is mode 0600.
type LeaseRecord struct {
	JobID                    string    `json:"job_id"`
	Attempt                  int       `json:"attempt"`
	LeaseToken               string    `json:"lease_token"`
	HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
	Payload                  []byte    `json:"payload,omitempty"`
	LeasedAt                 time.Time `json:"leased_at"`
}

// WorkspaceRecord says where a job's worktree is.
type WorkspaceRecord struct {
	Repo          string `json:"repo"`
	Clone         string `json:"clone"`
	Worktree      string `json:"worktree"`
	Branch        string `json:"branch"`
	DefaultBranch string `json:"default_branch"`
	BaseSHA       string `json:"base_sha"`
}

type record struct {
	T         string                   `json:"t"`
	N         int64                    `json:"n,omitempty"`
	At        time.Time                `json:"at"`
	Lease     *LeaseRecord             `json:"lease,omitempty"`
	Workspace *WorkspaceRecord         `json:"workspace,omitempty"`
	Progress  *behavior.WorkerProgress `json:"progress,omitempty"`
	Result    *behavior.WorkerResult   `json:"result,omitempty"`
	Key       string                   `json:"key,omitempty"`
	Dropped   bool                     `json:"dropped,omitempty"`
	InstrID   string                   `json:"instr_id,omitempty"`
}

// outboxEntry is one progress event or result waiting to be accepted.
type outboxEntry struct {
	N        int64
	Progress *behavior.WorkerProgress
	Result   *behavior.WorkerResult
	Key      string
}

// JobState is a job's journal replayed.
type JobState struct {
	Lease     LeaseRecord
	Workspace *WorkspaceRecord
	Pending   []outboxEntry // in order, not yet accepted or dropped
	LastSeq   int64
	LastN     int64
	Result    *behavior.WorkerResult // queued result, if any
	Instrs    map[string]bool
	Done      bool
	DoneAt    time.Time
	Cleaned   bool
}

// Journal is the append-only, fsynced record of one job: its lease, its
// workspace and its outbox. Every progress event and result is written
// here before it is sent (REQ-15).
type Journal struct {
	mu    sync.Mutex
	dir   string
	f     *os.File
	state JobState
	wake  chan struct{}
	now   func() time.Time
}

func jobsDir(stateDir string) string { return filepath.Join(stateDir, "jobs") }

// CreateJournal starts the journal of a new lease. An existing journal of
// the same job (an earlier attempt or a reissued lease) is replaced only
// when that job is done; otherwise the new lease is appended to it.
func CreateJournal(stateDir string, l LeaseRecord, now func() time.Time) (*Journal, error) {
	if !jobIDRE.MatchString(l.JobID) {
		return nil, errors.New("invalid job id")
	}
	dir := filepath.Join(jobsDir(stateDir), l.JobID)
	if j, err := OpenJournal(dir, now); err == nil {
		if !j.state.Done {
			j.state.Pending = nil // a new lease token: old entries can no longer be accepted
			j.state.Result = nil
			if err := j.append(record{T: recLease, Lease: &l}); err != nil {
				j.Close()
				return nil, err
			}
			j.state.Lease = l
			return j, nil
		}
		ws := j.state.Workspace
		j.Close()
		if err := os.Remove(filepath.Join(dir, "journal.jsonl")); err != nil {
			return nil, errors.New("journal not writable")
		}
		j2, err := newJournal(dir, l, now)
		if err == nil && ws != nil {
			_ = j2.append(record{T: recWorkspace, Workspace: ws})
			j2.state.Workspace = ws
		}
		return j2, err
	}
	return newJournal(dir, l, now)
}

func newJournal(dir string, l LeaseRecord, now func() time.Time) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("state directory not writable")
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.New("journal not writable")
	}
	j := &Journal{dir: dir, f: f, wake: make(chan struct{}, 1), now: now, state: JobState{Lease: l, Instrs: map[string]bool{}}}
	if err := j.append(record{T: recLease, Lease: &l}); err != nil {
		f.Close()
		return nil, err
	}
	syncDir(dir)
	return j, nil
}

// OpenJournal replays an existing job journal. A torn last line (a crash
// mid-write) is ignored.
func OpenJournal(dir string, now func() time.Time) (*Journal, error) {
	path := filepath.Join(dir, "journal.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := JobState{Instrs: map[string]bool{}}
	pending := map[int64]outboxEntry{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxJournalLine)
	seenLease := false
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.N > st.LastN {
			st.LastN = r.N
		}
		switch r.T {
		case recLease:
			if r.Lease != nil {
				st.Lease = *r.Lease
				seenLease = true
				pending = map[int64]outboxEntry{}
				st.Result = nil
			}
		case recWorkspace:
			st.Workspace = r.Workspace
		case recProgress:
			if r.Progress != nil {
				pending[r.N] = outboxEntry{N: r.N, Progress: r.Progress}
				if r.Progress.Seq > st.LastSeq {
					st.LastSeq = r.Progress.Seq
				}
			}
		case recResult:
			if r.Result != nil {
				pending[r.N] = outboxEntry{N: r.N, Result: r.Result, Key: r.Key}
				st.Result = r.Result
			}
		case recAck:
			delete(pending, r.N)
		case recInstr:
			st.Instrs[r.InstrID] = true
		case recDone:
			st.Done, st.DoneAt = true, r.At
		case recCleaned:
			st.Cleaned = true
		}
	}
	if !seenLease {
		return nil, errors.New("journal has no lease")
	}
	for _, e := range pending {
		st.Pending = append(st.Pending, e)
	}
	sort.Slice(st.Pending, func(a, b int) bool { return st.Pending[a].N < st.Pending[b].N })
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.New("journal not writable")
	}
	if now == nil {
		now = time.Now
	}
	return &Journal{dir: dir, f: f, state: st, wake: make(chan struct{}, 1), now: now}, nil
}

// append writes one record and fsyncs it. Caller holds mu or owns j.
func (j *Journal) append(r record) error {
	if j.now == nil {
		j.now = time.Now
	}
	r.At = j.now().UTC()
	b, err := json.Marshal(r)
	if err != nil {
		return errors.New("journal record not encodable")
	}
	b = append(b, '\n')
	if _, err := j.f.Write(b); err != nil {
		return errors.New("journal write failed")
	}
	if err := j.f.Sync(); err != nil {
		return errors.New("journal sync failed")
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Close closes the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// State returns a copy of the replayed state.
func (j *Journal) State() JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.state
	st.Pending = append([]outboxEntry(nil), j.state.Pending...)
	st.Instrs = map[string]bool{}
	for k := range j.state.Instrs {
		st.Instrs[k] = true
	}
	return st
}

// Dir is the job's state directory.
func (j *Journal) Dir() string { return j.dir }

// SetWorkspace records where the job's worktree is.
func (j *Journal) SetWorkspace(w WorkspaceRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.append(record{T: recWorkspace, Workspace: &w}); err != nil {
		return err
	}
	j.state.Workspace = &w
	return nil
}

// MarkInstruction records that an instruction id was delivered to the
// executor (the text is not journaled).
func (j *Journal) MarkInstruction(id string) (fresh bool, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Instrs[id] {
		return false, nil
	}
	if err := j.append(record{T: recInstr, InstrID: id}); err != nil {
		return false, err
	}
	j.state.Instrs[id] = true
	return true, nil
}

// EnqueueProgress journals a progress event with the next seq.
func (j *Journal) EnqueueProgress(phase string, c behavior.WorkerProgressCounts) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Result != nil {
		return nil // nothing after the result
	}
	p := behavior.WorkerProgress{Seq: j.state.LastSeq + 1, Phase: phase, Counts: c}
	n := j.state.LastN + 1
	if err := j.append(record{T: recProgress, N: n, Progress: &p}); err != nil {
		return err
	}
	j.state.LastSeq, j.state.LastN = p.Seq, n
	j.state.Pending = append(j.state.Pending, outboxEntry{N: n, Progress: &p})
	j.notify()
	return nil
}

// EnqueueResult journals the job's one result. A second call is ignored.
func (j *Journal) EnqueueResult(r behavior.WorkerResult) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Result != nil {
		return nil
	}
	r = boundResult(r)
	key := idempotencyKey(j.state.Lease.JobID, j.state.Lease.Attempt)
	n := j.state.LastN + 1
	if err := j.append(record{T: recResult, N: n, Result: &r, Key: key}); err != nil {
		return err
	}
	j.state.LastN = n
	j.state.Result = &r
	j.state.Pending = append(j.state.Pending, outboxEntry{N: n, Result: &r, Key: key})
	j.notify()
	return nil
}

func (j *Journal) notify() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

func (j *Journal) ack(n int64, dropped bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.append(record{T: recAck, N: n, Dropped: dropped}); err != nil {
		return err
	}
	for i, e := range j.state.Pending {
		if e.N == n {
			j.state.Pending = append(j.state.Pending[:i], j.state.Pending[i+1:]...)
			break
		}
	}
	return nil
}

// MarkDone records that the job is over for this device.
func (j *Journal) MarkDone() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Done {
		return nil
	}
	if err := j.append(record{T: recDone}); err != nil {
		return err
	}
	j.state.Done, j.state.DoneAt = true, j.now().UTC()
	return nil
}

// MarkCleaned records that the worktree was removed.
func (j *Journal) MarkCleaned() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.append(record{T: recCleaned}); err != nil {
		return err
	}
	j.state.Cleaned = true
	return nil
}

func (j *Journal) next() (outboxEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.state.Pending) == 0 {
		return outboxEntry{}, false
	}
	return j.state.Pending[0], true
}

// Backoff computes retry delays with full jitter (W6).
type Backoff struct {
	Base, Max time.Duration
	Rand      func(n int64) int64 // [0, n); nil uses a time-seeded source
	attempt   int
}

// Next returns the next delay and grows the window.
func (b *Backoff) Next() time.Duration {
	base, max := b.Base, b.Max
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = time.Minute
	}
	window := base << min(b.attempt, 16)
	if window > max || window <= 0 {
		window = max
	}
	b.attempt++
	r := b.Rand
	if r == nil {
		r = jitter
	}
	return window/2 + time.Duration(r(int64(window/2)+1))
}

// Reset starts over after a success.
func (b *Backoff) Reset() { b.attempt = 0 }

func jitter(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return time.Now().UnixNano() % n
}

// Sender delivers a journal's outbox in order (REQ-15). Each entry is
// retried with backoff until the hub accepts it; a 409/404/400 answer
// means it can never be accepted (stale lease, finished job), so it is
// dropped.
type Sender struct {
	Hub   Hub
	Sleep func(ctx context.Context, d time.Duration) error
	Back  Backoff
}

// Flush sends every pending entry. It returns nil when the outbox is empty
// and ctx.Err() when ctx ends first.
func (s *Sender) Flush(ctx context.Context, j *Journal) error {
	for {
		e, ok := j.next()
		if !ok {
			return nil
		}
		token := j.State().Lease.LeaseToken
		jobID := j.State().Lease.JobID
		var err error
		if e.Result != nil {
			err = s.Hub.Result(ctx, jobID, token, e.Key, *e.Result)
		} else {
			err = s.Hub.Progress(ctx, jobID, token, *e.Progress)
		}
		switch {
		case err == nil:
			s.Back.Reset()
			if aerr := j.ack(e.N, false); aerr != nil {
				return aerr
			}
		case errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound), errors.Is(err, ErrRejected):
			if e.Result != nil {
				log.Print("worker: result not accepted by the hub (lease lost or job finished); dropped")
			}
			if aerr := j.ack(e.N, true); aerr != nil {
				return aerr
			}
		default:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrUnauthorized) {
				log.Print("worker: device token rejected; outbox will retry")
			}
			if serr := s.sleep(ctx, s.Back.Next()); serr != nil {
				return serr
			}
		}
	}
}

// Run flushes whenever the journal gets a new entry, until ctx ends.
func (s *Sender) Run(ctx context.Context, j *Journal) {
	for {
		if s.Flush(ctx, j) != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-j.wake:
		}
	}
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ListJournals opens every job journal under stateDir.
func ListJournals(stateDir string, now func() time.Time) ([]*Journal, error) {
	entries, err := os.ReadDir(jobsDir(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("state directory not readable")
	}
	var out []*Journal
	for _, e := range entries {
		if !e.IsDir() || !jobIDRE.MatchString(e.Name()) {
			continue
		}
		j, err := OpenJournal(filepath.Join(jobsDir(stateDir), e.Name()), now)
		if err != nil {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}
