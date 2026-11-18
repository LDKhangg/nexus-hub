package behavior

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Fix B regressions (review B, commit b317aa1): R-B-2 (mapping),
// R-B-4 (mention resolution), R-B-5 (preview resend), R-B-6 (Cancel keeps the
// report button), R-B-7 (unreadable topic), R-B-8 (outcome line), D-B-1 (a
// press on the wrong message). Fakes only.

// --- R-B-2 ---

// R-B-2: the orchestrator's job view carries the worker's summary, so the step can store it.
func TestP10FixB_OrchestratorJobCarriesSummary(t *testing.T) {
	j := orchestratorJob(WorkerJob{ID: "job-1", State: WorkerJobSucceeded,
		Result: &WorkerResult{Summary: "Đã xong", Branch: "fix/x", PRURL: "https://github.com/o/r/pull/9", TestOutcome: "passed", ReasonCode: "ok"}})
	if j.Summary != "Đã xong" || j.Branch != "fix/x" || j.PRURL != "https://github.com/o/r/pull/9" || j.TestOutcome != "passed" {
		t.Fatalf("job %+v", j)
	}
	if got := orchestratorJob(WorkerJob{ID: "job-2", State: WorkerJobWaiting}); got.Summary != "" {
		t.Fatalf("a job without a result has summary %q", got.Summary)
	}
}

// R-B-2: the stored summary reaches the /plan report draft (multibyte, long).
func TestP10FixB_PlanReportShowsStoredSummary(t *testing.T) {
	r := newP10REnv(t)
	summary := strings.TrimSpace(strings.Repeat("Đã xong phần việc ", 25))
	r.addPlan(70, p10Task, p10PlanTask(1, "api", "Sửa API", summary, "fix/api", "https://github.com/o/r/pull/3"))
	rep, err := r.rep.ExecutionReport(context.Background(), p10WS, 70)
	if err != nil || !strings.Contains(rep.Text, summary) || !strings.Contains(rep.Text, "https://github.com/o/r/pull/3") {
		t.Fatalf("report %q err %v", rep.Text, err)
	}
}

// --- R-B-4 ---

// p10BPeople records the search term and limit of each directory call and returns a fixed list.
type p10BPeople struct {
	*p4Directory
	mu       sync.Mutex
	result   []people.Contact
	searches []string
	limits   []int
}

func (d *p10BPeople) List(_ context.Context, _, _, search string, limit int) ([]people.Contact, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.searches = append(d.searches, search)
	d.limits = append(d.limits, limit)
	return append([]people.Contact(nil), d.result...), nil
}

func p10BClickUp(id, name string) people.Contact {
	return people.Contact{ID: "c" + id, Name: name, Accounts: []people.Account{p10Account(id)}}
}

func (x *p10TEnv) withDirectory(cs ...people.Contact) *p10BPeople {
	d := &p10BPeople{p4Directory: &p4Directory{}, result: cs}
	x.w.d.People = d
	return d
}

func (x *p10TEnv) mention(name string) (map[string]any, string) {
	m, text, _ := x.call(x.turn(p10Thread, 0), map[string]any{"text": "nhờ xem giúp", "mentions": []any{name}})
	return m, text
}

func (x *p10TEnv) mentionedIDs() []string {
	var all []string
	for _, d := range p10Rows(x.p10Env) {
		all = append(all, strings.Join(d.MentionUserIDs, ","))
	}
	return all
}

// R-B-4: "Nam" resolves "Nguyễn Văn Nam" (a whole-word match of a longer name).
func TestP10FixB_MentionWholeWordResolves(t *testing.T) {
	x := newP10TEnv(t)
	x.withDirectory(p10BClickUp("4001", "Nguyễn Văn Nam"))
	m, text := x.mention("Nam")
	if m["status"] != "preview_waiting" {
		t.Fatalf("result %q", text)
	}
	if got := x.mentionedIDs(); len(got) != 1 || got[0] != "4001" {
		t.Fatalf("mentions %v", got)
	}
	x.noPost(t)
}

