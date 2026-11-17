package behavior

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Test debt TD-7A-2, TD-7A-3, TD-7A-4 (thresholds) and TD-TT-4. Fakes only.

// TC-D2 (TD-7A-2): "/task please do X" outside a topic is a usage reply, never a started task.
func TestTD_7A2_TaskOutsideTopicWithoutTaskID(t *testing.T) {
	for _, args := range []string{
		"please do X",
		"fix the login bug",
		"Update the docs now",
		"đổi tên nút đăng nhập",
		"acme/api please do X", // a repo word does not make a task id
		"🐛 fix it",
	} {
		t.Run(args, func(t *testing.T) {
			e := b3TaskWorker("acme/api")
			out := e.task(args, 0)
			if out.code != "usage" || len(e.started()) != 0 {
				t.Fatalf("code=%q started=%v replies=%v", out.code, e.started(), e.m.texts())
			}
			if got := e.m.texts(); len(got) != 1 || !strings.Contains(got[0], "/task <TASK-ID>") {
				t.Fatalf("usage reply %v", got)
			}
			if len(e.links.calls) != 0 {
				t.Fatalf("resolver called for a non-id word: %v", e.links.calls)
			}
			if len(e.conv.instructions) != 0 {
				t.Fatalf("instruction stored: %+v", e.conv.instructions)
			}
		})
	}
	// The same words inside a task topic are the instruction.
	e := b3TaskWorker("acme/api")
	if out := e.task("please do X", b1Thread); out.code != "" || len(e.started()) != 1 {
		t.Fatalf("in topic: code=%q started=%v", out.code, e.started())
	}
}

