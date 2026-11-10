package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedThreads(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, ctx, pool, "INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')", workspaceA, workspaceB)
	mustExec(t, ctx, pool, "INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'github'),($3,$4,'clickup')", integrationA, workspaceA, integrationB, workspaceB)
	mustExec(t, ctx, pool, "INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'pr','1'),($4,$2,$3,'pr','2'),($5,$6,$7,'task','1')", threadA, workspaceA, integrationA, threadB, threadC, workspaceB, integrationB)
}

func TestMessageCRUD(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := r.List(ctx, workspaceA, threadA, 10, 0)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty: %v %v", empty, err)
	}
	externalID, sender := "comment:1", "author-1"
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	input := CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ExternalMessageID: &externalID, SenderID: &sender, SenderName: "Author", SenderRole: memory.SenderRoleExternalUser, Content: "original", OccurredAt: when, Metadata: map[string]any{"source": "test"}}
	first, err := r.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID <= 0 || first.ThreadID != threadA || !first.OccurredAt.Equal(when) || first.CreatedAt.IsZero() || *first.SenderID != sender || *first.ExternalMessageID != externalID || first.Metadata["source"] != "test" {
		t.Fatalf("create: %+v", first)
	}
	got, err := r.Get(ctx, workspaceA, threadA, first.ID)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("get: %+v %v", got, err)
	}
	_, err = r.Create(ctx, input)
	requirePGCode(t, err, "23505")
	input.ThreadID = threadB
	if _, err = r.Create(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.ThreadID, input.ExternalMessageID, input.SenderID, input.OccurredAt = threadA, nil, nil, time.Time{}
	second, err := r.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	third, err := r.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExternalMessageID != nil || second.SenderID != nil || !second.OccurredAt.Equal(second.CreatedAt) {
		t.Fatalf("local message: %+v", second)
	}
	page, err := r.List(ctx, workspaceA, threadA, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != second.ID || third.ID <= second.ID {
		t.Fatalf("page: %+v %v", page, err)
	}
	updated, err := r.Update(ctx, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: first.ID, Content: "edited"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Content != "edited" || len(updated.Metadata) != 0 || updated.SenderName != first.SenderName || updated.SenderRole != first.SenderRole || *updated.SenderID != sender || *updated.ExternalMessageID != externalID || !updated.OccurredAt.Equal(when) || !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("update: %+v", updated)
	}
	tr, _ := NewThreadRepository(pool)
	requirePGCode(t, tr.Delete(ctx, workspaceA, threadA), "23503")
	if err = r.Delete(ctx, workspaceA, threadA, first.ID); err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceA, threadA, first.ID)
	if !errors.Is(err, memory.ErrMessageNotFound) {
		t.Fatal(err)
	}
	_, err = r.Update(ctx, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: first.ID})
	if !errors.Is(err, memory.ErrMessageNotFound) {
		t.Fatal(err)
	}
	if err = r.Delete(ctx, workspaceA, threadA, first.ID); !errors.Is(err, memory.ErrMessageNotFound) {
		t.Fatal(err)
	}
}

func TestMessageWorkspaceIsolation(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, _ := NewMessageRepository(pool)
	first, err := r.Create(ctx, CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, SenderRole: memory.SenderRoleSystem, Content: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]string{{workspaceB, threadA}, {workspaceA, threadB}, {workspaceB, threadC}} {
		_, err = r.Get(ctx, scope[0], scope[1], first.ID)
		if !errors.Is(err, memory.ErrMessageNotFound) {
			t.Fatal(err)
		}
		_, err = r.Update(ctx, UpdateMessageInput{WorkspaceID: scope[0], ThreadID: scope[1], ID: first.ID, Content: "wrong"})
		if !errors.Is(err, memory.ErrMessageNotFound) && !errors.Is(err, relay.ErrThreadNotFound) {
			t.Fatal(err)
		}
		err = r.Delete(ctx, scope[0], scope[1], first.ID)
		if !errors.Is(err, memory.ErrMessageNotFound) && !errors.Is(err, relay.ErrThreadNotFound) {
			t.Fatal(err)
		}
	}
	_, err = r.Create(ctx, CreateMessageInput{WorkspaceID: workspaceB, ThreadID: threadA, SenderRole: memory.SenderRoleSystem})
	if !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	list, err := r.List(ctx, workspaceB, threadA, 10, 0)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("isolation: %v %v", list, err)
	}
	got, err := r.Get(ctx, workspaceA, threadA, first.ID)
	if err != nil || got.Content != "safe" {
		t.Fatalf("mutated: %+v %v", got, err)
	}
}