// R-B-4: "An" is not "Hoàng Anh": a substring-only match is ambiguous and makes no draft.
func TestP10FixB_MentionSubstringIsAmbiguous(t *testing.T) {
	x := newP10TEnv(t)
	x.withDirectory(p10BClickUp("4002", "Hoàng Anh"))
	m, text := x.mention("An")
	names := p10Names(t, m)
	if m["status"] != "mentions_unresolved" || len(names) != 1 || names[0]["result"] != "ambiguous" {
		t.Fatalf("result %q", text)
	}
	cands, _ := names[0]["candidates"].([]any)
	if len(cands) != 1 || cands[0] != "Hoàng Anh" || strings.Contains(text, "4002") {
		t.Fatalf("candidates %v text %q", cands, text)
	}
	x.noDraft(t)
}

// R-B-4: two whole-word matches are ambiguous; so are two people sharing the first name.
func TestP10FixB_MentionTwoWholeWordMatchesAmbiguous(t *testing.T) {
	x := newP10TEnv(t)
	x.withDirectory(p10BClickUp("4001", "Nguyễn Văn Nam"), p10BClickUp("4002", "Nam Trần"))
	m, text := x.mention("Nam")
	names := p10Names(t, m)
	if m["status"] != "mentions_unresolved" || len(names) != 1 || names[0]["result"] != "ambiguous" {
		t.Fatalf("result %q", text)
	}
	if cands, _ := names[0]["candidates"].([]any); len(cands) != 2 {
		t.Fatalf("candidates %v", cands)
	}
	x.noDraft(t)
}

// R-B-4: an exact ClickUp username or platform name resolves even when the display name differs.
func TestP10FixB_MentionExactUsername(t *testing.T) {
	x := newP10TEnv(t)
	c := people.Contact{ID: "c9", Name: "Bảo Nguyễn", Accounts: []people.Account{{ID: "a9", Platform: "clickup", ExternalID: "4009", PlatformName: "Bao N", Username: "nvbao"}}}
	x.withDirectory(c, p10BClickUp("4010", "Bảo Trần"))
	if m, text := x.mention("NVBAO"); m["status"] != "preview_waiting" {
		t.Fatalf("username: %q", text)
	}
	if got := x.mentionedIDs(); len(got) != 1 || got[0] != "4009" {
		t.Fatalf("mentions %v", got)
	}
	if m, text := x.mention("Bao N"); m["status"] != "preview_waiting" {
		t.Fatalf("platform name: %q", text)
	}
}

// R-B-4: a full page of 50 never resolves a name, even with one exact match; 49 does.
func TestP10FixB_MentionFullPageNeverResolves(t *testing.T) {
	page := func(n int) []people.Contact {
		cs := []people.Contact{p10BClickUp("5000", "Nam")}
		for i := 1; i < n; i++ {
			cs = append(cs, p10BClickUp(strconv.Itoa(5000+i), "Nam Người "+strconv.Itoa(i)))
		}
		return cs
	}
	x := newP10TEnv(t)
	d := x.withDirectory(page(assistantMentionSearchLimit)...)
	m, text := x.mention("Nam")
	names := p10Names(t, m)
	if m["status"] != "mentions_unresolved" || len(names) != 1 || names[0]["result"] != "ambiguous" {
		t.Fatalf("full page: %q", text)
	}
	if cands, _ := names[0]["candidates"].([]any); len(cands) == 0 || len(cands) > assistantMentionCandidates {
		t.Fatalf("candidates %v", cands)
	}
	x.noDraft(t)
	if len(d.limits) != 1 || d.limits[0] != 50 {
		t.Fatalf("directory limit %v", d.limits)
	}
	// One short of a full page: the exact match is certain.
	d.mu.Lock()
	d.result = page(assistantMentionSearchLimit - 1)
	d.mu.Unlock()
	if m, text := x.mention("Nam"); m["status"] != "preview_waiting" {
		t.Fatalf("49 contacts: %q", text)
	}
	if got := x.mentionedIDs(); len(got) != 1 || got[0] != "5000" {
		t.Fatalf("mentions %v", got)
	}
}