// TC-D3 (TD-7A-3): topic text during a running local task becomes an instruction that the worker receives with its heartbeats.
func TestTD_7A3_TopicTextBecomesWorkerInstruction(t *testing.T) {
	ctx := context.Background()
	r := &phase6BReader{details: map[int64]workflow.ExecutionDetail{41: phase6BDetail(41, p4WS, workflow.StateRunning)}}
	c := &phase6BConversations{}
	w := phase6BWorker(r, c, &p4Messenger{})
	// The /task text is instruction v1 and was delivered with the job.
	if _, err := c.AppendInstruction(ctx, workflow.InstructionRequest{WorkspaceID: p4WS, ExecutionID: 41, Text: "initial task", TelegramUpdateID: 1}); err != nil {
		t.Fatal(err)
	}
	routes := newB3Routes()
	routes.routes[41].InstructionVersion = 1
	src := &LocalTaskInstructions{Routes: routes, Reader: c}
	job := b3Job(WorkerJobRunning)

	if got, err := src.PendingWorkerInstructions(ctx, job); err != nil || len(got) != 0 {
		t.Fatalf("nothing is pending before the owner writes: %+v %v", got, err)
	}
	// The owner writes in the task topic (the topic's only active task is the target).
	tt := &assistantToolTurn{u: InboundUpdate{WorkspaceID: p4WS, UpdateID: 90}, to: ChatRef{WorkspaceID: p4WS}, workflowID: 41}
	text, failed := w.toolWorkflowNote(ctx, tt, toolArgs{"id": float64(41), "text": "also add\x07 docs\x00"})
	if failed || !strings.Contains(text, "v2") {
		t.Fatalf("note: %q failed=%v", text, failed)
	}
	for i := 0; i < 3; i++ { // resent with every heartbeat until acked
		got, err := src.PendingWorkerInstructions(ctx, job)
		if err != nil || len(got) != 1 || got[0].ID != "v2" || got[0].Text != "also add docs" {
			t.Fatalf("heartbeat %d: %+v %v", i, got, err)
		}
	}
	if err := src.AckWorkerInstructions(ctx, job, []string{"v2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := src.PendingWorkerInstructions(ctx, job); len(got) != 0 {
		t.Fatalf("acked instruction resent: %+v", got)
	}
	// A second message arrives later and is the only one pending.
	tt2 := &assistantToolTurn{u: InboundUpdate{WorkspaceID: p4WS, UpdateID: 91}, to: ChatRef{WorkspaceID: p4WS}, workflowID: 41}
	if text, failed := w.toolWorkflowNote(ctx, tt2, toolArgs{"id": float64(41), "text": "and a changelog line"}); failed {
		t.Fatalf("second note: %q", text)
	}
	got, _ := src.PendingWorkerInstructions(ctx, job)
	if len(got) != 1 || got[0].ID != "v3" || got[0].Text != "and a changelog line" {
		t.Fatalf("pending %+v", got)
	}
	// Another workspace's job never sees them; text over the limit is refused and queued nowhere.
	other := job
	other.WorkspaceID = "019a6690-abcd-7000-8000-0000000000ff"
	if got, _ := src.PendingWorkerInstructions(ctx, other); len(got) != 0 {
		t.Fatalf("foreign workspace got %+v", got)
	}
	tt3 := &assistantToolTurn{u: InboundUpdate{WorkspaceID: p4WS, UpdateID: 92}, to: ChatRef{WorkspaceID: p4WS}, workflowID: 41}
	if _, failed := w.toolWorkflowNote(ctx, tt3, toolArgs{"id": float64(41), "text": strings.Repeat("x", 2001)}); !failed {
		t.Fatal("over-long instruction accepted")
	}
	if n := len(c.instructions[41]); n != 3 {
		t.Fatalf("instructions stored: %d", n)
	}
}

// TC-D4 (TD-7A-4): every telemetry threshold blocks at its boundary and the owner hears the matching text once.
func TestTD_7A4_ThresholdBoundariesAndTexts(t *testing.T) {
	now := b3Now
	th := DefaultWorkerThresholds
	i64 := func(v int64) *int64 { return &v }
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	gib := int64(1) << 30
	cases := []struct {
		name   string
		snap   *WorkerTelemetry
		reason string
		text   string
	}{
		{"nil snapshot never blocks", nil, "", ""},
		{"unknown values never block", &WorkerTelemetry{State: "idle"}, "", ""},
		{"session at the minimum is fine", &WorkerTelemetry{ClaudeSessionLeftPct: i64(10)}, "", ""},
		{"session below the minimum blocks", &WorkerTelemetry{ClaudeSessionLeftPct: i64(9), ClaudeSessionResetAt: at(time.Hour)},
			WorkerBlockClaudeSession, "hạn mức Claude, chờ reset lúc " + now.Add(time.Hour).UTC().Format("2006-01-02 15:04") + " UTC"},
		{"session low without a reset time", &WorkerTelemetry{ClaudeSessionLeftPct: i64(0)}, WorkerBlockClaudeSession, "hạn mức Claude, chờ reset."},
		{"session low but reset time passed", &WorkerTelemetry{ClaudeSessionLeftPct: i64(0), ClaudeSessionResetAt: at(-time.Second)}, "", ""},
		{"session low and reset exactly now", &WorkerTelemetry{ClaudeSessionLeftPct: i64(0), ClaudeSessionResetAt: at(0)}, "", ""},
		{"weekly low blocks", &WorkerTelemetry{ClaudeWeeklyLeftPct: i64(5), ClaudeWeeklyResetAt: at(48 * time.Hour)}, WorkerBlockClaudeWeekly, "hạn mức Claude"},
		{"session wins over weekly", &WorkerTelemetry{ClaudeSessionLeftPct: i64(1), ClaudeWeeklyLeftPct: i64(1), ClaudeWeeklyResetAt: at(time.Hour)}, WorkerBlockClaudeSession, "hạn mức Claude"},
		{"session reset passed, weekly still low", &WorkerTelemetry{ClaudeSessionLeftPct: i64(1), ClaudeSessionResetAt: at(-time.Minute), ClaudeWeeklyLeftPct: i64(1)}, WorkerBlockClaudeWeekly, "hạn mức Claude"},
		{"disk at the minimum is fine", &WorkerTelemetry{DiskUsedBytes: i64(95 * gib), DiskTotalBytes: i64(100 * gib)}, "", ""},
		{"disk below the minimum blocks", &WorkerTelemetry{DiskUsedBytes: i64(100*gib - 5*gib + 1), DiskTotalBytes: i64(100 * gib)}, WorkerBlockDisk, "dung lượng ổ đĩa"},
		{"disk used unknown", &WorkerTelemetry{DiskTotalBytes: i64(1)}, "", ""},
		{"RAM at the maximum is fine", &WorkerTelemetry{RAMUsedBytes: i64(90), RAMTotalBytes: i64(100)}, "", ""},
		{"RAM above the maximum blocks", &WorkerTelemetry{RAMUsedBytes: i64(91), RAMTotalBytes: i64(100)}, WorkerBlockRAM, "quá nhiều RAM"},
		{"RAM total unknown or zero", &WorkerTelemetry{RAMUsedBytes: i64(91), RAMTotalBytes: i64(0)}, "", ""},
		{"Claude wins over disk and RAM", &WorkerTelemetry{ClaudeSessionLeftPct: i64(0), DiskUsedBytes: i64(100 * gib), DiskTotalBytes: i64(100 * gib), RAMUsedBytes: i64(99), RAMTotalBytes: i64(100)}, WorkerBlockClaudeSession, "hạn mức Claude"},
		{"disk wins over RAM", &WorkerTelemetry{DiskUsedBytes: i64(100 * gib), DiskTotalBytes: i64(100 * gib), RAMUsedBytes: i64(99), RAMTotalBytes: i64(100)}, WorkerBlockDisk, "dung lượng ổ đĩa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := th.Block(c.snap, now)
			if c.reason == "" {
				if b != nil {
					t.Fatalf("blocked: %+v", b)
				}
				return
			}
			if b == nil || b.Reason != c.reason {
				t.Fatalf("block %+v want %s", b, c.reason)
			}
			if got := workerBlockText(b); !strings.Contains(got, c.text) {
				t.Fatalf("text %q lacks %q", got, c.text)
			}
		})
	}
	// Custom thresholds move the boundaries.
	strict := WorkerThresholds{MinClaudeLeftPct: 50, MinDiskFreeBytes: 1, MaxRAMUsedPct: 10}
	if b := strict.Block(&WorkerTelemetry{ClaudeSessionLeftPct: i64(49)}, now); b == nil || b.Reason != WorkerBlockClaudeSession {
		t.Fatalf("strict session: %+v", b)
	}
	if b := strict.Block(&WorkerTelemetry{RAMUsedBytes: i64(11), RAMTotalBytes: i64(100)}, now); b == nil || b.Reason != WorkerBlockRAM {
		t.Fatalf("strict RAM: %+v", b)
	}
	if got := workerBlockText(nil); got != msgWorkerWaitingOffline {
		t.Fatalf("nil block text %q", got)
	}
}

