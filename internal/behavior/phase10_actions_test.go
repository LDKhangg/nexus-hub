package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Batch A, K1 (REQ-10): the orchestrator's behavior-actions
// executor over a fake draft port. The real draft flow is covered in
// phase10_drafts_test.go.

type p10Port struct {
	mu        sync.Mutex
	drafts    map[int64]ClickUpDraft
	next      int64
	created   []DraftRequest
	cancelled []int64
	createErr error
	createID  int64 // id returned with createErr
	noFinder  bool
}

func newP10Port() *p10Port { return &p10Port{drafts: map[int64]ClickUpDraft{}} }

func (p *p10Port) Create(_ context.Context, r DraftRequest) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return p.createID, p.createErr
	}
	p.next++
	p.created = append(p.created, r)
	p.drafts[p.next] = ClickUpDraft{ID: p.next, WorkspaceID: r.Chat.WorkspaceID, TaskID: r.TaskID, Source: r.Source,
		ExecutionID: r.ExecutionID, PlanTaskKey: r.PlanTaskKey, State: DraftPending}
	return p.next, nil
}
func (p *p10Port) Get(_ context.Context, ws string, id int64) (ClickUpDraft, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.drafts[id]
	if !ok || d.WorkspaceID != ws {
		return ClickUpDraft{}, ErrNotFound
	}
	return d, nil
}
func (p *p10Port) Cancel(_ context.Context, _ string, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelled = append(p.cancelled, id)
	return nil
}
func (p *p10Port) ForPlanTask(_ context.Context, ws string, exec int64, key string) (ClickUpDraft, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noFinder {
		return ClickUpDraft{}, false, ErrNotAvailable
	}
	var best ClickUpDraft
	for _, d := range p.drafts {
		if d.WorkspaceID == ws && d.ExecutionID == exec && d.PlanTaskKey == key && d.ID > best.ID {
			best = d
		}
	}
	return best, best.ID > 0, nil
}
func (p *p10Port) set(id int64, f func(*ClickUpDraft)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.drafts[id]
	f(&d)
	p.drafts[id] = d
}

type p10Sayer struct {
	mu   sync.Mutex
	said []string
	to   []orchestrator.Route
	err  error
}

func (s *p10Sayer) Say(_ context.Context, r orchestrator.Route, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.said = append(s.said, text)
	s.to = append(s.to, r)
	return nil
}

func p10Run(caps ...string) orchestrator.TaskRun {
	return orchestrator.TaskRun{
		WorkspaceID: p10WS, ExecutionID: 12, StepKey: "run_01",
		Task:  orchestrator.PlanTask{Key: "w", Title: "Báo kết quả cho anh Nam", Capabilities: caps},
		Route: orchestrator.Route{WorkspaceID: p10WS, ExecutionID: 12, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: 77, ClickUpTaskID: p10Task},
	}
}