// R-B-4 (abuse): %, _ and \ in a name are escaped before the directory's LIKE search.
func TestP10FixB_MentionEscapesLikeWildcards(t *testing.T) {
	x := newP10TEnv(t)
	d := x.withDirectory()
	for name, want := range map[string]string{
		"100%":    `100\%`,
		"a_b":     `a\_b`,
		`c\d`:     `c\\d`,
		"%_\\":    `\%\_\\`,
		"Nguyễn":  "Nguyễn",
		"plain x": "plain x",
	} {
		x.mention(name)
		d.mu.Lock()
		got := d.searches[len(d.searches)-1]
		d.mu.Unlock()
		if got != want {
			t.Errorf("search for %q is %q, want %q", name, got, want)
		}
	}
	if escapeLikeTerm("") != "" || escapeLikeTerm(`\%_`) != `\\\%\_` {
		t.Fatal("escapeLikeTerm")
	}
	x.noDraft(t)
}

// R-B-4 (abuse): "%" alone must not match everyone: with a directory that returns nobody for the literal, no draft.
func TestP10FixB_MentionPercentIsNotAWildcard(t *testing.T) {
	x := newP10TEnv(t)
	x.withDirectory()
	m, text := x.mention("%")
	names := p10Names(t, m)
	if m["status"] != "mentions_unresolved" || len(names) != 1 || names[0]["result"] != "not_found" {
		t.Fatalf("result %q", text)
	}
	x.noDraft(t)
}

// --- R-B-7 ---

// R-B-7: a topic storage error with a task id from the model gives task_unreadable and no draft, never the model's task.
func TestP10FixB_TopicErrorNeverFallsBackToModelTask(t *testing.T) {
	x := newP10TEnv(t)
	x.topics.mu.Lock()
	x.topics.err = errors.New("db down")
	x.topics.mu.Unlock()
	for _, in := range []map[string]any{
		{"text": "bình luận", "task_id": "86zzz9"},
		{"text": "bình luận", "task_id": p10Task},
		{"text": "bình luận"},
	} {
		m, text, isErr := x.call(x.turn(p10Thread, 0), in)
		if isErr || m["status"] != "task_unreadable" {
			t.Fatalf("input %v: %q", in, text)
		}
	}
	x.noDraft(t)
	// Storage is back: the same call drafts on the topic's task.
	x.topics.mu.Lock()
	x.topics.err = nil
	x.topics.mu.Unlock()
	if m, text, isErr := x.call(x.turn(p10Thread, 0), map[string]any{"text": "bình luận"}); isErr || m["status"] != "preview_waiting" {
		t.Fatalf("after recovery: %q", text)
	}
}

// R-B-7: a thread with no topic row is unchanged: the model's task is used, none gives task_required.
func TestP10FixB_ThreadWithoutTopicIsUnchanged(t *testing.T) {
	x := newP10TEnv(t)
	if m, text, _ := x.call(x.turn(99, 0), map[string]any{"text": "bình luận"}); m["status"] != "task_required" {
		t.Fatalf("no task: %q", text)
	}
	x.noDraft(t)
	if m, text, _ := x.call(x.turn(99, 0), map[string]any{"text": "bình luận", "task_id": "86zzz9"}); m["status"] != "preview_waiting" {
		t.Fatalf("explicit task: %q", text)
	}
	if rows := p10Rows(x.p10Env); len(rows) != 1 || rows[0].TaskID != "86zzz9" {
		t.Fatalf("drafts %+v", rows)
	}
}

// --- D-B-1 ---

func (e *p10Env) approvalDecision(t *testing.T, a Approval) Decision {
	t.Helper()
	cur, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, a.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	return cur.Decision
}