// TC-D4b (TD-7A-4): the notifier tells the owner why a task waits, once per reason, for each threshold.
func TestTD_7A4_NotifierThresholdTexts(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	gib := int64(1) << 30
	for _, tc := range []struct {
		name string
		snap WorkerTelemetry
		want string
	}{
		{"disk", WorkerTelemetry{State: "idle", DiskUsedBytes: i64(99 * gib), DiskTotalBytes: i64(100 * gib)}, "dung lượng ổ đĩa"},
		{"ram", WorkerTelemetry{State: "idle", RAMUsedBytes: i64(95), RAMTotalBytes: i64(100)}, "quá nhiều RAM"},
		{"weekly", WorkerTelemetry{State: "idle", ClaudeWeeklyLeftPct: i64(2)}, "hạn mức Claude, chờ reset."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &fakeWorkerAdmin{devices: []WorkerDevice{b3Device("laptop", b3Now.Add(-10*time.Second), "repo.write")}}
			tel := &b3Telemetry{recs: []WorkerTelemetryRecord{{DeviceID: "dev-laptop", ReceivedAt: b3Now, Snapshot: tc.snap}}}
			e := b3Notifier(admin, tel)
			for i := 0; i < 3; i++ {
				e.n.WorkerJobQueued(context.Background(), b3Job(WorkerJobWaiting))
				e.drain()
			}
			got := e.m.texts()
			if len(got) != 1 || !strings.HasPrefix(got[0], "WF-41: ") || !strings.Contains(got[0], tc.want) {
				t.Fatalf("messages %v", got)
			}
			if e.routes.route(41).WaitingNotice != workerNoticeBlocked {
				t.Fatalf("notice state %q", e.routes.route(41).WaitingNotice)
			}
		})
	}
}

// --- TD-TT-4: /task remembers the repository of the task's last local task ---

type ttRepoStarter struct {
	*b3Starter
	last  map[string]string
	err   error
	calls [][2]string
}

func (s *ttRepoStarter) LastTaskRepo(_ context.Context, ws, task string) (string, error) {
	s.calls = append(s.calls, [2]string{ws, task})
	return s.last[task], s.err
}

func ttTaskWorker(repos []string, last map[string]string) (*b3TaskEnv, *ttRepoStarter) {
	e := &b3TaskEnv{m: &p4Messenger{}, st: &b3Starter{repos: repos}, conv: &phase6BConversations{}, links: &b3Links{}}
	mem := &ttRepoStarter{b3Starter: e.st, last: last}
	e.w = NewInboxWorker(InboxDeps{
		Messenger: e.m, Conversations: e.conv, LocalTasks: mem, Links: e.links,
		Topics: &b1Topics{rows: []Topic{b1TaskTopic(b1Task, b1Thread)}},
	}, InboxSettings{})
	e.w.now = func() time.Time { return b3Now }
	return e, mem
}

