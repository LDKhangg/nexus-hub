package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerStore keeps worker devices, enrollment codes and the audit trail.
// Tokens and codes are stored only as SHA-256 hashes.
type WorkerStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	now  func() time.Time

	mu         sync.Mutex
	changed    chan struct{} // closed and replaced when jobs may be leasable
	thresholds behavior.WorkerThresholds
	observer   behavior.WorkerJobObserver
}

func NewWorkerStore(pool *pgxpool.Pool) (*WorkerStore, error) {
	if pool == nil {
		return nil, errors.New("worker store requires pool")
	}
	return &WorkerStore{
		pool: pool, q: sqlc.New(pool), now: time.Now,
		changed: make(chan struct{}), thresholds: behavior.DefaultWorkerThresholds,
	}, nil
}

var (
	_ behavior.WorkerAdmin         = (*WorkerStore)(nil)
	_ behavior.WorkerEnroller      = (*WorkerStore)(nil)
	_ behavior.WorkerAuthenticator = (*WorkerStore)(nil)
)

const (
	workerCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	workerCodeLen      = 12
	workerTokenPrefix  = "nhw_"
	maxWorkerCaps      = 32
)

var (
	workerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)
	workerCapRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

func hashSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// normalizeWorkerCode accepts the code as shown (XXXX-XXXX-XXXX), in any case.
func normalizeWorkerCode(code string) string {
	r := strings.NewReplacer("-", "", " ", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(code)))
}

func randomWorkerCode() (string, error) {
	buf := make([]byte, workerCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, workerCodeLen)
	for i, b := range buf {
		out[i] = workerCodeAlphabet[int(b)%len(workerCodeAlphabet)] // 256 % 32 == 0: unbiased
	}
	s := string(out)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12], nil
}

func randomWorkerToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return workerTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func mapWorkerDevice(row sqlc.WorkerDevice) behavior.WorkerDevice {
	var caps []string
	_ = json.Unmarshal(row.Capabilities, &caps)
	return behavior.WorkerDevice{
		ID: uuidString(row.ID), WorkspaceID: uuidString(row.WorkspaceID), Name: row.Name,
		Capabilities: caps, State: row.State, LastSeenAt: tsPtr(row.LastSeenAt),
		CreatedAt: row.CreatedAt.Time, RevokedAt: tsPtr(row.RevokedAt),
	}
}

func (s *WorkerStore) CreateEnrollCode(ctx context.Context, workspaceID, createdBy string) (string, time.Time, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return "", time.Time{}, invalid("workspace_id")
	}
	if len(createdBy) > 128 {
		createdBy = createdBy[:128]
	}
	code, err := randomWorkerCode()
	if err != nil {
		return "", time.Time{}, errors.New("worker code generation failed")
	}
	expires := s.now().Add(behavior.WorkerEnrollCodeTTL)
	err = s.q.CreateWorkerEnrollCode(ctx, sqlc.CreateWorkerEnrollCodeParams{
		WorkspaceID: ws, CodeHash: hashSecret(normalizeWorkerCode(code)), ExpiresAt: ts(expires), CreatedBy: createdBy,
	})
	if err != nil {
		return "", time.Time{}, topicError(err)
	}
	return code, expires, nil
}

func (s *WorkerStore) Enroll(ctx context.Context, code, name string, capabilities []string) (behavior.WorkerDevice, string, error) {
	name = strings.TrimSpace(name)
	if !workerNameRE.MatchString(name) {
		return behavior.WorkerDevice{}, "", invalid("device name")
	}
	if len(capabilities) > maxWorkerCaps {
		return behavior.WorkerDevice{}, "", invalid("capabilities")
	}
	for _, c := range capabilities {
		if !workerCapRE.MatchString(c) {
			return behavior.WorkerDevice{}, "", invalid("capabilities")
		}
	}
	if capabilities == nil {
		capabilities = []string{}
	}
	capJSON, _ := json.Marshal(capabilities)
	token, err := randomWorkerToken()
	if err != nil {
		return behavior.WorkerDevice{}, "", errors.New("worker token generation failed")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerDevice{}, "", err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	q := s.q.WithTx(tx)
	ws, err := q.RedeemWorkerEnrollCode(ctx, hashSecret(normalizeWorkerCode(code)))
	if err != nil {
		return behavior.WorkerDevice{}, "", notFound(err)
	}
	row, err := q.CreateWorkerDevice(ctx, sqlc.CreateWorkerDeviceParams{
		WorkspaceID: ws, Name: name, TokenHash: hashSecret(token), Capabilities: capJSON,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return behavior.WorkerDevice{}, "", behavior.ErrConflict
		}
		return behavior.WorkerDevice{}, "", topicError(err)
	}
	if err := q.InsertWorkerEvent(ctx, sqlc.InsertWorkerEventParams{
		WorkspaceID: ws, DeviceID: row.ID, Kind: "enrolled",
	}); err != nil {
		return behavior.WorkerDevice{}, "", topicError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerDevice{}, "", err
	}
	return mapWorkerDevice(row), token, nil
}

func (s *WorkerStore) Authenticate(ctx context.Context, token string) (behavior.WorkerDevice, error) {
	if token == "" || len(token) > 256 {
		return behavior.WorkerDevice{}, behavior.ErrNotFound
	}
	hash := hashSecret(token)
	row, err := s.q.AuthenticateWorkerDevice(ctx, hash)
	if err != nil {
		return behavior.WorkerDevice{}, notFound(err)
	}
	if subtle.ConstantTimeCompare(row.TokenHash, hash) != 1 || row.RevokedAt.Valid {
		return behavior.WorkerDevice{}, behavior.ErrNotFound
	}
	return mapWorkerDevice(row), nil
}

func (s *WorkerStore) ListDevices(ctx context.Context, workspaceID string) ([]behavior.WorkerDevice, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListWorkerDevices(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkerDevice, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapWorkerDevice(r))
	}
	return out, nil
}

func (s *WorkerStore) RevokeDevice(ctx context.Context, workspaceID, name string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return invalid("workspace_id")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return invalid("device name")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	q := s.q.WithTx(tx)
	row, err := q.RevokeWorkerDevice(ctx, sqlc.RevokeWorkerDeviceParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return notFound(err)
	}
	if err := q.InsertWorkerEvent(ctx, sqlc.InsertWorkerEventParams{
		WorkspaceID: ws, DeviceID: row.ID, JobID: pgtype.UUID{}, Kind: "revoked", ReasonCode: "owner_revoked",
	}); err != nil {
		return fmt.Errorf("worker audit: %w", err)
	}
	return tx.Commit(ctx)
}