// D-B-1: a press on another message is stale but keeps the approval of the open draft's real preview alive:
// the real Đăng then posts exactly once.
func TestP10FixB_WrongMessagePressKeepsRealPreviewWorking(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "nội dung thật")
	previewID, sent := e.msg.lastSent(t)
	ok, no := e.approvalOf(t, sent, "Đăng"), e.approvalOf(t, sent, "Huỷ")
	for _, kind := range []CallbackKind{CallbackApprove, CallbackReject, CallbackFreeText} {
		a := ok
		if kind == CallbackReject {
			a = no
		}
		toast, code, _ := e.press(kind, a, previewID+99)
		if toast != msgDraftStale || code != "stale" {
			t.Fatalf("kind %v: toast %q code %q", kind, toast, code)
		}
		if got := e.approvalDecision(t, a); got != "" {
			t.Fatalf("kind %v: the approval was closed (%q) by a press on another message", kind, got)
		}
	}
	if toast, code, _ := e.press(CallbackApprove, ok, 0); code != "stale" || toast != msgDraftStale || e.approvalDecision(t, ok) != "" {
		t.Fatalf("press without a message id: %q %q", toast, code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 0 || e.draft(t, id).State != DraftPending {
		t.Fatalf("a wrong press changed the draft: %d posts, %s", n, e.draft(t, id).State)
	}
	// The real press posts, once, even when repeated.
	e.press(CallbackApprove, ok, previewID)
	e.press(CallbackApprove, ok, previewID)
	if calls := e.tool.callsOf(ToolPostComment); len(calls) != 1 || e.draft(t, id).State != DraftPosted {
		t.Fatalf("posts %d state %s", len(calls), e.draft(t, id).State)
	}
}

// D-B-1: for a terminal draft or a draft with no preview bound, the wrong-message press closes the approval.
func TestP10FixB_WrongMessagePressClosesDeadApproval(t *testing.T) {
	for name, mutate := range map[string]func(e *p10Env, id int64){
		"posted": func(e *p10Env, id int64) {
			e.store.mu.Lock()
			e.store.rows[id].State = DraftPosted
			e.store.mu.Unlock()
		},
		"cancelled": func(e *p10Env, id int64) {
			e.store.mu.Lock()
			e.store.rows[id].State = DraftCancelled
			e.store.mu.Unlock()
		},
		"unbound": func(e *p10Env, id int64) {
			e.store.mu.Lock()
			e.store.rows[id].PreviewMessageID = 0
			e.store.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newP10Env(t)
			id := e.create(t, "nội dung")
			previewID, sent := e.msg.lastSent(t)
			a := e.approvalOf(t, sent, "Đăng")
			mutate(e, id)
			if toast, code, _ := e.press(CallbackApprove, a, previewID+7); toast != msgDraftStale || code != "stale" {
				t.Fatalf("toast %q code %q", toast, code)
			}
			if e.approvalDecision(t, a) == "" {
				t.Fatal("the approval of a dead draft stayed open")
			}
			if n := len(e.tool.callsOf(ToolPostComment)); n != 0 {
				t.Fatalf("%d posts", n)
			}
		})
	}
}

// --- R-B-5 ---

