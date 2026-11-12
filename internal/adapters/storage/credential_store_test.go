package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
)

func newCredDB(t *testing.T) (context.Context, *CredentialStore) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	s, err := NewCredentialStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, s
}

func TestCredentialStore_RoundTrip(t *testing.T) {
	ctx, s := newCredDB(t)
	var _ secrets.Store = s
	nonce := bytes.Repeat([]byte{1}, 12)
	if err := s.Put(ctx, workspaceA, "gh", "github_pat", nonce, []byte("ct1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, workspaceA, "gh", "github_pat", nonce, []byte("ct2")); err != nil {
		t.Fatal(err)
	}
	kind, n, ct, err := s.Get(ctx, workspaceA, "gh")
	if err != nil || kind != "github_pat" || !bytes.Equal(n, nonce) || string(ct) != "ct2" {
		t.Fatalf("%s %x %s %v", kind, n, ct, err)
	}
	list, err := s.List(ctx, workspaceA)
	if err != nil || len(list) != 1 || list[0].Name != "gh" || list[0].Kind != "github_pat" || list[0].UpdatedAt.IsZero() {
		t.Fatalf("%+v %v", list, err)
	}
	if err := s.Delete(ctx, workspaceA, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Get(ctx, workspaceA, "gh"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("after delete %v", err)
	}
	if err := s.Delete(ctx, workspaceA, "gh"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("double delete %v", err)
	}
}

func TestCredentialStore_WorkspaceScoped(t *testing.T) {
	ctx, s := newCredDB(t)
	if err := s.Put(ctx, workspaceA, "gh", "github_pat", make([]byte, 12), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Get(ctx, workspaceB, "gh"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("cross-workspace read %v", err)
	}
	if list, _ := s.List(ctx, workspaceB); len(list) != 0 {
		t.Fatal("cross-workspace list")
	}
	if err := s.Delete(ctx, workspaceB, "gh"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal("cross-workspace delete")
	}
	// End to end with the resolver: encrypted at rest, decrypts in A only.
	key := bytes.Repeat([]byte{7}, 32)
	r := secrets.NewResolver(s, key, nil, nil, time.Minute, time.Now)
	if err := r.SetSecret(ctx, workspaceA, "tg", "telegram_bot", "123456:plaintext-token"); err != nil {
		t.Fatal(err)
	}
	_, _, ct, _ := s.Get(ctx, workspaceA, "tg")
	if bytes.Contains(ct, []byte("plaintext-token")) {
		t.Fatal("plaintext at rest")
	}
	if v, err := r.Resolve(ctx, workspaceA, "tg"); err != nil || v != "123456:plaintext-token" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := r.Resolve(ctx, workspaceB, "tg"); err == nil {
		t.Fatal("resolved in other workspace")
	}
}

func TestSettingsStore_RoundTrip(t *testing.T) {
	ctx, pool := testDB(t)
	s, err := NewSettingsStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(ctx, "allow_dangerous"); err != nil || ok {
		t.Fatalf("unset %v %v", ok, err)
	}
	if err := s.Set(ctx, "allow_dangerous", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "allow_dangerous", json.RawMessage(`"maybe"`)); err == nil {
		t.Fatal("invalid value stored")
	}
	if err := s.Set(ctx, "nope", json.RawMessage(`1`)); err == nil {
		t.Fatal("unknown key stored")
	}
	if v, ok, err := s.Get(ctx, "allow_dangerous"); err != nil || !ok || string(v) != "true" {
		t.Fatalf("%s %v %v", v, ok, err)
	}
	all, err := s.All(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("%v %v", all, err)
	}
	loaded, err := settings.Load(ctx, s)
	if err != nil || !loaded.AllowDangerous {
		t.Fatalf("%+v %v", loaded, err)
	}
	if list, err := s.List(ctx); err != nil || len(list) != 1 || list[0].Key != "allow_dangerous" {
		t.Fatalf("%+v %v", list, err)
	}
}
