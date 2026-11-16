package storage

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/labstack/echo/v4"
)

// w7Hub serves the real worker handlers over the real store.
type w7Hub struct {
	*w7Env
	srv *httptest.Server
}

func newW7Hub(t *testing.T) *w7Hub {
	t.Helper()
	e := newW7(t)
	h := httpdelivery.NewWorkerHandler(e.s, e.s).WithLeases(e.s, nil)
	ec := echo.New()
	h.Register(ec)
	srv := httptest.NewServer(ec)
	t.Cleanup(srv.Close)
	return &w7Hub{w7Env: e, srv: srv}
}

type hubResp struct {
	code int
	body map[string]any
	raw  string
	hdr  http.Header
}

func (h *w7Hub) do(t *testing.T, path, token string, body any) hubResp {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := hubResp{code: resp.StatusCode, raw: string(raw), hdr: resp.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (h *w7Hub) poll(t *testing.T, token string, wait int, tel map[string]any) hubResp {
	body := map[string]any{"wait_seconds": wait}
	if tel != nil {
		body["telemetry"] = tel
	}
	return h.do(t, "/worker/v1/poll", token, body)
}

const zeroJob = "00000000-0000-0000-0000-000000000000"

func jobPaths(id string) []string {
	return []string{"/worker/v1/poll", "/worker/v1/jobs/" + id + "/heartbeat", "/worker/v1/jobs/" + id + "/progress", "/worker/v1/jobs/" + id + "/result"}
}

// T-40 (REQ-13): every endpoint answers 401 without a valid device token and never echoes it.
func TestWorkerHTTP7a_Unauthorized(t *testing.T) {
	h := newW7Hub(t)
	_, token := h.enroll(t, workspaceA, "laptop")
	paths := append(jobPaths(zeroJob), "/worker/v1/unknown", "/worker/v1/jobs/"+zeroJob+"/other")
	for _, p := range paths {
		for name, tok := range map[string]string{"none": "", "garbage": "nhw_garbage", "sql": "' OR '1'='1", "truncated": token[:len(token)-2], "long": strings.Repeat("z", 5000)} {
			r := h.do(t, p, tok, `{}`)
			if r.code != 401 || r.raw != "{\"error\":\"unauthorized\"}\n" {
				t.Fatalf("%s with %s token: %d %q", p, name, r.code, r.raw)
			}
			if tok != "" && strings.Contains(r.raw, tok) {
				t.Fatal("token echoed")
			}
		}
		// wrong scheme
		for _, hdr := range []string{"Token " + token, "Basic " + token, "Bearer", token} {
			req, _ := http.NewRequest("POST", h.srv.URL+p, strings.NewReader(`{}`))
			req.Header.Set("Authorization", hdr)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("%s with header %q: %d", p, strings.Fields(hdr)[0], resp.StatusCode)
			}
		}
		// token in the query string is not credentials
		req, _ := http.NewRequest("POST", h.srv.URL+p+"?token="+token, strings.NewReader(`{}`))
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("query token accepted on %s: %d", p, resp.StatusCode)
		}
	}
	// unknown path with a valid token is not a success
	if r := h.do(t, "/worker/v1/unknown", token, `{}`); r.code == 200 || r.code == 204 {
		t.Fatalf("unknown path: %d", r.code)
	}
}

// T-41 (REQ-13): a revoked token gets 401 on every endpoint, also mid-job, and cannot change the job.
func TestWorkerHTTP7a_RevokedTokenDeadEverywhere(t *testing.T) {
	h := newW7Hub(t)
	dev, token := h.enroll(t, workspaceA, "laptop")
	j := h.job(t, workspaceA, nil, "")
	l := h.mustLease(t, dev)
	if err := h.s.RevokeDevice(h.ctx, workspaceA, "laptop"); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]any{
		"/worker/v1/poll":                        map[string]any{"wait_seconds": 0},
		"/worker/v1/jobs/" + j.ID + "/heartbeat": map[string]any{"lease_token": l.LeaseToken},
		"/worker/v1/jobs/" + j.ID + "/progress":  map[string]any{"lease_token": l.LeaseToken, "seq": 1, "phase": "editing"},
		"/worker/v1/jobs/" + j.ID + "/result":    map[string]any{"lease_token": l.LeaseToken, "idempotency_key": "k", "status": "succeeded"},
	}
	for p, b := range bodies {
		if r := h.do(t, p, token, b); r.code != 401 {
			t.Fatalf("revoked token on %s: %d", p, r.code)
		}
	}
	if st := h.jobState(t, j.ID); st != behavior.WorkerJobLeased {
		t.Fatalf("revoked device changed the job: %s", st)
	}
	// the job is not lost: the lease lapses and the owner sees INTERRUPTED
	h.advance(11 * time.Minute)
	if n, _ := h.s.ExpireLeases(h.ctx); n != 1 {
		t.Fatal("lease of a revoked device did not expire")
	}
}

