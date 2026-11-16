package storage

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	_ behavior.WorkerJobs        = (*WorkerStore)(nil)
	_ behavior.WorkerLeases      = (*WorkerStore)(nil)
	_ behavior.WorkerMaintenance = (*WorkerStore)(nil)
)

const (
	workerLeasePrefix     = "nhl_"
	maxWorkerTelemetryLen = 4096
	maxWorkerListLimit    = 200
)

// SetAssignmentThresholds sets the REQ-19 telemetry thresholds (settings).
func (s *WorkerStore) SetAssignmentThresholds(th behavior.WorkerThresholds) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thresholds = th
}

// SetObserver registers the job change observer (W-4). Nil disables it.
func (s *WorkerStore) SetObserver(o behavior.WorkerJobObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = o
}

// JobsChanged is closed when a job is created or cancelled.
func (s *WorkerStore) JobsChanged() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *WorkerStore) signal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *WorkerStore) notify(ctx context.Context, kind string, row sqlc.WorkerJob) {
	s.mu.Lock()
	o := s.observer
	s.mu.Unlock()
	if o != nil {
		o.WorkerJobChanged(ctx, kind, mapWorkerJob(row))
	}
}

func randomLeaseToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return workerLeasePrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func mapWorkerJob(row sqlc.WorkerJob) behavior.WorkerJob {
	var caps []string
	_ = json.Unmarshal(row.RequiredCapabilities, &caps)
	if caps == nil {
		caps = []string{}
	}
	job := behavior.WorkerJob{
		ID: uuidString(row.ID), WorkspaceID: uuidString(row.WorkspaceID), ExecutionID: row.ExecutionID.Int64,
		StepID: row.StepID, RequiredCapabilities: caps, State: row.State, DeviceID: uuidString(row.DeviceID),
		Attempt: int(row.Attempt), Payload: json.RawMessage(row.Payload),
		LeasedAt: tsPtr(row.LeasedAt), LeaseExpiresAt: tsPtr(row.LeaseExpiresAt), InterruptedAt: tsPtr(row.InterruptedAt),
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, FinishedAt: tsPtr(row.FinishedAt),
	}
	if len(row.Progress) > 0 {
		var p behavior.WorkerProgress
		if json.Unmarshal(row.Progress, &p) == nil {
			job.Progress = &p
		}
	}
	if len(row.Result) > 0 {
		var r behavior.WorkerResult
		if json.Unmarshal(row.Result, &r) == nil {
			job.Result = &r
		}
	}
	return job
}

// secretKeySuffixes mark payload keys that look like credentials. The payload
// must never carry a secret (Global Constraints), so such keys are refused.
var secretKeySuffixes = []string{
	"token", "secret", "password", "passwd", "authorization", "api_key", "apikey",
	"credential", "credentials", "private_key", "cookie", "access_key",
}

func payloadHasSecretKey(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			key := strings.ReplaceAll(strings.ToLower(k), "-", "_")
			for _, suf := range secretKeySuffixes {
				if strings.HasSuffix(key, suf) {
					return true
				}
			}
			if payloadHasSecretKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if payloadHasSecretKey(child) {
				return true
			}
		}
	}
	return false
}

func validateWorkerCaps(caps []string) ([]byte, error) {
	if len(caps) > maxWorkerCaps {
		return nil, invalid("capabilities")
	}
	for _, c := range caps {
		if !workerCapRE.MatchString(c) {
			return nil, invalid("capabilities")
		}
	}
	if caps == nil {
		caps = []string{}
	}
	b, _ := json.Marshal(caps)
	return b, nil
}

func (s *WorkerStore) rollback(tx pgx.Tx) {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(c)
}

func workerEvent(ws, device, job pgtype.UUID, kind, reason string) sqlc.InsertWorkerEventParams {
	return sqlc.InsertWorkerEventParams{WorkspaceID: ws, DeviceID: device, JobID: job, Kind: kind, ReasonCode: reason}
}

func clampLimit(limit int) int32 {
	if limit <= 0 {
		return 50
	}
	if limit > maxWorkerListLimit {
		return maxWorkerListLimit
	}
	return int32(limit)
}

// --- owner side ---