// R-B-5: the preview send fails, the next 📤 resends the same draft's preview; Đăng then works on it.
func TestP10FixB_ReportPreviewResendThenPost(t *testing.T) {
	r := newP10REnv(t)
	r.addLocal(41, p10Task, WorkerJobSucceeded, p10Result("Đã xong phần API", "fix/api", "https://github.com/o/r/pull/9"))
	r.msg.failAt = 1
	if out := r.press(41); out.code != "preview_not_sent" || r.x.lastToast(t) != msgReportPreviewFailed {
		t.Fatalf("first press %+v toast %q", out, r.x.lastToast(t))
	}
	rows := r.drafts()
	if len(rows) != 1 || rows[0].PreviewMessageID != 0 || rows[0].State != DraftPending {
		t.Fatalf("drafts %+v", rows)
	}
	first := rows[0]
	// The resend itself fails too: still preview_not_sent, still one draft.
	r.msg.mu.Lock()
	r.msg.failAt = r.msg.sends + 1
	r.msg.mu.Unlock()
	if out := r.press(41); out.code != "preview_not_sent" || r.x.lastToast(t) != msgReportPreviewFailed || len(r.drafts()) != 1 {
		t.Fatalf("resend failure %+v toast %q drafts %d", out, r.x.lastToast(t), len(r.drafts()))
	}
	// The next press sends the preview.
	if out := r.press(41); out.code != "" || r.x.lastToast(t) != msgReportDraftMade {
		t.Fatalf("resend %+v toast %q", out, r.x.lastToast(t))
	}
	rows = r.drafts()
	if len(rows) != 1 || rows[0].ID != first.ID || rows[0].Body != first.Body || !rows[0].ExpiresAt.Equal(first.ExpiresAt) || rows[0].PreviewMessageID == 0 {
		t.Fatalf("after resend %+v (first %+v)", rows, first)
	}
	msgID, sent := r.msg.lastSent(t)
	if rows[0].PreviewMessageID != msgID || !strings.Contains(sent.msg.Text, "https://github.com/o/r/pull/9") || sent.to.ThreadID != p10Thread {
		t.Fatalf("preview %+v draft preview %d", sent, rows[0].PreviewMessageID)
	}
	if len(r.tool.calls) != 0 {
		t.Fatalf("%d ClickUp calls before Đăng", len(r.tool.calls))
	}
	// A press after the preview exists only reports the open draft; it sends nothing more.
	r.msg.mu.Lock()
	before := len(r.msg.sent)
	r.msg.mu.Unlock()
	if out := r.press(41); out.code != "" || r.x.lastToast(t) != msgReportDraftOpen {
		t.Fatalf("press after resend %+v toast %q", out, r.x.lastToast(t))
	}
	r.msg.mu.Lock()
	after := len(r.msg.sent)
	r.msg.mu.Unlock()
	if after != before || len(r.drafts()) != 1 {
		t.Fatalf("previews %d -> %d drafts %d", before, after, len(r.drafts()))
	}
	// Đăng on the resent preview posts once.
	r.p10Env.press(CallbackApprove, r.approvalOf(t, sent, "Đăng"), msgID)
	if calls := r.tool.callsOf(ToolPostComment); len(calls) != 1 || calls[0].input["task_id"] != p10Task {
		t.Fatalf("post calls %+v", calls)
	}
	if got := r.drafts(); len(got) != 1 || got[0].State != DraftPosted {
		t.Fatalf("drafts %+v", got)
	}
	r.noWrites(t)
}

// --- R-B-8 ---

// p10BExec adds the execution state to the source-kind fake.
type p10BExec struct {
	*p10XExec
	states map[int64]workflow.State
}

func (e *p10BExec) GetExecutionDetail(ctx context.Context, ws string, id int64) (workflow.ExecutionDetail, error) {
	d, err := e.p10XExec.GetExecutionDetail(ctx, ws, id)
	d.State = e.states[id]
	return d, err
}

func (r *p10REnv) withStates(states map[int64]workflow.State) {
	r.rep.Executions = &p10BExec{p10XExec: r.exec, states: states}
}

func reportLines(text string) []string { return strings.Split(text, "\n") }