// T-42 (REQ-13): bodies above 64 KiB are refused with 413 on every endpoint.
func TestWorkerHTTP7a_BodyLimit(t *testing.T) {
	h := newW7Hub(t)
	dev, token := h.enroll(t, workspaceA, "laptop")
	h.job(t, workspaceA, nil, "")
	l := h.mustLease(t, dev)
	big := []byte(`{"lease_token":"` + l.LeaseToken + `","summary":"` + strings.Repeat("a", 70<<10) + `"}`)
	for _, p := range jobPaths(zeroJob) {
		if r := h.do(t, p, token, big); r.code != 413 {
			t.Fatalf("%s: %d", p, r.code)
		}
	}
	if r := h.do(t, "/worker/v1/enroll", "", big); r.code != 413 {
		t.Fatalf("enroll: %d", r.code)
	}
	// just under the limit is parsed (and refused for content, not size)
	under := []byte(`{"lease_token":"x","pad":"` + strings.Repeat("a", 60<<10) + `"}`)
	if r := h.do(t, "/worker/v1/jobs/"+zeroJob+"/heartbeat", token, under); r.code == 413 {
		t.Fatal("60 KiB body refused as too large")
	}
}

// T-43 (REQ-13): a burst from one device is rate limited; another device is unaffected.
func TestWorkerHTTP7a_RateLimit(t *testing.T) {
	h := newW7Hub(t)
	_, tokA := h.enroll(t, workspaceA, "noisy")
	_, tokB := h.enroll(t, workspaceA, "quiet")
	limited, retryAfter := 0, ""
	for i := 0; i < 60; i++ {
		r := h.poll(t, tokA, 0, nil)
		if r.code == 429 {
			limited++
			retryAfter = r.hdr.Get("Retry-After")
		} else if r.code != 204 {
			t.Fatalf("poll %d: %d", i, r.code)
		}
	}
	if limited == 0 || retryAfter == "" {
		t.Fatalf("no rate limit after 60 rapid polls (limited=%d)", limited)
	}
	if r := h.poll(t, tokB, 0, nil); r.code != 204 {
		t.Fatalf("other device throttled: %d", r.code)
	}
}

// T-43b (REQ-13): enrollment guessing is rate limited.
func TestWorkerHTTP7a_EnrollRateLimit(t *testing.T) {
	h := newW7Hub(t)
	codes := map[int]int{}
	for i := 0; i < 25; i++ {
		r := h.do(t, "/worker/v1/enroll", "", map[string]any{"code": "AAAA-BBBB-CCCC", "name": "x"})
		codes[r.code]++
	}
	if codes[403] == 0 || codes[429] == 0 || codes[200] != 0 {
		t.Fatalf("enroll guesses: %v", codes)
	}
}

// T-44 (REQ-1): enrollment answers with fixed messages and does not burn the code on mistakes.
func TestWorkerHTTP7a_EnrollCodes(t *testing.T) {
	h := newW7Hub(t)
	code, _, _ := h.s.CreateEnrollCode(h.ctx, workspaceA, "owner")
	enroll := func(c, n string) hubResp {
		return h.do(t, "/worker/v1/enroll", "", map[string]any{"code": c, "name": n, "capabilities": []string{"repo.write"}})
	}
	if r := enroll("AAAA-BBBB-CCCC", "laptop"); r.code != 403 || r.body["error"] != "enrollment code is not valid" || strings.Contains(r.raw, "AAAA") {
		t.Fatalf("bad code: %d %s", r.code, r.raw)
	}
	for _, body := range []string{`{`, `{"code":"` + code + `"}`, `{"name":"x"}`, `{"code":"` + code + `","name":"x"} {}`} {
		if r := h.do(t, "/worker/v1/enroll", "", body); r.code != 400 {
			t.Fatalf("body %q: %d", body, r.code)
		}
	}
	if r := enroll(code, "my laptop"); r.code != 400 {
		t.Fatalf("bad name: %d", r.code)
	}
	r := enroll(code, "laptop")
	if r.code != 200 || !strings.HasPrefix(r.body["token"].(string), "nhw_") || r.hdr.Get("Cache-Control") != "no-store" || r.body["device_id"] == "" {
		t.Fatalf("enroll: %d %s", r.code, r.raw)
	}
	token := r.body["token"].(string)
	if r := enroll(code, "again"); r.code != 403 {
		t.Fatalf("code reuse: %d", r.code)
	}
	code2, _, _ := h.s.CreateEnrollCode(h.ctx, workspaceA, "owner")
	if r := enroll(code2, "laptop"); r.code != 409 || r.body["error"] != "device name is already in use" {
		t.Fatalf("clash: %d %s", r.code, r.raw)
	}
	if r := enroll(code2, "laptop-2"); r.code != 200 {
		t.Fatalf("code burned by 409: %d", r.code)
	}
	if r := h.poll(t, token, 0, nil); r.code != 204 {
		t.Fatalf("fresh token cannot poll: %d", r.code)
	}
}