func TestMessageSummaryInvalidation(t *testing.T) {
	for _, action := range []string{"update", "delete"} {
		for index := 0; index < 3; index++ {
			t.Run(action+string(rune('1'+index)), func(t *testing.T) {
				ctx, pool := testDB(t)
				seedThreads(t, ctx, pool)
				r, _ := NewMessageRepository(pool)
				ids := make([]int64, 3)
				for i := range ids {
					m, err := r.Create(ctx, CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, SenderRole: memory.SenderRoleSystem})
					if err != nil {
						t.Fatal(err)
					}
					ids[i] = m.ID
				}
				mustExec(t, ctx, pool, "UPDATE hub_threads SET rolling_summary='summary',summary_through_message_id=$2,updated_at='2020-01-01' WHERE id=$1", threadA, ids[1])
				var err error
				if action == "update" {
					_, err = r.Update(ctx, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: ids[index], Content: "changed"})
				} else {
					err = r.Delete(ctx, workspaceA, threadA, ids[index])
				}
				if err != nil {
					t.Fatal(err)
				}
				tr, _ := NewThreadRepository(pool)
				got, err := tr.Get(ctx, workspaceA, threadA)
				if err != nil {
					t.Fatal(err)
				}
				if index <= 1 && (got.RollingSummary != "" || got.SummaryThroughMessageID != nil) {
					t.Fatalf("stale summary: %+v", got)
				}
				if index == 2 && (got.RollingSummary != "summary" || got.SummaryThroughMessageID == nil || *got.SummaryThroughMessageID != ids[1]) {
					t.Fatalf("tail invalidated summary: %+v", got)
				}
				if got.UpdatedAt.Year() == 2020 {
					t.Fatal("thread timestamp not refreshed")
				}
			})
		}
	}
}

func TestMessageRollback(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, _ := NewMessageRepository(pool)
	m, err := r.Create(ctx, CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, SenderRole: memory.SenderRoleSystem, Content: "original"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, "UPDATE hub_threads SET rolling_summary='summary',summary_through_message_id=$2,updated_at='2020-01-01' WHERE id=$1", threadA, m.ID)
	// PostgreSQL rejects the NUL after summary invalidation has run inside the transaction.
	_, err = r.Update(ctx, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: m.ID, Content: "invalid\x00"})
	if err == nil {
		t.Fatal("expected PostgreSQL text rejection")
	}
	_, err = r.Update(ctx, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: m.ID, Metadata: map[string]any{"bad": make(chan int)}})
	if err == nil {
		t.Fatal("expected invalid JSON rejection")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = r.Update(canceled, UpdateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, ID: m.ID})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, workspaceA, threadA, m.ID)
	if err != nil || got.Content != "original" {
		t.Fatalf("rollback content: %+v %v", got, err)
	}
	tr, _ := NewThreadRepository(pool)
	thread, err := tr.Get(ctx, workspaceA, threadA)
	if err != nil || thread.RollingSummary != "summary" || thread.SummaryThroughMessageID == nil || *thread.SummaryThroughMessageID != m.ID || thread.UpdatedAt.Year() != 2020 {
		t.Fatalf("rollback summary: %+v %v", thread, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT id FROM hub_threads WHERE id=$1 FOR UPDATE", threadA); err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer waitCancel()
	_, err = r.Create(waitCtx, CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, SenderRole: memory.SenderRoleSystem})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Create(ctx, CreateMessageInput{WorkspaceID: workspaceA, ThreadID: threadA, SenderRole: memory.SenderRoleSystem}); err != nil {
		t.Fatalf("leaked lock: %v", err)
	}
	list, err := r.List(ctx, workspaceA, threadA, 10, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("failed insert persisted: %+v %v", list, err)
	}
	thread, err = tr.Get(ctx, workspaceA, threadA)
	if err != nil || thread.UpdatedAt.Year() == 2020 || thread.RollingSummary != "summary" {
		t.Fatalf("create timestamp/summary: %+v %v", thread, err)
	}
}
