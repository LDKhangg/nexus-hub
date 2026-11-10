package storage

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

func TestThreadLinkCRUD(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, err := NewThreadLinkRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	items, err := r.List(ctx, workspaceA, threadA, 10, 0)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty list: %v, %v", items, err)
	}
	created, err := r.Create(ctx, CreateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadB, TargetThreadID: threadA, Metadata: map[string]any{"reason": "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.SourceThreadID != threadA || created.TargetThreadID != threadB || created.CreatedAt.IsZero() {
		t.Fatalf("canonical link: %+v", created)
	}
	got, err := r.Get(ctx, strings.ToUpper(workspaceA), strings.ToUpper(threadB), strings.ToUpper(threadA))
	if err != nil || !reflect.DeepEqual(got, created) {
		t.Fatalf("reverse uppercase get: %+v, %v", got, err)
	}
	_, err = r.Create(ctx, CreateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadA, TargetThreadID: threadB})
	requirePGCode(t, err, "23505")
	_, err = r.Create(ctx, CreateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadA, TargetThreadID: strings.ToUpper(threadA)})
	if err == nil {
		t.Fatal("self link accepted")
	}
	updated, err := r.Update(ctx, UpdateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadB, TargetThreadID: threadA})
	if err != nil || len(updated.Metadata) != 0 || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("update: %+v, %v", updated, err)
	}
	for _, endpoint := range []string{threadA, threadB} {
		items, err := r.List(ctx, workspaceA, endpoint, 1, 0)
		if err != nil || len(items) != 1 || !reflect.DeepEqual(items[0], updated) {
			t.Fatalf("endpoint list: %+v, %v", items, err)
		}
		items, err = r.List(ctx, workspaceA, endpoint, 1, 1)
		if err != nil || len(items) != 0 {
			t.Fatalf("offset list: %+v, %v", items, err)
		}
	}
	threads, _ := NewThreadRepository(pool)
	requirePGCode(t, threads.Delete(ctx, workspaceA, threadB), "23503")
	if err := r.Delete(ctx, workspaceA, threadB, threadA); err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceA, threadA, threadB)
	if !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	_, err = r.Update(ctx, UpdateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadA, TargetThreadID: threadB})
	if !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("missing update: %v", err)
	}
	if err = r.Delete(ctx, workspaceA, threadA, threadB); !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("missing delete: %v", err)
	}
	if err = threads.Delete(ctx, workspaceA, threadB); err != nil {
		t.Fatal(err)
	}
}

func TestThreadLinkWorkspaceIsolation(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, _ := NewThreadLinkRepository(pool)
	_, err := r.Create(ctx, CreateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadA, TargetThreadID: threadC})
	requirePGCode(t, err, "23503")
	_, err = r.Create(ctx, CreateThreadLinkInput{WorkspaceID: workspaceA, SourceThreadID: threadA, TargetThreadID: threadB})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceB, threadA, threadB)
	if !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("wrong workspace get: %v", err)
	}
	_, err = r.Update(ctx, UpdateThreadLinkInput{WorkspaceID: workspaceB, SourceThreadID: threadA, TargetThreadID: threadB, Metadata: map[string]any{"bad": true}})
	if !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("wrong workspace update: %v", err)
	}
	if err = r.Delete(ctx, workspaceB, threadA, threadB); !errors.Is(err, relay.ErrThreadLinkNotFound) {
		t.Fatalf("wrong workspace delete: %v", err)
	}
	for _, scope := range [][2]string{{workspaceB, threadA}, {workspaceA, threadC}, {workspaceA, missingID}} {
		items, err := r.List(ctx, scope[0], scope[1], 10, 0)
		if err != nil || len(items) != 0 {
			t.Fatalf("leaked list: %+v, %v", items, err)
		}
	}
	got, err := r.Get(ctx, workspaceA, threadA, threadB)
	if err != nil || len(got.Metadata) != 0 {
		t.Fatalf("original changed: %+v, %v", got, err)
	}
}