// T-45 (REQ-2, 4): long poll returns 204 on short waits, rejects bad waits, wakes on job creation.
func TestWorkerHTTP7a_LongPoll(t *testing.T) {
	h := newW7Hub(t)
	_, token := h.enroll(t, workspaceA, "laptop", "repo.write")
	start := time.Now()
	if r := h.poll(t, token, 1, nil); r.code != 204 || time.Since(start) < 800*time.Millisecond || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("1 s poll: %d after %v", r.code, time.Since(start))
	}
	for _, w := range []int{56, 600, -1} {
		if r := h.poll(t, token, w, nil); r.code != 400 {
			t.Fatalf("wait_seconds=%d: %d", w, r.code)
		}
	}
	if r := h.do(t, "/worker/v1/poll", token, `{"wait_seconds":"soon"}`); r.code != 400 {
		t.Fatalf("string wait: %d", r.code)
	}
	// wake on job creation, well before the 3 s recheck
	done := make(chan hubResp, 1)
	began := time.Now()
	go func() { done <- h.poll(t, token, 20, nil) }()
	time.Sleep(400 * time.Millisecond)
	j := h.job(t, workspaceA, []string{"repo.write"}, `{"repo":"o/r"}`)
	select {
	case r := <-done:
		if r.code != 200 || r.body["job_id"] != j.ID || time.Since(began) > 2200*time.Millisecond {
			t.Fatalf("wake: %d %s after %v", r.code, r.raw, time.Since(began))
		}
		if r.body["lease_token"] == "" || r.body["heartbeat_interval_seconds"].(float64) != 20 || r.body["lease_seconds"].(float64) != 600 {
			t.Fatalf("lease fields: %s", r.raw)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("long poll did not wake")
	}
}

// T-46 (REQ-3, 12): poll, heartbeat, stale and foreign answers over HTTP.
func TestWorkerHTTP7a_JourneyPollHeartbeat(t *testing.T) {
	h := newW7Hub(t)
	_, tok := h.enroll(t, workspaceA, "laptop", "repo.write")
	_, tokOther := h.enroll(t, workspaceA, "other", "repo.write")
	payload := `{"repo":"o/r","instruction":"sửa lỗi 🙂"}`
	j := h.job(t, workspaceA, []string{"repo.write"}, payload)
	r := h.poll(t, tok, 0, nil)
	if r.code != 200 || r.body["job_id"] != j.ID || r.body["attempt"].(float64) != 1 {
		t.Fatalf("poll: %d %s", r.code, r.raw)
	}
	var echoed map[string]any
	pj, _ := json.Marshal(r.body["payload"])
	json.Unmarshal(pj, &echoed)
	if echoed["repo"] != "o/r" || echoed["instruction"] != "sửa lỗi 🙂" {
		t.Fatalf("payload: %s", pj)
	}
	lt := r.body["lease_token"].(string)
	base := "/worker/v1/jobs/" + j.ID
	if r := h.poll(t, tokOther, 0, nil); r.code != 204 {
		t.Fatalf("second device polled the leased job: %d", r.code)
	}
	if r := h.poll(t, tok, 0, nil); r.code != 204 {
		t.Fatalf("busy device got another job: %d", r.code)
	}
	hb := h.do(t, base+"/heartbeat", tok, map[string]any{"lease_token": lt})
	if hb.code != 200 || hb.body["cancel"] != false || hb.body["instructions"] == nil {
		t.Fatalf("heartbeat: %d %s", hb.code, hb.raw)
	}
	if r := h.do(t, base+"/heartbeat", tok, map[string]any{"lease_token": "nhl_stale"}); r.code != 409 {
		t.Fatalf("stale heartbeat: %d", r.code)
	}
	if r := h.do(t, base+"/heartbeat", tok, map[string]any{}); r.code != 400 {
		t.Fatalf("heartbeat without lease token: %d", r.code)
	}
	body := map[string]any{"lease_token": lt, "seq": 1, "phase": "editing", "idempotency_key": "k", "status": "succeeded"}
	for _, p := range []string{"/heartbeat", "/progress", "/result"} {
		if r := h.do(t, base+p, tokOther, body); r.code != 404 {
			t.Fatalf("foreign device on %s: %d", p, r.code)
		}
		if r := h.do(t, "/worker/v1/jobs/not-a-job"+p, tok, body); r.code != 404 {
			t.Fatalf("garbage job id on %s: %d", p, r.code)
		}
	}
	if st := h.jobState(t, j.ID); st != behavior.WorkerJobRunning && st != behavior.WorkerJobLeased {
		t.Fatalf("foreign requests changed the job: %s", st)
	}
}

