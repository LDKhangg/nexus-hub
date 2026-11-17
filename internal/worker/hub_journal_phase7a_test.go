package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

const secretToken = "nhw_TOPSECRETTOKENVALUE0123456789"

// ---- HTTP hub client (T-B-30 .. T-B-33) ----

// T-B-30 (REQ-15): status codes map to the retry or drop classes, and errors never carry the token or body.
func TestHTTPHub_StatusMapping(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"error":"` + secretToken + ` leaked body"}`))
	}))
	defer srv.Close()
	h := NewHTTPHub(srv.URL, secretToken, nil)
	want := map[int]error{
		409: ErrConflict, 404: ErrNotFound, 401: ErrUnauthorized,
		429: ErrRetry, 408: ErrRetry, 500: ErrRetry, 502: ErrRetry, 503: ErrRetry, 504: ErrRetry,
		400: ErrRejected, 403: ErrRejected, 413: ErrRejected, 422: ErrRejected,
	}
	for code, target := range want {
		status.Store(int32(code))
		err := h.Result(context.Background(), "job-1", "lease", "key", behavior.WorkerResult{Status: "succeeded"})
		if !errors.Is(err, target) {
			t.Errorf("HTTP %d -> %v, want %v", code, err, target)
		}
		if err != nil && (strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "leaked body") || strings.Contains(err.Error(), srv.URL)) {
			t.Errorf("HTTP %d error leaks: %v", code, err)
		}
	}
	// a dead hub is a retryable network error
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	if err := NewHTTPHub(url, secretToken, nil).Progress(context.Background(), "j", "l", behavior.WorkerProgress{Seq: 1, Phase: "editing"}); !errors.Is(err, ErrRetry) || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("network error: %v", err)
	}
}

// T-B-31 (REQ-15): requests carry the bearer token and idempotency key, escape the job id, and never follow redirects.
func TestHTTPHub_RequestShapeAndNoRedirect(t *testing.T) {
	var gotAuth, gotPath, gotCT string
	var gotBody map[string]any
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/worker/v1/jobs/redirect") {
			http.Redirect(w, r, evil.URL+"/steal", http.StatusTemporaryRedirect)
			return
		}
		gotAuth, gotPath, gotCT = r.Header.Get("Authorization"), r.RequestURI, r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := NewHTTPHub(srv.URL+"", secretToken, nil)
	if err := h.Result(context.Background(), "a/../b", "LEASE", "job:1:result", behavior.WorkerResult{Status: "failed", ReasonCode: "tests_failed"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+secretToken || gotCT != "application/json" {
		t.Fatalf("headers %q %q", gotAuth, gotCT)
	}
	if !strings.Contains(gotPath, "%2F") || strings.Contains(gotPath, "/../") {
		t.Fatalf("job id not escaped: %q", gotPath)
	}
	if gotBody["lease_token"] != "LEASE" || gotBody["idempotency_key"] != "job:1:result" || gotBody["status"] != "failed" || gotBody["reason_code"] != "tests_failed" {
		t.Fatalf("body %v", gotBody)
	}
	if refs, ok := gotBody["artifact_refs"].([]any); !ok || len(refs) != 0 {
		t.Fatalf("artifact_refs must be [] not null: %v", gotBody["artifact_refs"])
	}
	if err := h.Heartbeat2(t, "redirect"); err == nil || evilHits.Load() != 0 {
		t.Fatalf("redirect followed (hits=%d, err=%v)", evilHits.Load(), err)
	}
}

// Heartbeat2 is a tiny helper so the redirect case reads clearly.
func (h *HTTPHub) Heartbeat2(t *testing.T, jobID string) error {
	t.Helper()
	_, err := h.Heartbeat(context.Background(), jobID, "lease", nil)
	return err
}

// T-B-32 (REQ-2, REQ-18): poll asks for 55 s, 204 is no job, a lease without ids is retried, an oversized reply is retried.
func TestHTTPHub_PollContract(t *testing.T) {
	var mode atomic.Int32
	var wait atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Wait      int                       `json:"wait_seconds"`
			Telemetry *behavior.WorkerTelemetry `json:"telemetry"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		wait.Store(int64(body.Wait))
		switch mode.Load() {
		case 0:
			w.WriteHeader(http.StatusNoContent)
		case 1:
			_, _ = w.Write([]byte(`{"job_id":"j1","lease_token":"lt","attempt":2,"payload":{"repo":"o/r"},"heartbeat_interval_seconds":20}`))
		case 2:
			_, _ = w.Write([]byte(`{"attempt":2}`))
		case 3:
			_, _ = w.Write([]byte(`{"job_id":"j1","lease_token":"lt","payload":"` + strings.Repeat("x", 70<<10) + `"}`))
		}
	}))
	defer srv.Close()
	h := NewHTTPHub(srv.URL, secretToken, nil)
	if l, err := h.Poll(context.Background(), pollWait, &behavior.WorkerTelemetry{State: "idle"}); l != nil || err != nil || wait.Load() != 55 {
		t.Fatalf("204: %v %v wait=%d", l, err, wait.Load())
	}
	mode.Store(1)
	l, err := h.Poll(context.Background(), pollWait, nil)
	if err != nil || l == nil || l.JobID != "j1" || l.LeaseToken != "lt" || l.Attempt != 2 || l.HeartbeatIntervalSeconds != 20 {
		t.Fatalf("lease %+v %v", l, err)
	}
	for _, m := range []int32{2, 3} {
		mode.Store(m)
		if l, err := h.Poll(context.Background(), pollWait, nil); l != nil || !errors.Is(err, ErrRetry) {
			t.Fatalf("mode %d: %v %v", m, l, err)
		}
	}
}