func (s *WorkerStore) CreateJob(ctx context.Context, workspaceID string, executionID int64, stepID string, requiredCaps []string, payload json.RawMessage) (behavior.WorkerJob, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.WorkerJob{}, invalid("workspace_id")
	}
	if len(stepID) > 128 || executionID < 0 {
		return behavior.WorkerJob{}, invalid("step")
	}
	capJSON, err := validateWorkerCaps(requiredCaps)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if len(payload) > behavior.WorkerMaxPayloadBytes {
		return behavior.WorkerJob{}, invalid("payload")
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return behavior.WorkerJob{}, invalid("payload")
	}
	if _, ok := decoded.(map[string]any); !ok || payloadHasSecretKey(decoded) {
		return behavior.WorkerJob{}, invalid("payload")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	row, err := q.CreateWorkerJob(ctx, sqlc.CreateWorkerJobParams{
		WorkspaceID: ws, ExecutionID: optInt8(executionID), StepID: stepID,
		RequiredCapabilities: capJSON, Payload: payload,
	})
	if err != nil {
		return behavior.WorkerJob{}, topicError(err)
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ws, pgtype.UUID{}, row.ID, behavior.WorkerEventJobCreated, "")); err != nil {
		return behavior.WorkerJob{}, topicError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, err
	}
	s.signal()
	return mapWorkerJob(row), nil
}

func (s *WorkerStore) CancelJob(ctx context.Context, workspaceID, jobID string) (behavior.WorkerJob, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.WorkerJob{}, invalid("workspace_id")
	}
	id, err := parseDBID(jobID)
	if err != nil {
		return behavior.WorkerJob{}, behavior.ErrNotFound
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	row, err := q.CancelWorkerJob(ctx, sqlc.CancelWorkerJobParams{Now: ts(now), WorkspaceID: ws, ID: id})
	if err != nil {
		if !isNoRows(err) {
			return behavior.WorkerJob{}, err
		}
		if _, gerr := q.GetWorkerJob(ctx, sqlc.GetWorkerJobParams{WorkspaceID: ws, ID: id}); gerr != nil {
			return behavior.WorkerJob{}, notFound(gerr)
		}
		return behavior.WorkerJob{}, behavior.ErrConflict
	}
	kind, change := behavior.WorkerEventCancelRequested, behavior.WorkerChangeCancelRequested
	if row.State == behavior.WorkerJobCancelled {
		kind, change = behavior.WorkerEventCancelled, behavior.WorkerChangeFinished
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ws, row.DeviceID, row.ID, kind, behavior.WorkerReasonOwnerCancel)); err != nil {
		return behavior.WorkerJob{}, err
	}
	if row.DeviceID.Valid {
		if err := s.refreshDevice(ctx, q, row.DeviceID, now); err != nil {
			return behavior.WorkerJob{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, err
	}
	s.signal()
	s.notify(ctx, change, row)
	return mapWorkerJob(row), nil
}

func (s *WorkerStore) GetJob(ctx context.Context, workspaceID, jobID string) (behavior.WorkerJob, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.WorkerJob{}, invalid("workspace_id")
	}
	id, err := parseDBID(jobID)
	if err != nil {
		return behavior.WorkerJob{}, behavior.ErrNotFound
	}
	row, err := s.q.GetWorkerJob(ctx, sqlc.GetWorkerJobParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.WorkerJob{}, notFound(err)
	}
	return mapWorkerJob(row), nil
}

func (s *WorkerStore) ListJobs(ctx context.Context, workspaceID string, limit int) ([]behavior.WorkerJob, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListWorkerJobs(ctx, sqlc.ListWorkerJobsParams{WorkspaceID: ws, RowLimit: clampLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkerJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapWorkerJob(r))
	}
	return out, nil
}