// T-46 (REQ-7, 9): progress dedupe and the cancel flag over HTTP.
func TestWorkerHTTP7a_JourneyProgressAndCancel(t *testing.T) {
	h := newW7Hub(t)
	dev, tok := h.enroll(t, workspaceA, "laptop")
	j := h.job(t, workspaceA, nil, "")
	lt := h.mustLease(t, dev).LeaseToken
	base := "/worker/v1/jobs/" + j.ID
	pg := map[string]any{"lease_token": lt, "seq": 1, "phase": "editing", "counts": map[string]any{"files_changed": 2}}
	if r := h.do(t, base+"/progress", tok, pg); r.code != 200 || r.body["duplicate"] != false {
		t.Fatalf("progress: %d %s", r.code, r.raw)
	}
	if r := h.do(t, base+"/progress", tok, pg); r.code != 200 || r.body["duplicate"] != true {
		t.Fatalf("progress replay: %d %s", r.code, r.raw)
	}
	for _, bad := range []map[string]any{
		{"lease_token": lt, "seq": 0, "phase": "editing"},
		{"lease_token": lt, "seq": 2, "phase": "stealing"},
		{"lease_token": lt, "seq": 2, "phase": "editing", "counts": map[string]any{"turns": -1}},
	} {
		if r := h.do(t, base+"/progress", tok, bad); r.code != 400 {
			t.Fatalf("bad progress %v: %d", bad, r.code)
		}
	}
	if r := h.do(t, base+"/progress", tok, map[string]any{"lease_token": "nhl_stale", "seq": 5, "phase": "editing"}); r.code != 409 {
		t.Fatalf("stale progress: %d", r.code)
	}
	if _, err := h.s.CancelJob(h.ctx, workspaceA, j.ID); err != nil {
		t.Fatal(err)
	}
	if r := h.do(t, base+"/heartbeat", tok, map[string]any{"lease_token": lt}); r.code != 200 || r.body["cancel"] != true || r.body["state"] != "CANCELLING" {
		t.Fatalf("cancel flag: %d %s", r.code, r.raw)
	}
	if r := h.do(t, base+"/progress", tok, map[string]any{"lease_token": lt, "seq": 3, "phase": "testing"}); r.code != 200 || r.body["cancel"] != true {
		t.Fatalf("progress cancel flag: %d %s", r.code, r.raw)
	}
}

// T-46 (REQ-7): result validation, idempotency and conflict over HTTP.
func TestWorkerHTTP7a_JourneyResult(t *testing.T) {
	h := newW7Hub(t)
	dev, tok := h.enroll(t, workspaceA, "laptop")
	j := h.job(t, workspaceA, nil, "")
	lt := h.mustLease(t, dev).LeaseToken
	base := "/worker/v1/jobs/" + j.ID
	res := func(key string, extra map[string]any) hubResp {
		b := map[string]any{"lease_token": lt, "idempotency_key": key, "status": "succeeded", "summary": "stopped", "pr_url": "https://github.com/o/r/pull/3"}
		for k, v := range extra {
			b[k] = v
		}
		return h.do(t, base+"/result", tok, b)
	}
	for _, extra := range []map[string]any{
		{"pr_url": "http://github.com/o/r/pull/1"}, {"pr_url": "https://evil.example/pull/1"}, {"status": "great"},
		{"summary": strings.Repeat("x", 2001)}, {"reason_code": "free text"},
	} {
		if r := res("k1", extra); r.code != 400 {
			t.Fatalf("bad result %v: %d", extra, r.code)
		}
	}
	if r := res("bad key!", nil); r.code != 400 {
		t.Fatalf("bad idempotency key: %d", r.code)
	}
	if st := h.jobState(t, j.ID); st != behavior.WorkerJobLeased {
		t.Fatalf("rejected results changed the job: %s", st)
	}
	if r := res("k1", nil); r.code != 200 || r.body["state"] != "SUCCEEDED" || r.body["duplicate"] != false {
		t.Fatalf("result: %d %s", r.code, r.raw)
	}
	if r := res("k1", map[string]any{"status": "failed"}); r.code != 200 || r.body["duplicate"] != true || r.body["state"] != "SUCCEEDED" {
		t.Fatalf("replay: %d %s", r.code, r.raw)
	}
	conflict := res("k2", nil)
	if conflict.code != 409 {
		t.Fatalf("second result: %d", conflict.code)
	}
	for _, raw := range []string{conflict.raw, h.do(t, base+"/heartbeat", tok, map[string]any{"lease_token": "nhl_stale"}).raw} {
		if strings.Contains(raw, lt) || strings.Contains(raw, j.ID) || strings.Contains(raw, "stopped") {
			t.Fatalf("error echoes request data: %s", raw)
		}
	}
}