// ---- ClickUp read (T-B-33) ----

// T-B-33 (REQ-5): the ClickUp read is GET only, sends the raw token, escapes the id, is bounded, and never follows a redirect.
func TestClickUpClient(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "cu-token")
	if err := os.WriteFile(tokFile, []byte("pk_clickup_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	var auths []string
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/task/ok1":
			_, _ = w.Write([]byte(`{"id":"ok1","name":"  Fix   the\nbug ","url":"https://app.clickup.com/t/ok1","description":"` + strings.Repeat("d", 20000) + `","status":{"status":"open"}}`))
		case "/api/v2/task/ok1/comment":
			var cs []string
			for i := 1; i <= 15; i++ {
				cs = append(cs, `{"comment_text":"c`+itoa(i)+`","date":"`+itoa(1000+i)+`","user":{"username":"u"}}`)
			}
			cs = append(cs, `{"comment_text":"   ","date":"1","user":{"username":"empty"}}`)
			// newest first, as ClickUp returns them
			for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
				cs[i], cs[j] = cs[j], cs[i]
			}
			_, _ = w.Write([]byte(`{"comments":[` + strings.Join(cs, ",") + `]}`))
		case "/api/v2/task/boom":
			w.WriteHeader(500)
		case "/api/v2/task/redir":
			http.Redirect(w, r, evil.URL+"/x", http.StatusFound)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &ClickUpClient{BaseURL: srv.URL, TokenFile: tokFile}
	task, err := c.FetchTask(context.Background(), "ok1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "Fix the bug" || utf8.RuneCountInString(task.Description) > maxTaskDescRunes || task.Status != "open" {
		t.Fatalf("task %q desc=%d", task.Name, utf8.RuneCountInString(task.Description))
	}
	if len(task.Comments) != maxRecentComments || task.Comments[0] != "u: c6" || task.Comments[len(task.Comments)-1] != "u: c15" {
		t.Fatalf("comments (oldest first, last 10): %v", task.Comments)
	}
	mu.Lock()
	for _, a := range auths {
		if a != "pk_clickup_secret" {
			t.Fatalf("Authorization %q", a)
		}
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("non-GET request %q", s)
		}
	}
	mu.Unlock()
	for _, id := range []string{"boom", "redir", "missing"} {
		if _, err := c.FetchTask(context.Background(), id); !errors.Is(err, ErrClickUp) {
			t.Errorf("%s: %v", id, err)
		}
	}
	if evilHits.Load() != 0 {
		t.Fatal("the ClickUp token was sent to a redirect target")
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	for _, id := range []string{"../../x", "a/b", "", "x y", strings.Repeat("a", 65)} {
		if _, err := c.FetchTask(context.Background(), id); !errors.Is(err, ErrClickUp) {
			t.Errorf("id %q: %v", id, err)
		}
	}
	mu.Lock()
	reached := len(seen) != n
	mu.Unlock()
	if reached {
		t.Fatal("a hostile task id reached the network")
	}
	// token problems never reach the network and never leak
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tokFile, 0o644)
		if _, err := c.FetchTask(context.Background(), "ok1"); !errors.Is(err, ErrClickUp) || strings.Contains(err.Error(), "pk_clickup_secret") {
			t.Fatalf("loose token file: %v", err)
		}
	}
	if _, err := (&ClickUpClient{BaseURL: srv.URL}).FetchTask(context.Background(), "ok1"); !errors.Is(err, ErrClickUp) {
		t.Fatalf("no token file: %v", err)
	}
	var nilClient *ClickUpClient
	if _, err := nilClient.FetchTask(context.Background(), "ok1"); !errors.Is(err, ErrClickUp) {
		t.Fatalf("nil client: %v", err)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---- journal (T-B-40 .. T-B-47) ----

func newTestJournal(t *testing.T, id string) (*Journal, string) {
	t.Helper()
	state := t.TempDir()
	j, err := CreateJournal(state, LeaseRecord{JobID: id, Attempt: 1, LeaseToken: "lease-token-1", HeartbeatIntervalSeconds: 30, LeasedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, state
}

// T-B-40 (REQ-15): job ids are checked, so a hub-chosen id cannot leave the state directory.
func TestJournal_RejectsHostileJobIDs(t *testing.T) {
	state := t.TempDir()
	for _, id := range []string{"", "../x", "a/b", "..", ".hidden", "a b", "-x", strings.Repeat("a", 65), "x\n", "ö", "a/../../b"} {
		if j, err := CreateJournal(state, LeaseRecord{JobID: id}, nil); err == nil {
			j.Close()
			t.Errorf("job id %q accepted", id)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(state))
	for _, e := range entries {
		if e.Name() == "x" || e.Name() == "b" {
			t.Fatalf("directory %q created outside the state dir", e.Name())
		}
	}
	if j, err := CreateJournal(state, LeaseRecord{JobID: "0123abcd-4567-89ef-0123-456789abcdef"}, nil); err != nil {
		t.Fatalf("uuid rejected: %v", err)
	} else {
		j.Close()
	}
}

// T-B-41 (REQ-15): the journal is private (0600 file, 0700 directory) and a result is written before it is sent.
func TestJournal_PrivateAndWrittenBeforeSend(t *testing.T) {
	j, state := newTestJournal(t, "job-private")
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(j.Dir(), "journal.jsonl"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("journal mode: %v %v", fi, err)
		}
		di, _ := os.Stat(j.Dir())
		if di.Mode().Perm()&0o077 != 0 {
			t.Fatalf("journal dir mode %v", di.Mode())
		}
	}
	if err := j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{FilesChanged: 2}); err != nil {
		t.Fatal(err)
	}
	if err := j.EnqueueResult(behavior.WorkerResult{Status: "succeeded", Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	// Before any send, a second reader of the file (a crash and restart) sees both entries in order.
	re, err := OpenJournal(j.Dir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	st := re.State()
	if len(st.Pending) != 2 || st.Pending[0].Progress == nil || st.Pending[1].Result == nil || st.Pending[1].Key != "job-private:1:result" || st.Result == nil {
		t.Fatalf("replayed state %+v", st)
	}
	// a hub that checks the file at send time
	hub := &journalHub{dir: j.Dir(), t: t}
	s := &Sender{Hub: hub, Sleep: fastSleep}
	if err := s.Flush(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if !hub.sawResultOnDisk || !hub.sawProgressOnDisk {
		t.Fatalf("entry was sent before it was on disk: progress=%v result=%v", hub.sawProgressOnDisk, hub.sawResultOnDisk)
	}
	if hub.order() != "progress,result" {
		t.Fatalf("order %q", hub.order())
	}
	_ = state
}

type journalHub struct {
	t                                  *testing.T
	dir                                string
	mu                                 sync.Mutex
	seq                                []string
	sawProgressOnDisk, sawResultOnDisk bool
}

func (h *journalHub) order() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.seq, ",")
}
func (h *journalHub) Poll(context.Context, time.Duration, *behavior.WorkerTelemetry) (*Lease, error) {
	return nil, nil
}
func (h *journalHub) Heartbeat(context.Context, string, string, *behavior.WorkerTelemetry) (HeartbeatReply, error) {
	return HeartbeatReply{}, nil
}
func (h *journalHub) Progress(_ context.Context, _, _ string, _ behavior.WorkerProgress) error {
	data, _ := os.ReadFile(filepath.Join(h.dir, "journal.jsonl"))
	h.mu.Lock()
	h.seq = append(h.seq, "progress")
	h.sawProgressOnDisk = strings.Contains(string(data), `"t":"progress"`)
	h.mu.Unlock()
	return nil
}
func (h *journalHub) Result(_ context.Context, _, _, key string, _ behavior.WorkerResult) error {
	data, _ := os.ReadFile(filepath.Join(h.dir, "journal.jsonl"))
	h.mu.Lock()
	h.seq = append(h.seq, "result")
	h.sawResultOnDisk = strings.Contains(string(data), `"t":"result"`) && strings.Contains(string(data), key)
	h.mu.Unlock()
	return nil
}

func fastSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
		return nil
	}
}

// T-B-42 (REQ-15): the result is bounded to the hub contract (2000 runes, valid UTF-8, github PR URL only).
func TestJournal_ResultIsBoundedToHubContract(t *testing.T) {
	j, _ := newTestJournal(t, "job-bound")
	res := behavior.WorkerResult{Status: "failed", ReasonCode: "tests_failed", Summary: strings.Repeat("ế", 6000) + "\xff", PRURL: "http://evil.example/x", Branch: "nexus/ok"}
	if err := j.EnqueueResult(res); err != nil {
		t.Fatal(err)
	}
	got := j.State().Result
	if got.Validate() != nil || utf8.RuneCountInString(got.Summary) > 2000 || !utf8.ValidString(got.Summary) || got.PRURL != "" {
		t.Fatalf("result not bounded: %v runes=%d pr=%q", got.Validate(), utf8.RuneCountInString(got.Summary), got.PRURL)
	}
	if got.ArtifactRefs == nil {
		t.Fatal("artifact_refs must not be null")
	}
}

// T-B-43 (REQ-15): one result per attempt, nothing after it, and the same idempotency key every time.
func TestJournal_OneResultAndOrder(t *testing.T) {
	j, _ := newTestJournal(t, "job-one")
	_ = j.EnqueueProgress(PhaseCloning, behavior.WorkerProgressCounts{})
	_ = j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{})
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded", Summary: "first"})
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "failed", ReasonCode: "executor_crash", Summary: "second"})
	_ = j.EnqueueProgress(PhaseTesting, behavior.WorkerProgressCounts{})
	st := j.State()
	if st.Result == nil || st.Result.Summary != "first" || len(st.Pending) != 3 {
		t.Fatalf("pending %d result %+v", len(st.Pending), st.Result)
	}
	if st.Pending[0].Progress.Seq != 1 || st.Pending[1].Progress.Seq != 2 || st.Pending[2].Result == nil {
		t.Fatalf("order %+v", st.Pending)
	}
	if st.Pending[2].Key != "job-one:1:result" {
		t.Fatalf("key %q", st.Pending[2].Key)
	}
}