// TC-TT-4a (TD-TT-4, REQ-4): the remembered repo is used when several repos are registered.
func TestTD_TT4_RememberedRepoUsed(t *testing.T) {
	e, mem := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{b1Task: "acme/web"})
	if out := e.task("add a test", b1Thread); out.code != "" {
		t.Fatalf("code %q replies %v", out.code, e.m.texts())
	}
	r := e.started()
	if len(r) != 1 || r[0].Repo != "acme/web" {
		t.Fatalf("started %+v", r)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "add a test" {
		t.Fatalf("instruction %+v", got)
	}
	if len(mem.calls) != 1 || mem.calls[0] != [2]string{p4WS, b1Task} {
		t.Fatalf("memory read %v", mem.calls)
	}
	// The registered spelling is used, whatever case was remembered.
	e2, _ := ttTaskWorker([]string{"Acme/Api", "Acme/Web"}, map[string]string{b1Task: "acme/WEB"})
	e2.task("do it", b1Thread)
	if r := e2.started(); len(r) != 1 || r[0].Repo != "Acme/Web" {
		t.Fatalf("spelling: %+v", r)
	}
}

// TC-TT-4b (TD-TT-4, REQ-4): an explicit registered repo wins over the memory.
func TestTD_TT4_ExplicitRepoWins(t *testing.T) {
	e, _ := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{b1Task: "acme/web"})
	e.task("ACME/API add a test", b1Thread)
	r := e.started()
	if len(r) != 1 || r[0].Repo != "acme/api" {
		t.Fatalf("started %+v", r)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "add a test" {
		t.Fatalf("repo word leaked into the instruction: %+v", got)
	}
}

// TC-TT-4c (TD-TT-4, REQ-4): a remembered repo that is no longer registered falls back to the usage reply.
func TestTD_TT4_UnregisteredMemoryFallsBack(t *testing.T) {
	e, _ := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{b1Task: "gone/repo"})
	out := e.task("add a test", b1Thread)
	if out.code != "usage" || len(e.started()) != 0 {
		t.Fatalf("code=%q started=%v", out.code, e.started())
	}
	if got := e.m.texts(); len(got) != 1 || !strings.Contains(got[0], "acme/api") || !strings.Contains(got[0], "acme/web") || strings.Contains(got[0], "gone/repo") {
		t.Fatalf("reply %v", got)
	}
	// With a single registered repo the single repo is used, not the stale memory.
	one, _ := ttTaskWorker([]string{"acme/api"}, map[string]string{b1Task: "gone/repo"})
	one.task("do it", b1Thread)
	if r := one.started(); len(r) != 1 || r[0].Repo != "acme/api" {
		t.Fatalf("single repo: %+v", r)
	}
	// No repo registered at all: the no-repo reply, memory or not.
	none, _ := ttTaskWorker(nil, map[string]string{b1Task: "acme/web"})
	if out := none.task("do it", b1Thread); out.code != "no_repo" || len(none.started()) != 0 {
		t.Fatalf("no repo: %q", out.code)
	}
}

// TC-TT-4d (TD-TT-4, REQ-4): no history, a read error and an empty memory behave like today.
func TestTD_TT4_NoMemoryOrReadError(t *testing.T) {
	e, _ := ttTaskWorker([]string{"acme/api", "acme/web"}, nil)
	if out := e.task("do it", b1Thread); out.code != "usage" || len(e.started()) != 0 {
		t.Fatalf("no history: %q", out.code)
	}
	e2, mem := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{b1Task: "acme/web"})
	mem.err = context.DeadlineExceeded
	if out := e2.task("do it", b1Thread); out.code != "usage" || len(e2.started()) != 0 {
		t.Fatalf("read error must count as no memory: %q", out.code)
	}
	// A starter without the memory interface still works.
	plain := b3TaskWorker("acme/api", "acme/web")
	if out := plain.task("do it", b1Thread); out.code != "usage" {
		t.Fatalf("plain starter: %q", out.code)
	}
}

// TC-TT-4e (TD-TT-4, REQ-4): outside a topic the memory is keyed by the canonical task id, also for a custom id.
func TestTD_TT4_OutsideTopicUsesCanonicalTask(t *testing.T) {
	e, mem := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{"86plain": "acme/api", "86resolved": "acme/web"})
	e.task("86plain do it", 0)
	e.task("FC-6583 do it", 0)
	r := e.started()
	if len(r) != 2 || r[0].Repo != "acme/api" || r[1].Repo != "acme/web" {
		t.Fatalf("started %+v", r)
	}
	if len(mem.calls) != 2 || mem.calls[0][1] != "86plain" || mem.calls[1][1] != "86resolved" {
		t.Fatalf("memory keys %v (the custom id must be resolved first)", mem.calls)
	}
	// An unrelated task has no memory.
	e2, _ := ttTaskWorker([]string{"acme/api", "acme/web"}, map[string]string{"86plain": "acme/api"})
	if out := e2.task("86other do it", 0); out.code != "usage" || len(e2.started()) != 0 {
		t.Fatalf("other task: %q", out.code)
	}
}