// T-48 (REQ-18): invalid telemetry is a 400 on poll and heartbeat and stores nothing; valid telemetry is kept.
func TestWorkerHTTP7a_TelemetryIntake(t *testing.T) {
	h := newW7Hub(t)
	dev, tok := h.enroll(t, workspaceA, "laptop")
	h.job(t, workspaceA, nil, "")
	lease := h.mustLease(t, dev)
	bad := []map[string]any{
		{"state": "idle", "cpu_cores": 0},
		{"state": "sleeping"},
		{"state": "idle", "claude_session_left_pct": 101},
		{"state": "idle", "ram_used_bytes": 10, "ram_total_bytes": 5},
		{"state": "idle", "executor": "/bin/sh"},
	}
	hb := "/worker/v1/jobs/" + lease.Job.ID + "/heartbeat"
	for _, b := range bad {
		if r := h.poll(t, tok, 0, b); r.code != 400 || r.body["error"] != "invalid telemetry" {
			t.Fatalf("poll telemetry %v: %d %s", b, r.code, r.raw)
		}
		if r := h.do(t, hb, tok, map[string]any{"lease_token": lease.LeaseToken, "telemetry": b}); r.code != 400 {
			t.Fatalf("heartbeat telemetry %v: %d", b, r.code)
		}
	}
	if recs, _ := h.s.ListTelemetry(h.ctx, workspaceA); len(recs) != 0 {
		t.Fatalf("invalid telemetry stored: %+v", recs)
	}
	good := map[string]any{"state": "busy", "cpu_load": 2.5, "cpu_cores": 8, "claude_session_left_pct": 40,
		"counters": map[string]any{"jobs": 3, "failures": 1, "executor_tokens": 1000, "busy_seconds": 60}}
	if r := h.do(t, hb, tok, map[string]any{"lease_token": lease.LeaseToken, "telemetry": good}); r.code != 200 {
		t.Fatalf("good heartbeat: %d %s", r.code, r.raw)
	}
	recs, _ := h.s.ListTelemetry(h.ctx, workspaceA)
	if len(recs) != 1 || recs[0].Snapshot.State != "busy" || recs[0].Snapshot.Counters.Jobs != 3 || *recs[0].Snapshot.ClaudeSessionLeftPct != 40 {
		t.Fatalf("snapshot: %+v", recs)
	}
	// idle poll telemetry replaces it
	if r := h.poll(t, tok, 0, map[string]any{"state": "idle"}); r.code != 204 {
		t.Fatalf("idle poll: %d", r.code)
	}
	if recs, _ = h.s.ListTelemetry(h.ctx, workspaceA); recs[0].Snapshot.State != "idle" {
		t.Fatalf("latest not replaced: %+v", recs[0].Snapshot)
	}
}

// T-49 (REQ-3): two devices polling at once get one job between them.
func TestWorkerHTTP7a_TwoDevicesOneJob(t *testing.T) {
	h := newW7Hub(t)
	_, t1 := h.enroll(t, workspaceA, "one")
	_, t2 := h.enroll(t, workspaceA, "two")
	h.job(t, workspaceA, nil, "")
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, tok := range []string{t1, t2} {
		wg.Add(1)
		go func(tok string) { defer wg.Done(); codes <- h.poll(t, tok, 1, nil).code }(tok)
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[200] != 1 || got[204] != 1 {
		t.Fatalf("statuses %v", got)
	}
}
