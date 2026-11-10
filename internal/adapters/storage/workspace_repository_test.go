package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

func TestWorkspaceCRUD(t *testing.T) {
	ctx, pool := testDB(t)
	r, err := NewWorkspaceRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := r.List(ctx, 10, 0)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list: %v %v", empty, err)
	}
	first, err := r.Create(ctx, CreateWorkspaceInput{ID: workspaceA, DisplayName: "First"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != workspaceA || first.DisplayName != "First" || first.CreatedAt.IsZero() {
		t.Fatalf("create: %+v", first)
	}
	got, err := r.Get(ctx, workspaceA)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("get: %+v %v", got, err)
	}
	_, err = r.Create(ctx, CreateWorkspaceInput{ID: workspaceA, DisplayName: "Duplicate"})
	requirePGCode(t, err, "23505")
	_, err = r.Create(ctx, CreateWorkspaceInput{ID: workspaceB, DisplayName: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, "UPDATE workspaces SET updated_at = '2020-01-01'")
	page, err := r.List(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != workspaceA {
		t.Fatalf("page: %+v %v", page, err)
	}
	updated, err := r.Update(ctx, UpdateWorkspaceInput{ID: workspaceA, DisplayName: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Renamed" || !updated.CreatedAt.Equal(first.CreatedAt) || !updated.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("update: %+v", updated)
	}
	if err := r.Delete(ctx, workspaceA); err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceA)
	if !errors.Is(err, tenant.ErrWorkspaceNotFound) {
		t.Fatal(err)
	}
	_, err = r.Update(ctx, UpdateWorkspaceInput{ID: workspaceA, DisplayName: "Missing"})
	if !errors.Is(err, tenant.ErrWorkspaceNotFound) {
		t.Fatal(err)
	}
	if err = r.Delete(ctx, workspaceA); !errors.Is(err, tenant.ErrWorkspaceNotFound) {
		t.Fatal(err)
	}
}

func TestWorkspaceInvalidInput(t *testing.T) {
	if _, err := NewWorkspaceRepository(nil); err == nil {
		t.Fatal("nil pool accepted")
	}
	r := &WorkspaceRepository{}
	ctx := context.Background()
	if _, err := r.Create(ctx, CreateWorkspaceInput{ID: "bad"}); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if _, err := r.Get(ctx, ""); err == nil {
		t.Fatal("empty ID accepted")
	}
	if _, err := r.List(ctx, 0, 0); err == nil {
		t.Fatal("invalid page accepted")
	}
	if _, err := r.Update(ctx, UpdateWorkspaceInput{ID: "bad"}); err == nil {
		t.Fatal("invalid update accepted")
	}
	if err := r.Delete(ctx, "bad"); err == nil {
		t.Fatal("invalid delete accepted")
	}
}
