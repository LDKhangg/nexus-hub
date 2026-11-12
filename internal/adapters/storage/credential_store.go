package storage

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialStore persists encrypted credentials; it never sees plaintext.
type CredentialStore struct{ queries *sqlc.Queries }

func NewCredentialStore(pool *pgxpool.Pool) (*CredentialStore, error) {
	if pool == nil {
		return nil, errors.New("credential store requires pool")
	}
	return &CredentialStore{queries: sqlc.New(pool)}, nil
}

var _ secrets.Store = (*CredentialStore)(nil)

func (s *CredentialStore) Put(ctx context.Context, workspaceID, name, kind string, nonce, ct []byte) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	dbID, err := parseDBID(id.String())
	if err != nil {
		return err
	}
	return s.queries.PutCredential(ctx, sqlc.PutCredentialParams{ID: dbID, WorkspaceID: ws, Name: name, Kind: kind, Ciphertext: ct, Nonce: nonce})
}

func (s *CredentialStore) Get(ctx context.Context, workspaceID, name string) (string, []byte, []byte, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return "", nil, nil, secrets.ErrNotFound
	}
	row, err := s.queries.GetCredential(ctx, sqlc.GetCredentialParams{WorkspaceID: ws, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil, secrets.ErrNotFound
	}
	if err != nil {
		return "", nil, nil, err
	}
	return row.Kind, row.Nonce, row.Ciphertext, nil
}

func (s *CredentialStore) List(ctx context.Context, workspaceID string) ([]secrets.Meta, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListCredentials(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]secrets.Meta, 0, len(rows))
	for _, r := range rows {
		out = append(out, secrets.Meta{Name: r.Name, Kind: r.Kind, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

func (s *CredentialStore) Delete(ctx context.Context, workspaceID, name string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return secrets.ErrNotFound
	}
	n, err := s.queries.DeleteCredential(ctx, sqlc.DeleteCredentialParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return err
	}
	if n == 0 {
		return secrets.ErrNotFound
	}
	return nil
}