// T-B-44 (REQ-15, REQ-16): a torn last line (a crash while writing) is ignored on replay.
func TestJournal_TornLastLine(t *testing.T) {
	j, _ := newTestJournal(t, "job-torn")
	_ = j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{})
	path := filepath.Join(j.Dir(), "journal.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"t":"result","n":9,"result":{"status":"succ`)
	f.Close()
	re, err := OpenJournal(j.Dir(), nil)
	if err != nil {
		t.Fatalf("replay failed on a torn line: %v", err)
	}
	defer re.Close()
	if st := re.State(); len(st.Pending) != 1 || st.Result != nil || st.Lease.JobID != "job-torn" {
		t.Fatalf("state %+v", st)
	}
	// The restarted worker keeps writing to the same file: its first record must
	// survive the next replay even though the last line was torn.
	if err := re.EnqueueResult(behavior.WorkerResult{Status: "failed", ReasonCode: "worker_restarted", Summary: "restart"}); err != nil {
		t.Fatal(err)
	}
	re2, err := OpenJournal(j.Dir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer re2.Close()
	if st := re2.State(); st.Result == nil || st.Result.ReasonCode != "worker_restarted" || len(st.Pending) != 2 {
		t.Fatalf("the first record written after a torn line was lost on replay: pending=%d result=%+v", len(st.Pending), st.Result)
	}
	// an empty or lease-less journal is not usable
	empty := filepath.Join(t.TempDir(), "x")
	_ = os.MkdirAll(empty, 0o700)
	_ = os.WriteFile(filepath.Join(empty, "journal.jsonl"), []byte("garbage\n"), 0o600)
	if _, err := OpenJournal(empty, nil); err == nil {
		t.Fatal("journal without a lease accepted")
	}
}

// T-B-45 (REQ-15): a new lease for an unfinished job keeps its journal but drops entries of the old token; a finished job starts fresh and keeps its worktree record.
func TestJournal_ReleaseAndDone(t *testing.T) {
	j, state := newTestJournal(t, "job-lease")
	_ = j.SetWorkspace(WorkspaceRecord{Repo: "o/r", Worktree: "/wt", Branch: "nexus/x-1", BaseSHA: strings.Repeat("a", 40)})
	_ = j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{})
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"})
	j.Close()
	j2, err := CreateJournal(state, LeaseRecord{JobID: "job-lease", Attempt: 2, LeaseToken: "lease-token-2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := j2.State()
	if st.Lease.LeaseToken != "lease-token-2" || len(st.Pending) != 0 || st.Result != nil || st.Workspace == nil {
		t.Fatalf("re-leased state %+v", st)
	}
	// replay of the same file gives the same view (the old result is not resurrected)
	re, err := OpenJournal(j2.Dir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := re.State(); st.Lease.LeaseToken != "lease-token-2" || len(st.Pending) != 0 || st.Result != nil {
		t.Fatalf("replayed %+v", st)
	}
	re.Close()
	if err := j2.MarkDone(); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	j3, err := CreateJournal(state, LeaseRecord{JobID: "job-lease", Attempt: 3, LeaseToken: "lease-token-3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer j3.Close()
	if st := j3.State(); st.Done || st.Workspace == nil || st.Workspace.Branch != "nexus/x-1" || st.Lease.Attempt != 3 {
		t.Fatalf("after done: %+v", st)
	}
}

// T-B-46 (REQ-15): an instruction id is delivered at most once, also after a restart.
func TestJournal_InstructionDedupe(t *testing.T) {
	j, _ := newTestJournal(t, "job-instr")
	if fresh, err := j.MarkInstruction("v2"); !fresh || err != nil {
		t.Fatalf("first: %v %v", fresh, err)
	}
	if fresh, _ := j.MarkInstruction("v2"); fresh {
		t.Fatal("second delivery counted as fresh")
	}
	re, _ := OpenJournal(j.Dir(), nil)
	defer re.Close()
	if fresh, _ := re.MarkInstruction("v2"); fresh {
		t.Fatal("instruction delivered again after restart")
	}
	data, _ := os.ReadFile(filepath.Join(j.Dir(), "journal.jsonl"))
	if strings.Contains(string(data), "text") {
		t.Fatal("instruction text must not be journaled")
	}
}

// ---- sender and backoff (T-B-48 .. T-B-52) ----

type scriptHub struct {
	mu       sync.Mutex
	progress []int64
	results  []string // idempotency keys
	failFor  func(op string, n int) error
	calls    map[string]int
}

func (h *scriptHub) Poll(context.Context, time.Duration, *behavior.WorkerTelemetry) (*Lease, error) {
	return nil, nil
}
func (h *scriptHub) Heartbeat(context.Context, string, string, *behavior.WorkerTelemetry) (HeartbeatReply, error) {
	return HeartbeatReply{}, nil
}
func (h *scriptHub) count(op string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls == nil {
		h.calls = map[string]int{}
	}
	h.calls[op]++
	return h.calls[op]
}
func (h *scriptHub) Progress(_ context.Context, _, _ string, p behavior.WorkerProgress) error {
	n := h.count("progress")
	if h.failFor != nil {
		if err := h.failFor("progress", n); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.progress = append(h.progress, p.Seq)
	h.mu.Unlock()
	return nil
}
func (h *scriptHub) Result(_ context.Context, _, _, key string, _ behavior.WorkerResult) error {
	n := h.count("result")
	if h.failFor != nil {
		if err := h.failFor("result", n); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.results = append(h.results, key)
	h.mu.Unlock()
	return nil
}

// T-B-48 (REQ-15): network loss, 429 and 5xx retry the same entry with the same key until accepted, in order.
func TestSender_RetriesInOrderWithSameKey(t *testing.T) {
	j, _ := newTestJournal(t, "job-retry")
	_ = j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{})
	_ = j.EnqueueProgress(PhasePushing, behavior.WorkerProgressCounts{})
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"})
	hub := &scriptHub{failFor: func(op string, n int) error {
		if n <= 3 {
			return []error{ErrRetry, ErrRetry, ErrRetry}[n-1]
		}
		return nil
	}}
	var sleeps []time.Duration
	s := &Sender{Hub: hub, Back: Backoff{Base: time.Second, Max: time.Minute, Rand: func(int64) int64 { return 0 }},
		Sleep: func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }}
	if err := s.Flush(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if len(hub.progress) != 2 || hub.progress[0] != 1 || hub.progress[1] != 2 || len(hub.results) != 1 || hub.results[0] != "job-retry:1:result" {
		t.Fatalf("progress %v results %v", hub.progress, hub.results)
	}
	if hub.calls["progress"] != 5 || hub.calls["result"] != 1 {
		t.Fatalf("calls %v (progress 1 must be tried 4 times, then the rest once)", hub.calls)
	}
	if len(sleeps) != 3 || !(sleeps[0] < sleeps[1] && sleeps[1] < sleeps[2]) {
		t.Fatalf("backoff must grow: %v", sleeps)
	}
	if len(j.State().Pending) != 0 {
		t.Fatal("outbox not empty after success")
	}
}

// T-B-49 (REQ-15, REQ-16): 409, 404 and 400 drop an entry that can never be accepted; 401 keeps retrying.
func TestSender_DropsPermanentErrorsRetriesUnauthorized(t *testing.T) {
	for _, drop := range []error{ErrConflict, ErrNotFound, ErrRejected, errors.Join(ErrRejected, errors.New("HTTP 400"))} {
		j, _ := newTestJournal(t, "job-drop")
		_ = j.EnqueueProgress(PhaseEditing, behavior.WorkerProgressCounts{})
		_ = j.EnqueueResult(behavior.WorkerResult{Status: "failed", ReasonCode: "worker_restarted"})
		hub := &scriptHub{failFor: func(op string, n int) error { return drop }}
		s := &Sender{Hub: hub, Sleep: fastSleep}
		if err := s.Flush(context.Background(), j); err != nil {
			t.Fatalf("%v: %v", drop, err)
		}
		if hub.calls["progress"] != 1 || hub.calls["result"] != 1 || len(j.State().Pending) != 0 {
			t.Fatalf("%v: calls %v pending %d (each entry must be tried once and dropped)", drop, hub.calls, len(j.State().Pending))
		}
		j.Close()
	}
	j, _ := newTestJournal(t, "job-401")
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"})
	hub := &scriptHub{failFor: func(op string, n int) error {
		if n < 4 {
			return ErrUnauthorized
		}
		return nil
	}}
	if err := (&Sender{Hub: hub, Sleep: fastSleep}).Flush(context.Background(), j); err != nil || hub.calls["result"] != 4 || len(hub.results) != 1 {
		t.Fatalf("401 must retry, not drop: calls %v err %v", hub.calls, err)
	}
}

// T-B-50 (REQ-15): shutdown stops the sender and leaves the entry in the journal for the next start.
func TestSender_ShutdownKeepsOutbox(t *testing.T) {
	j, _ := newTestJournal(t, "job-stop")
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"})
	ctx, cancel := context.WithCancel(context.Background())
	hub := &scriptHub{failFor: func(string, int) error { cancel(); return ErrRetry }}
	err := (&Sender{Hub: hub}).Flush(ctx, j)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("flush: %v", err)
	}
	re, _ := OpenJournal(j.Dir(), nil)
	defer re.Close()
	if len(re.State().Pending) != 1 {
		t.Fatal("the unsent result was lost on shutdown")
	}
}

// T-B-51 (REQ-15): the running sender wakes on a new entry and delivers it.
func TestSender_RunDeliversNewEntries(t *testing.T) {
	j, _ := newTestJournal(t, "job-run")
	hub := &scriptHub{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Sender{Hub: hub, Sleep: fastSleep}).Run(ctx, j); close(done) }()
	_ = j.EnqueueProgress(PhaseCloning, behavior.WorkerProgressCounts{})
	_ = j.EnqueueResult(behavior.WorkerResult{Status: "succeeded"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		n := len(hub.results)
		hub.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(hub.results) != 1 || len(hub.progress) != 1 {
		t.Fatalf("progress %v results %v", hub.progress, hub.results)
	}
}

// T-B-52 (REQ-2): backoff is jittered, grows, is capped, and resets.
func TestBackoff(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, Rand: func(n int64) int64 { return n - 1 }} // top of the jitter range
	var last time.Duration
	for i := 0; i < 12; i++ {
		d := b.Next()
		if d < last || d > time.Minute {
			t.Fatalf("step %d: %v (last %v)", i, d, last)
		}
		last = d
	}
	if last != time.Minute {
		t.Fatalf("cap not reached: %v", last)
	}
	b.Reset()
	if d := b.Next(); d > time.Second {
		t.Fatalf("reset: %v", d)
	}
	low := Backoff{Base: time.Second, Max: time.Minute, Rand: func(int64) int64 { return 0 }}
	if d := low.Next(); d != 500*time.Millisecond {
		t.Fatalf("full jitter floor is half the window: %v", d)
	}
	var zero Backoff
	for i := 0; i < 40; i++ {
		if d := zero.Next(); d <= 0 || d > time.Minute {
			t.Fatalf("zero-value backoff step %d: %v", i, d)
		}
	}
}
