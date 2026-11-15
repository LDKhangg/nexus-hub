package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

func phase6ConversationStore(t *testing.T, e *wfEnv) *ConversationStore {
	t.Helper()
	mustExec(t, e.ctx, e.pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram'),($3,$2,'telegram')`, integrationA, workspaceA, integrationB)
	store, err := NewConversationStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TC-A1.1/A1.2/A1.3/A2.1/A4.1/A5.1 (REQ-1, REQ-2, REQ-4, REQ-5):
// conversation identity is scoped, messages are metadata-only, summaries are
// sanitized and byte-bounded, and workflow/resource links are idempotent and
// workspace isolated.
func TestPhase6ConversationStorageScopeMetadataAndBindings(t *testing.T) {
	e := newWFEnv(t)
	store := phase6ConversationStore(t, e)
	versionA := e.enabledVersion(workspaceA, "phase6-a")
	versionB := e.enabledVersion(workspaceB, "phase6-b")
	exA := e.start(workspaceA, versionA.ID, "phase6-a", chainDef("one"))
	exB := e.start(workspaceB, versionB.ID, "phase6-b", chainDef("one"))

	first, err := store.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001})
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001})
	if err != nil || again.ID != first.ID {
		t.Fatalf("ensure idempotency: first=%d again=%d err=%v", first.ID, again.ID, err)
	}
	otherBot, err := store.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationB, ChatID: 7001})
	if err != nil || otherBot.ID == first.ID {
		t.Fatalf("integration isolation: first=%d other=%d err=%v", first.ID, otherBot.ID, err)
	}

	msg, err := store.RecordMessage(e.ctx, workflow.Message{WorkspaceID: workspaceA, ConversationID: first.ID, Direction: "in", Kind: "text", TelegramMessageID: 91, ReplyToMessageID: 90})
	if err != nil || msg.Direction != "in" || msg.TelegramMessageID != 91 || msg.ReplyToMessageID != 90 {
		t.Fatalf("record metadata: msg=%+v err=%v", msg, err)
	}
	var forbiddenColumns int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='conversation_messages' AND column_name IN ('body','text','content','message')`).Scan(&forbiddenColumns); err != nil {
		t.Fatal(err)
	}
	if forbiddenColumns != 0 {
		t.Fatalf("conversation_messages has %d raw-content columns", forbiddenColumns)
	}
	if _, err := store.RecordMessage(e.ctx, workflow.Message{WorkspaceID: workspaceB, ConversationID: first.ID, Direction: "in", Kind: "text", TelegramMessageID: 92}); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("cross-workspace message err=%v, want ErrNotFound", err)
	}

	if err := store.SetSummary(e.ctx, workspaceA, first.ID, " remembered\x00 fact\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSummary(e.ctx, workspaceA, first.ID); err != nil || got != " remembered fact" {
		t.Fatalf("summary=%q err=%v", got, err)
	}
	if err := store.SetSummary(e.ctx, workspaceA, first.ID, strings.Repeat("é", 1001)); !errors.Is(err, workflow.ErrInvalidInput) {
		t.Fatalf("over-2000-byte summary err=%v, want ErrInvalidInput", err)
	}
	if _, err := store.GetSummary(e.ctx, workspaceB, first.ID); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("cross-workspace summary err=%v, want ErrNotFound", err)
	}

	for i := 0; i < 2; i++ {
		if err := store.BindWorkflow(e.ctx, workspaceA, first.ID, exA.ID, "telegram.inbound"); err != nil {
			t.Fatal(err)
		}
		if err := store.BindResource(e.ctx, workflow.ResourceBinding{WorkspaceID: workspaceA, ExecutionID: exA.ID, Kind: "clickup.task", ExternalID: "TASK-7"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := store.ListBoundWorkflows(e.ctx, workspaceA, first.ID); err != nil || len(got) != 1 || got[0] != exA.ID {
		t.Fatalf("workflow bindings=%v err=%v", got, err)
	}
	if got, err := store.ExecutionsByResource(e.ctx, workspaceA, "clickup.task", "TASK-7"); err != nil || len(got) != 1 || got[0] != exA.ID {
		t.Fatalf("resource bindings=%v err=%v", got, err)
	}
	if got, err := store.ExecutionsByResource(e.ctx, workspaceB, "clickup.task", "TASK-7"); err != nil || len(got) != 0 {
		t.Fatalf("cross-workspace resource bindings=%v err=%v", got, err)
	}
	if err := store.BindWorkflow(e.ctx, workspaceA, first.ID, exB.ID, "wrong-workspace"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("cross-workspace execution bind err=%v, want ErrNotFound", err)
	}
}

// TC-A5.2 (REQ-5): starting an execution atomically creates deduplicated
// bindings for explicit resources and trusted source/subject references.
func TestPhase6StartExecutionCreatesResourceBindings(t *testing.T) {
	e := newWFEnv(t)
	store, err := NewConversationStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	v := e.enabledVersion(workspaceA, "phase6-resources")
	req := e.startReq(workspaceA, v.ID, "resources", chainDef("one"))
	req.References.Subject = behavior.TaskSubject("task-9")
	req.ResourceReferences = []workflow.ResourceReference{{Kind: "github.pr", ExternalID: "org/repo#8"}, {Kind: "github.pr", ExternalID: "org/repo#8"}}
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []workflow.ResourceReference{{Kind: "github.pr", ExternalID: "org/repo#8"}, {Kind: "clickup.task", ExternalID: "task-9"}, {Kind: "clickup.comment", ExternalID: "comment-1"}} {
		got, findErr := store.ExecutionsByResource(e.ctx, workspaceA, resource.Kind, resource.ExternalID)
		if findErr != nil || len(got) != 1 || got[0] != ex.ID {
			t.Errorf("resource %+v executions=%v err=%v", resource, got, findErr)
		}
	}
	chatReq := e.startReq(workspaceA, v.ID, "chat-resources", chainDef("one"))
	chatReq.References.Subject = behavior.ChatSubject("message-7")
	chatEx, err := e.store.StartExecution(e.ctx, chatReq)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ExecutionsByResource(e.ctx, workspaceA, "clickup.chat_message", "message-7")
	if err != nil || len(got) != 1 || got[0] != chatEx.ID {
		t.Fatalf("chat subject executions=%v err=%v", got, err)
	}
}

// TC-A1.4 (REQ-1): instruction appends sanitize/cap text, allocate monotonic
// versions under concurrency, and replay one Telegram update workspace-wide.
func TestPhase6InstructionsConcurrentAndIdempotent(t *testing.T) {
	e := newWFEnv(t)
	store := phase6ConversationStore(t, e)
	v := e.enabledVersion(workspaceA, "phase6-instructions")
	ex1 := e.start(workspaceA, v.ID, "instructions-1", chainDef("one"))
	ex2 := e.start(workspaceA, v.ID, "instructions-2", chainDef("one"))
	conv, err := store.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 8001})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendInstruction(e.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex1.ID, ConversationID: conv.ID, TelegramUpdateID: 500, Text: "do\x00 this " + strings.Repeat("界", 2100)})
	if err != nil || first.Version != 1 || strings.ContainsRune(first.Text, '\x00') || len([]rune(first.Text)) != 2000 {
		t.Fatalf("first instruction=%+v runes=%d err=%v", first, len([]rune(first.Text)), err)
	}
	replayed, err := store.AppendInstruction(e.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex1.ID, ConversationID: conv.ID, TelegramUpdateID: 500, Text: "different"})
	if err != nil || replayed != first {
		t.Fatalf("same-execution replay=%+v err=%v", replayed, err)
	}
	if _, err := store.AppendInstruction(e.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex2.ID, ConversationID: conv.ID, TelegramUpdateID: 500, Text: "different"}); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("cross-execution update reuse err=%v, want ErrConflict", err)
	}

	const concurrent = 8
	versions := make(chan int64, concurrent)
	errs := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			instruction, appendErr := store.AppendInstruction(e.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex1.ID, ConversationID: conv.ID, TelegramUpdateID: int64(501 + i), Text: "next"})
			if appendErr == nil {
				versions <- instruction.Version
			}
			errs <- appendErr
		}(i)
	}
	wg.Wait()
	close(versions)
	close(errs)
	for appendErr := range errs {
		if appendErr != nil {
			t.Fatalf("concurrent append: %v", appendErr)
		}
	}
	gotVersions := make([]int, 0, concurrent)
	for version := range versions {
		gotVersions = append(gotVersions, int(version))
	}
	sort.Ints(gotVersions)
	for i, version := range gotVersions {
		if version != i+2 {
			t.Fatalf("versions=%v, want contiguous 2..%d", gotVersions, concurrent+1)
		}
	}
}

// TC-A1.5 (REQ-1): an accepted update remains replayable after the workflow
// reaches a terminal state; eligibility must not override idempotency.
func TestPhase6InstructionReplayAfterTerminalReturnsStoredResult(t *testing.T) {
	e := newWFEnv(t)
	store := phase6ConversationStore(t, e)
	v := e.enabledVersion(workspaceA, "phase6-terminal-replay")
	ex := e.start(workspaceA, v.ID, "terminal-replay", chainDef("one"))
	conv, err := store.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 8101})
	if err != nil {
		t.Fatal(err)
	}
	req := workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex.ID, ConversationID: conv.ID, TelegramUpdateID: 900, Text: "remember this"}
	want, err := store.AppendInstruction(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "phase6-terminal"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.AppendInstruction(e.ctx, req)
	if err != nil || got != want {
		t.Fatalf("terminal replay=(%+v, %v), want stored %+v", got, err, want)
	}
}

// TC-A1.6 (REQ-1): migration 00023 has executable Up and Down sections and
// can recreate all conversation tables after rollback.
func TestPhase6ConversationMigrationRoundTrip(t *testing.T) {
	ctx, pool := testDB(t)
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../db/migrations/00023_conversations.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("migration has no Down section")
	}
	up = strings.TrimPrefix(up, "-- +goose Up")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('conversations','conversation_messages','conversation_workflows','resource_bindings','workflow_instructions')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 5 {
		t.Fatalf("recreated tables=%d, want 5", tables)
	}
}