// R-B-8: a failed or cancelled /task carries the outcome line right after the head; a success has none.
func TestP10FixB_LocalReportOutcomeLine(t *testing.T) {
	res := func() *WorkerResult { return &WorkerResult{Status: "failed", Summary: "2 test lỗi", Branch: "fix/x"} }
	for _, tc := range []struct {
		name     string
		jobState string
		wf       workflow.State
		want     string
	}{
		{"job failed", WorkerJobFailed, workflow.StateRunning, msgReportOutcomeFailed},
		{"job cancelled", WorkerJobCancelled, workflow.StateRunning, msgReportOutcomeCancelled},
		{"execution failed", WorkerJobSucceeded, workflow.StateFailed, msgReportOutcomeFailed},
		{"execution cancelled", WorkerJobSucceeded, workflow.StateCancelled, msgReportOutcomeCancelled},
		{"success", WorkerJobSucceeded, workflow.StateCompleted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newP10REnv(t)
			r.addLocal(41, p10Task, tc.jobState, res())
			r.withStates(map[int64]workflow.State{41: tc.wf})
			rep, err := r.rep.ExecutionReport(context.Background(), p10WS, 41)
			if err != nil {
				t.Fatal(err)
			}
			lines := reportLines(rep.Text)
			if tc.want == "" {
				if strings.Contains(rep.Text, "Trạng thái:") {
					t.Fatalf("a successful report has an outcome line: %q", rep.Text)
				}
				return
			}
			if len(lines) < 3 || lines[1] != tc.want || !strings.Contains(rep.Text, "2 test lỗi") {
				t.Fatalf("report %q", rep.Text)
			}
		})
	}
}

// R-B-8: a failed or cancelled /plan carries the outcome line; the draft the button makes and its preview show it.
func TestP10FixB_PlanReportOutcomeLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		wf   workflow.State
		want string
	}{
		{"failed", workflow.StateFailed, msgReportOutcomeFailed},
		{"cancelled", workflow.StateCancelled, msgReportOutcomeCancelled},
		{"completed", workflow.StateCompleted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newP10REnv(t)
			r.addPlan(70, p10Task, p10PlanTask(1, "api", "Sửa API", "Đã sửa một phần", "fix/api", "https://github.com/o/r/pull/3"))
			r.withStates(map[int64]workflow.State{70: tc.wf})
			if out := r.press(70); out.code != "" {
				t.Fatalf("press %+v", out)
			}
			rows := r.drafts()
			if len(rows) != 1 {
				t.Fatalf("drafts %+v", rows)
			}
			_, sent := r.msg.lastSent(t)
			has := strings.Contains(rows[0].Body, "Trạng thái:")
			if tc.want == "" {
				if has || strings.Contains(sent.msg.Text, "Trạng thái:") {
					t.Fatalf("a completed plan has an outcome line: %q", rows[0].Body)
				}
				return
			}
			if !strings.Contains(rows[0].Body, tc.want) || !strings.Contains(sent.msg.Text, tc.want) {
				t.Fatalf("body %q preview %q", rows[0].Body, sent.msg.Text)
			}
			if lines := reportLines(strings.TrimPrefix(rows[0].Body, draftMarker)); len(lines) < 2 || strings.TrimSpace(lines[1]) != tc.want {
				t.Fatalf("outcome line not right after the head: %q", rows[0].Body)
			}
		})
	}
}

// R-B-8: reportOutcomeLine itself: the job state or the execution state, either one.
func TestP10FixB_ReportOutcomeLineTable(t *testing.T) {
	for _, tc := range []struct {
		state workflow.State
		job   string
		want  string
	}{
		{workflow.StateCancelled, "", msgReportOutcomeCancelled},
		{workflow.StateFailed, "", msgReportOutcomeFailed},
		{workflow.StateRunning, WorkerJobCancelled, msgReportOutcomeCancelled},
		{workflow.StateRunning, WorkerJobFailed, msgReportOutcomeFailed},
		{workflow.StateFailed, WorkerJobCancelled, msgReportOutcomeCancelled},
		{workflow.StateCompleted, WorkerJobSucceeded, ""},
		{workflow.StateCompleted, "", ""},
	} {
		if got := reportOutcomeLine(tc.state, tc.job); got != tc.want {
			t.Errorf("reportOutcomeLine(%q, %q) = %q, want %q", tc.state, tc.job, got, tc.want)
		}
	}
}
