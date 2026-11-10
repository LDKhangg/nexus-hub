package storage

import (
	"errors"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"reflect"
	"testing"
)

func TestIntegrationCRUD(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, "INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')", workspaceA, workspaceB)
	r, err := NewIntegrationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Create(ctx, CreateIntegrationInput{ID: integrationA, WorkspaceID: workspaceA, Platform: "future-platform", Role: tenant.IntegrationRoleSource, Config: map[string]any{"channel": "dev"}, IsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Platform != "future-platform" || first.Config["channel"] != "dev" || !first.IsEnabled || first.CreatedAt.IsZero() {
		t.Fatalf("create: %+v", first)
	}
	got, err := r.Get(ctx, workspaceA, integrationA)
	if err != nil || !reflect.DeepEqual(first, got) {
		t.Fatalf("get: %+v %v", got, err)
	}
	_, err = r.Create(ctx, CreateIntegrationInput{ID: integrationB, WorkspaceID: workspaceA, Platform: "clickup", Role: tenant.IntegrationRoleSource})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, "UPDATE integrations SET created_at='2020-01-01', updated_at='2020-01-01'")
	page, err := r.List(ctx, workspaceA, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != integrationA {
		t.Fatalf("page: %+v %v", page, err)
	}
	before, err := r.Get(ctx, workspaceA, integrationA)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := r.Update(ctx, UpdateIntegrationInput{ID: integrationA, WorkspaceID: workspaceA, Role: tenant.IntegrationRoleBoth, Config: nil, IsEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Platform != first.Platform || updated.WorkspaceID != workspaceA || updated.Role != tenant.IntegrationRoleBoth || updated.IsEnabled || len(updated.Config) != 0 || !updated.CreatedAt.Equal(before.CreatedAt) || !updated.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("update: %+v", updated)
	}
	if err = r.Delete(ctx, workspaceA, integrationA); err != nil {
		t.Fatal(err)
	}
	_, err = r.Get(ctx, workspaceA, integrationA)
	if !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
	_, err = r.Update(ctx, UpdateIntegrationInput{ID: integrationA, WorkspaceID: workspaceA})
	if !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
	if err = r.Delete(ctx, workspaceA, integrationA); !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatal(err)
	}
}

func TestIntegrationCredentialsPreserved(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, "INSERT INTO workspaces(id,display_name) VALUES($1,'A')", workspaceA)
	mustExec(t, ctx, pool, "INSERT INTO integrations(id,workspace_id,platform,encrypted_credentials) VALUES($1,$2,'github','test-ciphertext')", integrationA, workspaceA)
	r, _ := NewIntegrationRepository(pool)
	got, err := r.Update(ctx, UpdateIntegrationInput{ID: integrationA, WorkspaceID: workspaceA, Role: tenant.IntegrationRoleSource, IsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config == nil {
		t.Fatal("nil JSON object")
	}
	if _, ok := reflect.TypeOf(got).FieldByName("EncryptedCredentials"); ok {
		t.Fatal("credentials exposed")
	}
	var ciphertext string
	if err := pool.QueryRow(ctx, "SELECT encrypted_credentials FROM integrations WHERE id=$1", integrationA).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != "test-ciphertext" {
		t.Fatal("credentials overwritten")
	}
}