// TC-K1a (REQ-10): clickup.write makes a draft on the execution's own task in its topic and waits; it posts nothing.
func TestP10Actions_ClickUpWriteMakesDraftAndWaits(t *testing.T) {
	port := newP10Port()
	a := &OrchestratorActions{Drafts: port}
	run := p10Run(orchestrator.CapClickUpWrite)
	run.Inputs = []string{"Sửa lỗi: PR: https://github.com/o/r/pull/3 · Nhánh: feat/x"}
	out, err := a.RunTask(context.Background(), run)
	if err != nil || out.Wait != "draft:1" || out.Succeeded || out.Blocked {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if len(port.created) != 1 {
		t.Fatalf("%d drafts", len(port.created))
	}
	r := port.created[0]
	if r.TaskID != p10Task || r.Source != DraftSourceOrchestrator || r.ExecutionID != 12 || r.PlanTaskKey != "run_01:w" {
		t.Fatalf("request %+v", r)
	}
	if r.Chat.ChatID != p10Chat || r.Chat.ThreadID != 77 || r.Chat.IntegrationID != p10Bot {
		t.Fatalf("preview goes to %+v", r.Chat)
	}
	if !strings.HasPrefix(r.Text, "Báo kết quả cho anh Nam") || !strings.Contains(r.Text, "pull/3") {
		t.Fatalf("text %q", r.Text)
	}
}

// TC-K1a (REQ-10, abuse): no ClickUp task, an unsafe task id, an empty or oversized text.
func TestP10Actions_ClickUpWriteRefusals(t *testing.T) {
	a := func() (*OrchestratorActions, *p10Port) {
		p := newP10Port()
		return &OrchestratorActions{Drafts: p}, p
	}
	t.Run("no ClickUp task", func(t *testing.T) {
		ac, port := a()
		run := p10Run(orchestrator.CapClickUpWrite)
		run.Route.ClickUpTaskID = ""
		out, _ := ac.RunTask(context.Background(), run)
		if out.ReasonCode != ReasonActionNoClickUpTask || len(port.created) != 0 {
			t.Fatalf("%+v drafts %d", out, len(port.created))
		}
	})
	t.Run("unsafe task id", func(t *testing.T) {
		ac, port := a()
		run := p10Run(orchestrator.CapClickUpWrite)
		run.Route.ClickUpTaskID = "../other"
		if out, _ := ac.RunTask(context.Background(), run); out.ReasonCode != ReasonActionNoClickUpTask || len(port.created) != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("empty text", func(t *testing.T) {
		ac, port := a()
		run := p10Run(orchestrator.CapClickUpWrite)
		run.Task.Title = "  \n"
		if out, _ := ac.RunTask(context.Background(), run); out.ReasonCode != ReasonActionInvalidText || len(port.created) != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("long text is cut to 4000 runes", func(t *testing.T) {
		ac, port := a()
		run := p10Run(orchestrator.CapClickUpWrite)
		run.Task.Title = strings.Repeat("ệ", 6000)
		if out, _ := ac.RunTask(context.Background(), run); out.Wait == "" {
			t.Fatalf("%+v", out)
		}
		if n := utf8.RuneCountInString(port.created[0].Text); n > 4000 || !utf8.ValidString(port.created[0].Text) {
			t.Fatalf("%d runes", n)
		}
	})
	t.Run("unsupported capability", func(t *testing.T) {
		ac, _ := a()
		if out, _ := ac.RunTask(context.Background(), p10Run(orchestrator.CapRepoWrite)); out.ReasonCode != ReasonActionUnsupported {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("no draft flow wired", func(t *testing.T) {
		ac := &OrchestratorActions{}
		if out, _ := ac.RunTask(context.Background(), p10Run(orchestrator.CapClickUpWrite)); out.ReasonCode != orchestrator.ReasonExecutorUnavailable {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("invalid draft", func(t *testing.T) {
		ac, port := a()
		port.createErr = ErrInvalid
		if out, _ := ac.RunTask(context.Background(), p10Run(orchestrator.CapClickUpWrite)); out.ReasonCode != ReasonActionInvalidText {
			t.Fatalf("%+v", out)
		}
	})
}

// TC-K1 (REQ-10): a draft whose preview was never sent is cancelled at once; a store error is returned for a retry.
func TestP10Actions_PreviewNotSentCancelsDraft(t *testing.T) {
	port := newP10Port()
	port.createErr, port.createID = errors.New("send failed"), 41
	a := &OrchestratorActions{Drafts: port}
	out, err := a.RunTask(context.Background(), p10Run(orchestrator.CapClickUpWrite))
	if err != nil || out.ReasonCode != ReasonActionPreviewNotSent || out.Wait != "" {
		t.Fatalf("%+v %v", out, err)
	}
	if len(port.cancelled) != 1 || port.cancelled[0] != 41 {
		t.Fatalf("cancelled %v", port.cancelled)
	}
	port2 := newP10Port()
	port2.createErr = errors.New("db down")
	if _, err := (&OrchestratorActions{Drafts: port2}).RunTask(context.Background(), p10Run(orchestrator.CapClickUpWrite)); err == nil {
		t.Fatal("a storage error must be returned so the step retries")
	}
}

// TC-K1b, K1c (REQ-10): posted is success with the comment id as evidence; cancelled and expired block;
// pending, posting and failed keep waiting.
func TestP10Actions_ResumeMapsDraftState(t *testing.T) {
	cases := []struct {
		state   string
		comment string
		check   func(orchestrator.TaskOutcome) bool
	}{
		{DraftPosted, "9001", func(o orchestrator.TaskOutcome) bool {
			return o.Succeeded && len(o.Evidence) == 1 && o.Evidence[0].ID == "clickup:comment:9001"
		}},
		{DraftPosted, "", func(o orchestrator.TaskOutcome) bool {
			return o.Succeeded && len(o.Evidence) == 1 && o.Evidence[0].ID == "clickup:draft:1"
		}},
		{DraftCancelled, "", func(o orchestrator.TaskOutcome) bool {
			return o.Blocked && !o.Succeeded && o.ReasonCode == ReasonActionDraftCancelled
		}},
		{DraftExpired, "", func(o orchestrator.TaskOutcome) bool {
			return o.Blocked && !o.Succeeded && o.ReasonCode == ReasonActionDraftExpired
		}},
		{DraftPending, "", func(o orchestrator.TaskOutcome) bool { return o.Wait == "draft:1" && !o.Succeeded && !o.Blocked }},
		{DraftPosting, "", func(o orchestrator.TaskOutcome) bool { return o.Wait == "draft:1" }},
		{DraftFailed, "", func(o orchestrator.TaskOutcome) bool { return o.Wait == "draft:1" }},
	}
	for _, c := range cases {
		t.Run(c.state+c.comment, func(t *testing.T) {
			port := newP10Port()
			a := &OrchestratorActions{Drafts: port}
			run := p10Run(orchestrator.CapClickUpWrite)
			if _, err := a.RunTask(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			port.set(1, func(d *ClickUpDraft) { d.State, d.PostedCommentID = c.state, c.comment })
			out, err := a.ResumeTask(context.Background(), run, "draft:1")
			if err != nil || !c.check(out) {
				t.Fatalf("%+v %v", out, err)
			}
			if len(port.created) != 1 {
				t.Fatalf("resume made %d drafts", len(port.created))
			}
		})
	}
}

// TC-K1d (REQ-10, human): a retry after a crash finds the earlier draft and makes no second one;
// with none it makes exactly one.
func TestP10Actions_RetryAfterCrashMakesNoSecondDraft(t *testing.T) {
	port := newP10Port()
	a := &OrchestratorActions{Drafts: port}
	run := p10Run(orchestrator.CapClickUpWrite)
	if _, err := a.RunTask(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		out, err := a.ResumeTask(context.Background(), run, "")
		if err != nil || out.Wait != "draft:1" {
			t.Fatalf("%d: %+v %v", i, out, err)
		}
	}
	if len(port.created) != 1 {
		t.Fatalf("%d drafts after retries", len(port.created))
	}
	// The crash came before the draft existed: one is made now, and a second retry finds it.
	empty := newP10Port()
	b := &OrchestratorActions{Drafts: empty}
	if out, err := b.ResumeTask(context.Background(), run, ""); err != nil || out.Wait != "draft:1" {
		t.Fatalf("%+v %v", out, err)
	}
	if out, err := b.ResumeTask(context.Background(), run, ""); err != nil || out.Wait != "draft:1" || len(empty.created) != 1 {
		t.Fatalf("%+v %v drafts %d", out, err, len(empty.created))
	}
	// A store that cannot look drafts up reports the task interrupted rather than guessing.
	empty.noFinder = true
	if out, _ := b.ResumeTask(context.Background(), run, ""); out.ReasonCode != ReasonActionInterrupted {
		t.Fatalf("%+v", out)
	}
}

// TC-K1b (REQ-10, abuse): a wait reference or draft that is not this task's gives draft_missing.
func TestP10Actions_ResumeRejectsForeignDrafts(t *testing.T) {
	port := newP10Port()
	a := &OrchestratorActions{Drafts: port}
	run := p10Run(orchestrator.CapClickUpWrite)
	if _, err := a.RunTask(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	port.set(1, func(d *ClickUpDraft) { d.State = DraftPosted })
	foreign := map[string]func(*ClickUpDraft){
		"other execution": func(d *ClickUpDraft) { d.ExecutionID = 99 },
		"other task":      func(d *ClickUpDraft) { d.TaskID = "otherTask" },
		"other plan key":  func(d *ClickUpDraft) { d.PlanTaskKey = "run_02:w" },
		"assistant draft": func(d *ClickUpDraft) { d.Source = DraftSourceAssistant },
	}
	for name, f := range foreign {
		t.Run(name, func(t *testing.T) {
			port.set(1, f)
			defer port.set(1, func(d *ClickUpDraft) {
				d.ExecutionID, d.TaskID, d.PlanTaskKey, d.Source = 12, p10Task, "run_01:w", DraftSourceOrchestrator
			})
			out, err := a.ResumeTask(context.Background(), run, "draft:1")
			if err != nil || out.ReasonCode != ReasonActionDraftMissing || out.Succeeded {
				t.Fatalf("%+v %v", out, err)
			}
		})
	}
	for _, ref := range []string{"draft:x", "draft:0", "draft:-3", "5", "draft:77"} {
		out, err := a.ResumeTask(context.Background(), run, ref)
		if err != nil || out.ReasonCode != ReasonActionDraftMissing {
			t.Fatalf("ref %q: %+v %v", ref, out, err)
		}
	}
}

// TC-K1e (REQ-10): telegram.notify goes to the execution's topic once; a failure is a safe code;
// an interrupted notify is never sent again.
func TestP10Actions_NotifyOncePerTopic(t *testing.T) {
	say := &p10Sayer{}
	a := &OrchestratorActions{Owner: say}
	run := p10Run(orchestrator.CapTelegramNotify)
	out, err := a.RunTask(context.Background(), run)
	if err != nil || !out.Succeeded || len(out.Evidence) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if len(say.said) != 1 || !strings.HasPrefix(say.said[0], "WF-12: Báo kết quả") || say.to[0].ThreadID != 77 || say.to[0].ChatID != p10Chat {
		t.Fatalf("said %q to %+v", say.said, say.to)
	}
	// A retried step resumes: it must not say it again.
	for i := 0; i < 2; i++ {
		o, err := a.ResumeTask(context.Background(), run, "")
		if err != nil || o.Succeeded || o.ReasonCode != ReasonActionInterrupted {
			t.Fatalf("%+v %v", o, err)
		}
	}
	if len(say.said) != 1 {
		t.Fatalf("said %d times", len(say.said))
	}
	fail := &OrchestratorActions{Owner: &p10Sayer{err: errors.New("telegram 429 token=SECRET")}}
	o, _ := fail.RunTask(context.Background(), run)
	if o.ReasonCode != ReasonActionNotifyFailed || strings.Contains(o.Summary, "SECRET") {
		t.Fatalf("%+v", o)
	}
	empty := run
	empty.Task.Title = ""
	if o, _ := a.RunTask(context.Background(), empty); o.ReasonCode != ReasonActionInvalidText || len(say.said) != 1 {
		t.Fatalf("%+v", o)
	}
}

type p10Waker struct {
	mu    sync.Mutex
	woken []string
	err   error
}

func (w *p10Waker) WakeWorkerStep(_ context.Context, ws string, exec int64, step string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken = append(w.woken, ws+"|"+step)
	return w.err
}

// TC-K1b (REQ-10): the finished-draft listener wakes the waiting run step of orchestrator drafts only.
func TestP10Actions_DraftWakerWakesOrchestratorStepOnly(t *testing.T) {
	w := &p10Waker{}
	fn := OrchestratorDraftWaker(w)
	ctx := context.Background()
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceOrchestrator, ExecutionID: 12, PlanTaskKey: "run_03:w", State: DraftPosted})
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceAssistant, ExecutionID: 12, PlanTaskKey: "run_03:w"})
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceOrchestrator, ExecutionID: 0, PlanTaskKey: "run_03:w"})
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceOrchestrator, ExecutionID: 12, PlanTaskKey: "nokey"})
	if len(w.woken) != 1 || w.woken[0] != p10WS+"|run_03" {
		t.Fatalf("woken %v", w.woken)
	}
	// An execution that already moved on (not eligible) or a failing waker never panics.
	w.err = workflow.ErrNotEligible
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceOrchestrator, ExecutionID: 12, PlanTaskKey: "run_01:w"})
	w.err = errors.New("db")
	fn(ctx, ClickUpDraftOutcome{WorkspaceID: p10WS, Source: DraftSourceOrchestrator, ExecutionID: 12, PlanTaskKey: "run_01:w"})
	OrchestratorDraftWaker(nil)(ctx, ClickUpDraftOutcome{Source: DraftSourceOrchestrator, ExecutionID: 1, PlanTaskKey: "run_01:w"})
}

type p10RouteStore struct{ got []LocalTaskRoute }

func (s *p10RouteStore) EnsureOrchestratedRoute(_ context.Context, r LocalTaskRoute) error {
	s.got = append(s.got, r)
	return nil
}

// TC-K3a (REQ-12): PrepareJob stores the execution's topic as the job's route.
func TestP10Actions_PrepareJobStoresTopicRoute(t *testing.T) {
	store := &p10RouteStore{}
	jobs := OrchestratorJobs{Routes: store}
	route := orchestrator.Route{WorkspaceID: p10WS, ExecutionID: 12, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: 77, TaskRef: "FC-1", ClickUpTaskID: p10Task}
	if err := jobs.PrepareJob(context.Background(), route, "run_01", "org/alpha", 3); err != nil {
		t.Fatal(err)
	}
	if len(store.got) != 1 {
		t.Fatalf("%d routes", len(store.got))
	}
	r := store.got[0]
	if r.ExecutionID != 12 || r.ChatID != p10Chat || r.ThreadID != 77 || r.Repo != "org/alpha" || r.ClickUpTaskID != p10Task || r.InstructionVersion != 3 {
		t.Fatalf("route %+v", r)
	}
	if err := jobs.PrepareJob(context.Background(), route, "run_01", "", 3); err != nil || len(store.got) != 1 {
		t.Fatalf("an empty repo must store nothing: %v %d", err, len(store.got))
	}
	if err := jobs.PrepareJob(context.Background(), route, "run_01", "org/alpha", -5); err != nil || store.got[1].InstructionVersion != 0 {
		t.Fatalf("negative version: %v %+v", err, store.got)
	}
}
