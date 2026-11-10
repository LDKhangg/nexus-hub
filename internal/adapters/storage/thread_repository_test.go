package storage

import (
	"errors"
	"reflect"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

func TestThreadCRUD(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, "INSERT INTO workspaces(id,display_name) VALUES($1,'A')", workspaceA)
	mustExec(t, ctx, pool, "INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'github')", integrationA, workspaceA)
	r, err := NewThreadRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	input := CreateThreadInput{ID: threadA, WorkspaceID: workspaceA, IntegrationID: integrationA, ExternalScope: "owner/repo", ThreadType: "pull_request", ExternalID: "42", Title: "Review", Metadata: map[string]any{"url": "local"}}
	first, err := r.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "Review" || first.ExternalID != "42" || first.IntegrationID != integrationA || first.Metadata["url"] != "local" || first.RollingSummary != "" || first.SummaryThroughMessageID != nil {
		t.Fatalf("create: %+v", first)
	}
	got, err := r.Get(ctx, workspaceA, threadA)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("get: %+v %v", got, err)
	}
	input.ID = threadB
	_, err = r.Create(ctx, input)
	requirePGCode(t, err, "23505")
	input.ExternalScope = "owner/other"
	_, err = r.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, "UPDATE hub_threads SET updated_at='2020-01-01'")
	page, err := r.List(ctx, workspaceA, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != threadA {
		t.Fatalf("page: %+v %v", page, err)
	}
	updated, err := r.Update(ctx, UpdateThreadInput{ID: threadA, WorkspaceID: workspaceA, Title: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Renamed" || updated.ExternalScope != first.ExternalScope || updated.ExternalID != first.ExternalID || updated.ThreadType != first.ThreadType || !updated.CreatedAt.Equal(first.CreatedAt) || len(updated.Metadata) != 0 {
		t.Fatalf("update: %+v", updated)
	}
	ir, _ := NewIntegrationRepository(pool)
	requirePGCode(t, ir.Delete(ctx, workspaceA, integrationA), "23503")
	wr, _ := NewWorkspaceRepository(pool)
	requirePGCode(t, wr.Delete(ctx, workspaceA), "23503")
	if err = r.Delete(ctx, workspaceA, threadA); err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceA, threadA)
	if !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	_, err = r.Update(ctx, UpdateThreadInput{ID: threadA, WorkspaceID: workspaceA})
	if !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	if err = r.Delete(ctx, workspaceA, threadA); !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
}

func TestChildWorkspaceIsolation(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, "INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')", workspaceA, workspaceB)
	mustExec(t, ctx, pool, "INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'github')", integrationA, workspaceA)
	ir, _ := NewIntegrationRepository(pool)
	tr, _ := NewThreadRepository(pool)
	_, err := ir.Get(ctx, workspaceB, integrationA)
	if !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
	_, err = ir.Update(ctx, UpdateIntegrationInput{ID: integrationA, WorkspaceID: workspaceB})
	if !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
	if err = ir.Delete(ctx, workspaceB, integrationA); !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
	list, err := ir.List(ctx, workspaceB, 10, 0)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("isolation: %v %v", list, err)
	}
	_, err = ir.Create(ctx, CreateIntegrationInput{ID: integrationB, WorkspaceID: missingID, Platform: "github", Role: tenant.IntegrationRoleSource})
	requirePGCode(t, err, "23503")
	_, err = tr.Create(ctx, CreateThreadInput{ID: threadA, WorkspaceID: workspaceB, IntegrationID: integrationA, ThreadType: "pr", ExternalID: "42"})
	requirePGCode(t, err, "23503")
	_, err = tr.Create(ctx, CreateThreadInput{ID: threadA, WorkspaceID: workspaceA, IntegrationID: integrationA, ThreadType: "pr", ExternalID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Get(ctx, workspaceB, threadA)
	if !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	_, err = tr.Update(ctx, UpdateThreadInput{ID: threadA, WorkspaceID: workspaceB, Title: "wrong"})
	if !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	if err = tr.Delete(ctx, workspaceB, threadA); !errors.Is(err, relay.ErrThreadNotFound) {
		t.Fatal(err)
	}
	threads, err := tr.List(ctx, workspaceB, 10, 0)
	if err != nil || threads == nil || len(threads) != 0 {
		t.Fatalf("isolation: %v %v", threads, err)
	}
	got, err := tr.Get(ctx, workspaceA, threadA)
	if err != nil || got.Title != "" {
		t.Fatalf("other tenant mutated: %+v %v", got, err)
	}
}