func (s *WorkerStore) ListEvents(ctx context.Context, workspaceID string, limit int) ([]behavior.WorkerEvent, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListWorkerEvents(ctx, sqlc.ListWorkerEventsParams{WorkspaceID: ws, RowLimit: clampLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkerEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.WorkerEvent{
			ID: r.ID, DeviceID: uuidString(r.DeviceID), JobID: uuidString(r.JobID),
			Kind: r.Kind, ReasonCode: r.ReasonCode, CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *WorkerStore) ListTelemetry(ctx context.Context, workspaceID string) ([]behavior.WorkerTelemetryRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListWorkerTelemetry(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkerTelemetryRecord, 0, len(rows))
	for _, r := range rows {
		var t behavior.WorkerTelemetry
		if json.Unmarshal(r.Snapshot, &t) != nil {
			continue
		}
		out = append(out, behavior.WorkerTelemetryRecord{DeviceID: uuidString(r.DeviceID), Snapshot: t, ReceivedAt: r.ReceivedAt.Time})
	}
	return out, nil
}

// --- device side ---

type workerDeviceIDs struct{ ws, dev pgtype.UUID }

func deviceIDs(d behavior.WorkerDevice) (workerDeviceIDs, error) {
	ws, err := parseDBID(d.WorkspaceID)
	if err != nil {
		return workerDeviceIDs{}, invalid("device")
	}
	dev, err := parseDBID(d.ID)
	if err != nil {
		return workerDeviceIDs{}, invalid("device")
	}
	return workerDeviceIDs{ws: ws, dev: dev}, nil
}

func (s *WorkerStore) refreshDevice(ctx context.Context, q *sqlc.Queries, dev pgtype.UUID, now time.Time) error {
	return q.RefreshWorkerDeviceState(ctx, sqlc.RefreshWorkerDeviceStateParams{
		OnlineAfter: ts(now.Add(-behavior.WorkerOnlineWindow)), DeviceID: dev,
	})
}

func (s *WorkerStore) LeaseJob(ctx context.Context, device behavior.WorkerDevice) (behavior.WorkerLease, bool, error) {
	ids, err := deviceIDs(device)
	if err != nil {
		return behavior.WorkerLease{}, false, err
	}
	now := s.now()
	s.mu.Lock()
	th := s.thresholds
	s.mu.Unlock()
	if tel, err := s.q.GetWorkerTelemetry(ctx, ids.dev); err == nil {
		var snap behavior.WorkerTelemetry
		if json.Unmarshal(tel.Snapshot, &snap) == nil && th.Block(&snap, now) != nil {
			return behavior.WorkerLease{}, false, nil
		}
	} else if !isNoRows(err) {
		return behavior.WorkerLease{}, false, err
	}
	capJSON, err := validateWorkerCaps(device.Capabilities)
	if err != nil {
		return behavior.WorkerLease{}, false, err
	}
	token, err := randomLeaseToken()
	if err != nil {
		return behavior.WorkerLease{}, false, errors.New("lease token generation failed")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerLease{}, false, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	row, err := q.LeaseWorkerJob(ctx, sqlc.LeaseWorkerJobParams{
		DeviceID: ids.dev, LeaseTokenHash: hashSecret(token), LeaseExpiresAt: ts(now.Add(behavior.WorkerLeaseDuration)),
		Now: ts(now), WorkspaceID: ids.ws, Capabilities: capJSON,
	})
	if err != nil {
		if isNoRows(err) || isUniqueViolation(err) {
			return behavior.WorkerLease{}, false, nil
		}
		return behavior.WorkerLease{}, false, err
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ids.ws, ids.dev, row.ID, behavior.WorkerEventLeased, "")); err != nil {
		return behavior.WorkerLease{}, false, err
	}
	if err := s.refreshDevice(ctx, q, ids.dev, now); err != nil {
		return behavior.WorkerLease{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerLease{}, false, err
	}
	s.notify(ctx, behavior.WorkerChangeLeased, row)
	return behavior.WorkerLease{Job: mapWorkerJob(row), LeaseToken: token}, true, nil
}

// heldJob is a job row locked for a device request.
type heldJob struct {
	row     sqlc.WorkerJob
	resumed bool // INTERRUPTED within the resume window
}

// workerReject is a refused device request: the event is recorded, then err
// is returned.
type workerReject struct {
	reason string
	err    error
}

// lockForDevice locks the job and checks that device holds its lease with
// token. finished jobs are returned with reject job_finished so the result
// path can apply idempotency first.
func (s *WorkerStore) lockForDevice(ctx context.Context, q *sqlc.Queries, ids workerDeviceIDs, jobID, token string, now time.Time) (heldJob, *workerReject, error) {
	id, err := parseDBID(jobID)
	if err != nil {
		return heldJob{}, nil, behavior.ErrNotFound
	}
	row, err := q.LockWorkerJob(ctx, sqlc.LockWorkerJobParams{WorkspaceID: ids.ws, ID: id})
	if err != nil {
		return heldJob{}, nil, notFound(err)
	}
	if !row.DeviceID.Valid || row.DeviceID.Bytes != ids.dev.Bytes {
		return heldJob{row: row}, &workerReject{reason: behavior.WorkerReasonForeignJob, err: behavior.ErrNotFound}, nil
	}
	if token == "" || len(token) > 256 || subtle.ConstantTimeCompare(row.LeaseTokenHash, hashSecret(token)) != 1 {
		return heldJob{row: row}, &workerReject{reason: behavior.WorkerReasonStaleLease, err: behavior.ErrConflict}, nil
	}
	switch {
	case behavior.WorkerJobActive(row.State):
		return heldJob{row: row}, nil, nil
	case row.State == behavior.WorkerJobInterrupted:
		if row.InterruptedAt.Valid && now.Sub(row.InterruptedAt.Time) <= behavior.WorkerResumeWindow {
			return heldJob{row: row, resumed: true}, nil, nil
		}
		return heldJob{row: row}, &workerReject{reason: behavior.WorkerReasonLateResume, err: behavior.ErrConflict}, nil
	case behavior.WorkerJobFinished(row.State):
		return heldJob{row: row}, &workerReject{reason: behavior.WorkerReasonFinished, err: behavior.ErrConflict}, nil
	}
	return heldJob{row: row}, &workerReject{reason: behavior.WorkerReasonStaleLease, err: behavior.ErrConflict}, nil
}

// rejectDevice records the refusal and commits it.
func (s *WorkerStore) rejectDevice(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, ids workerDeviceIDs, job pgtype.UUID, rej *workerReject) error {
	if err := q.InsertWorkerEvent(ctx, workerEvent(ids.ws, ids.dev, job, behavior.WorkerEventRejected, rej.reason)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return rej.err
}

// resumeCheck refuses a resume while the device holds another active job.
func (s *WorkerStore) resumeCheck(ctx context.Context, q *sqlc.Queries, ids workerDeviceIDs, h heldJob) (*workerReject, error) {
	if !h.resumed {
		return nil, nil
	}
	busy, err := q.DeviceHasOtherActiveWorkerJob(ctx, sqlc.DeviceHasOtherActiveWorkerJobParams{DeviceID: ids.dev, JobID: h.row.ID})
	if err != nil {
		return nil, err
	}
	if busy {
		return &workerReject{reason: behavior.WorkerReasonDeviceBusy, err: behavior.ErrConflict}, nil
	}
	return nil, nil
}

func liveState(state string) string {
	if state == behavior.WorkerJobLeased || state == behavior.WorkerJobInterrupted {
		return behavior.WorkerJobRunning
	}
	return state
}

func (s *WorkerStore) Heartbeat(ctx context.Context, device behavior.WorkerDevice, jobID, leaseToken string) (behavior.WorkerJob, error) {
	ids, err := deviceIDs(device)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	h, rej, err := s.lockForDevice(ctx, q, ids, jobID, leaseToken, now)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	if rej == nil {
		if rej, err = s.resumeCheck(ctx, q, ids, h); err != nil {
			return behavior.WorkerJob{}, err
		}
	}
	if rej != nil {
		return behavior.WorkerJob{}, s.rejectDevice(ctx, tx, q, ids, h.row.ID, rej)
	}
	row, err := q.UpdateWorkerJobLease(ctx, sqlc.UpdateWorkerJobLeaseParams{
		LeaseExpiresAt: ts(now.Add(behavior.WorkerLeaseDuration)), State: liveState(h.row.State), Now: ts(now), ID: h.row.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return behavior.WorkerJob{}, behavior.ErrConflict
		}
		return behavior.WorkerJob{}, err
	}
	if err := s.afterResume(ctx, q, ids, h, now); err != nil {
		return behavior.WorkerJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, err
	}
	if h.resumed {
		s.notify(ctx, behavior.WorkerChangeResumed, row)
	}
	return mapWorkerJob(row), nil
}

func (s *WorkerStore) afterResume(ctx context.Context, q *sqlc.Queries, ids workerDeviceIDs, h heldJob, now time.Time) error {
	if !h.resumed {
		return nil
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ids.ws, ids.dev, h.row.ID, behavior.WorkerEventResumed, "")); err != nil {
		return err
	}
	return s.refreshDevice(ctx, q, ids.dev, now)
}

func (s *WorkerStore) ReportProgress(ctx context.Context, device behavior.WorkerDevice, jobID, leaseToken string, p behavior.WorkerProgress) (behavior.WorkerJob, bool, error) {
	if err := p.Validate(); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	ids, err := deviceIDs(device)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	h, rej, err := s.lockForDevice(ctx, q, ids, jobID, leaseToken, now)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if rej == nil && p.Seq <= h.row.ProgressSeq && !h.resumed {
		return mapWorkerJob(h.row), true, tx.Commit(ctx)
	}
	if rej == nil {
		if rej, err = s.resumeCheck(ctx, q, ids, h); err != nil {
			return behavior.WorkerJob{}, false, err
		}
	}
	if rej != nil {
		return behavior.WorkerJob{}, false, s.rejectDevice(ctx, tx, q, ids, h.row.ID, rej)
	}
	duplicate := p.Seq <= h.row.ProgressSeq
	progress, seq := h.row.Progress, h.row.ProgressSeq
	if !duplicate {
		p.At = now.UTC()
		progress, _ = json.Marshal(p)
		seq = p.Seq
	}
	row, err := q.UpdateWorkerJobProgress(ctx, sqlc.UpdateWorkerJobProgressParams{
		Progress: progress, ProgressSeq: seq, LeaseExpiresAt: ts(now.Add(behavior.WorkerLeaseDuration)),
		State: liveState(h.row.State), Now: ts(now), ID: h.row.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return behavior.WorkerJob{}, false, behavior.ErrConflict
		}
		return behavior.WorkerJob{}, false, err
	}
	if err := s.afterResume(ctx, q, ids, h, now); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if h.resumed {
		s.notify(ctx, behavior.WorkerChangeResumed, row)
	}
	if !duplicate {
		s.notify(ctx, behavior.WorkerChangeProgress, row)
	}
	return mapWorkerJob(row), duplicate, nil
}

func (s *WorkerStore) ReportResult(ctx context.Context, device behavior.WorkerDevice, jobID, leaseToken, idempotencyKey string, r behavior.WorkerResult) (behavior.WorkerJob, bool, error) {
	if !behavior.ValidWorkerIdempotencyKey(idempotencyKey) {
		return behavior.WorkerJob{}, false, invalid("idempotency_key")
	}
	if err := r.Validate(); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	state, _ := behavior.WorkerResultState(r.Status)
	if r.ArtifactRefs == nil {
		r.ArtifactRefs = []string{}
	}
	resultJSON, err := json.Marshal(r)
	if err != nil {
		return behavior.WorkerJob{}, false, invalid("result")
	}
	ids, err := deviceIDs(device)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	h, rej, err := s.lockForDevice(ctx, q, ids, jobID, leaseToken, now)
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if rej != nil && rej.reason == behavior.WorkerReasonFinished {
		// Same device, same lease token: a retried upload with the first key
		// returns the first result; anything else is a conflict.
		if h.row.IdempotencyKey.Valid && subtle.ConstantTimeCompare([]byte(h.row.IdempotencyKey.String), []byte(idempotencyKey)) == 1 {
			return mapWorkerJob(h.row), true, tx.Commit(ctx)
		}
		rej.reason = behavior.WorkerReasonResultClash
	}
	if rej != nil {
		return behavior.WorkerJob{}, false, s.rejectDevice(ctx, tx, q, ids, h.row.ID, rej)
	}
	row, err := q.FinishWorkerJob(ctx, sqlc.FinishWorkerJobParams{
		State: state, Result: resultJSON, IdempotencyKey: pgtype.Text{String: idempotencyKey, Valid: true},
		Now: ts(now), ID: h.row.ID,
	})
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if h.resumed {
		if err := q.InsertWorkerEvent(ctx, workerEvent(ids.ws, ids.dev, row.ID, behavior.WorkerEventResumed, "")); err != nil {
			return behavior.WorkerJob{}, false, err
		}
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ids.ws, ids.dev, row.ID, behavior.WorkerEventResultPrefix+r.Status, r.ReasonCode)); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if err := s.refreshDevice(ctx, q, ids.dev, now); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, false, err
	}
	s.notify(ctx, behavior.WorkerChangeFinished, row)
	return mapWorkerJob(row), false, nil
}

func (s *WorkerStore) RecordTelemetry(ctx context.Context, device behavior.WorkerDevice, t behavior.WorkerTelemetry) error {
	if err := t.Validate(); err != nil {
		return err
	}
	ids, err := deviceIDs(device)
	if err != nil {
		return err
	}
	snap, err := json.Marshal(t)
	if err != nil || len(snap) > maxWorkerTelemetryLen {
		return invalid("telemetry")
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	if err := q.UpsertWorkerTelemetry(ctx, sqlc.UpsertWorkerTelemetryParams{
		DeviceID: ids.dev, WorkspaceID: ids.ws, Snapshot: snap, ReceivedAt: ts(now),
	}); err != nil {
		return topicError(err)
	}
	p := sqlc.UpsertWorkerTelemetryHourlyParams{
		DeviceID: ids.dev, Hour: ts(now.UTC().Truncate(time.Hour)), WorkspaceID: ids.ws,
		JobsTotal:           pgtype.Int8{Int64: t.Counters.Jobs, Valid: true},
		FailuresTotal:       pgtype.Int8{Int64: t.Counters.Failures, Valid: true},
		ExecutorTokensTotal: pgtype.Int8{Int64: t.Counters.ExecutorTokens, Valid: true},
		BusySecondsTotal:    pgtype.Int8{Int64: t.Counters.BusySeconds, Valid: true},
	}
	if t.CPULoad != nil {
		p.CpuLoad = pgtype.Float8{Float64: *t.CPULoad, Valid: true}
	}
	if pct := t.RAMUsedPct(); pct != nil {
		p.RamUsedPct = pgtype.Float8{Float64: *pct, Valid: true}
	}
	if free := t.DiskFreeBytes(); free != nil {
		p.DiskFreeBytes = pgtype.Int8{Int64: *free, Valid: true}
	}
	if t.ClaudeSessionLeftPct != nil {
		p.ClaudeSessionLeft = pgtype.Int2{Int16: int16(*t.ClaudeSessionLeftPct), Valid: true}
	}
	if t.ClaudeWeeklyLeftPct != nil {
		p.ClaudeWeeklyLeft = pgtype.Int2{Int16: int16(*t.ClaudeWeeklyLeftPct), Valid: true}
	}
	if err := q.UpsertWorkerTelemetryHourly(ctx, p); err != nil {
		return topicError(err)
	}
	return tx.Commit(ctx)
}

// --- maintenance ---

// ExpireLeases interrupts jobs whose lease ran out (event lease_lost); a job
// that was cancelling is cancelled.
func (s *WorkerStore) ExpireLeases(ctx context.Context) (int, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	rows, err := q.ExpireWorkerLeases(ctx, ts(now))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		kind, reason := behavior.WorkerEventLeaseLost, behavior.WorkerReasonLeaseExpired
		if r.State == behavior.WorkerJobCancelled {
			kind = behavior.WorkerEventCancelled
		}
		if err := q.InsertWorkerEvent(ctx, workerEvent(r.WorkspaceID, r.DeviceID, r.ID, kind, reason)); err != nil {
			return 0, err
		}
		if err := s.refreshDevice(ctx, q, r.DeviceID, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, r := range rows {
		change := behavior.WorkerChangeInterrupted
		if r.State == behavior.WorkerJobCancelled {
			change = behavior.WorkerChangeFinished
		}
		s.notify(ctx, change, r)
	}
	return len(rows), nil
}

// MarkDevicesOffline marks devices without contact for 90 s offline.
func (s *WorkerStore) MarkDevicesOffline(ctx context.Context) (int, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	rows, err := q.MarkWorkerDevicesOffline(ctx, ts(now.Add(-behavior.WorkerOnlineWindow)))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := q.InsertWorkerEvent(ctx, workerEvent(r.WorkspaceID, r.ID, pgtype.UUID{}, behavior.WorkerEventOffline, behavior.WorkerReasonNoContact)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// PruneTelemetry deletes hourly rollups older than 30 days.
func (s *WorkerStore) PruneTelemetry(ctx context.Context) (int64, error) {
	return s.q.PruneWorkerTelemetryHourly(ctx, ts(s.now().Add(-behavior.WorkerTelemetryRetain)))
}
