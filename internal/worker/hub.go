package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Hub errors. Every error from a Hub is one of these (possibly wrapped);
// none carries a body, token or URL.
var (
	// ErrRetry: network failure, timeout, 429 or 5xx; try again later.
	ErrRetry = errors.New("hub: temporarily unavailable")
	// ErrConflict: 409, the lease is stale or the job finished.
	ErrConflict = errors.New("hub: lease conflict")
	// ErrNotFound: 404, unknown job or not this device's job.
	ErrNotFound = errors.New("hub: not found")
	// ErrUnauthorized: 401, the device token was rejected (revoked?).
	ErrUnauthorized = errors.New("hub: device token rejected")
	// ErrRejected: 400 or another 4xx; resending the same request will
	// never succeed.
	ErrRejected = errors.New("hub: request rejected")
)

// Lease is one job handed to this device by a poll.
type Lease struct {
	JobID                    string          `json:"job_id"`
	ExecutionID              int64           `json:"execution_id,omitempty"`
	StepID                   string          `json:"step_id,omitempty"`
	Attempt                  int             `json:"attempt"`
	RequiredCapabilities     []string        `json:"required_capabilities"`
	Payload                  json.RawMessage `json:"payload"`
	LeaseToken               string          `json:"lease_token"`
	LeaseExpiresAt           time.Time       `json:"lease_expires_at"`
	LeaseSeconds             int             `json:"lease_seconds"`
	HeartbeatIntervalSeconds int             `json:"heartbeat_interval_seconds"`
}

// HeartbeatReply is the hub's answer to a heartbeat.
type HeartbeatReply struct {
	State                    string                       `json:"state"`
	Cancel                   bool                         `json:"cancel"`
	HeartbeatIntervalSeconds int                          `json:"heartbeat_interval_seconds"`
	Instructions             []behavior.WorkerInstruction `json:"instructions"`
}

// Hub is the worker's view of the hub lease protocol. Tests replace it
// with a fake or point HTTPHub at an httptest server.
type Hub interface {
	// Poll long-polls up to wait for a job; nil lease means none.
	Poll(ctx context.Context, wait time.Duration, t *behavior.WorkerTelemetry) (*Lease, error)
	Heartbeat(ctx context.Context, jobID, leaseToken string, t *behavior.WorkerTelemetry) (HeartbeatReply, error)
	Progress(ctx context.Context, jobID, leaseToken string, p behavior.WorkerProgress) error
	Result(ctx context.Context, jobID, leaseToken, idempotencyKey string, r behavior.WorkerResult) error
}

// InstructionAcker is a Hub whose heartbeat also acknowledges the owner
// instructions the worker journaled; the hub resends an instruction until
// it is acknowledged (R-B-4). HTTPHub implements it.
type InstructionAcker interface {
	HeartbeatAck(ctx context.Context, jobID, leaseToken string, t *behavior.WorkerTelemetry, ackIDs []string) (HeartbeatReply, error)
}

// MaxAckInstructionIDs bounds ack_instruction_ids in one heartbeat.
const MaxAckInstructionIDs = 64

// HTTPHub talks to /worker/v1 with the device token.
type HTTPHub struct {
	base   string
	token  string
	client *http.Client
}

// NewHTTPHub returns a client for base (already validated). Redirects are
// never followed, so the token never goes to another host.
func NewHTTPHub(base, token string, client *http.Client) *HTTPHub {
	if client == nil {
		client = &http.Client{}
	}
	cp := *client
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cp.Timeout = 0 // per-request contexts bound each call
	return &HTTPHub{base: base, token: token, client: &cp}
}

const (
	hubCallTimeout  = 30 * time.Second
	pollGrace       = 20 * time.Second
	maxHubReplySize = 64 << 10
)

func (h *HTTPHub) do(ctx context.Context, path string, body any, timeout time.Duration, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, ErrRejected
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, ErrRejected
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, ErrRetry
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHubReplySize))
	if err != nil {
		return resp.StatusCode, ErrRetry
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if out != nil && json.Unmarshal(data, out) != nil {
			return resp.StatusCode, ErrRetry
		}
		return resp.StatusCode, nil
	case resp.StatusCode == http.StatusNoContent:
		return resp.StatusCode, nil
	}
	return resp.StatusCode, statusErr(resp.StatusCode)
}

func statusErr(code int) error {
	switch {
	case code == http.StatusConflict:
		return ErrConflict
	case code == http.StatusNotFound:
		return ErrNotFound
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500:
		return ErrRetry
	}
	return fmt.Errorf("%w (HTTP %d)", ErrRejected, code)
}

func jobPath(jobID, op string) string {
	return "/worker/v1/jobs/" + url.PathEscape(jobID) + "/" + op
}

// Poll implements Hub.
func (h *HTTPHub) Poll(ctx context.Context, wait time.Duration, t *behavior.WorkerTelemetry) (*Lease, error) {
	secs := int(wait / time.Second)
	body := map[string]any{"wait_seconds": secs}
	if t != nil {
		body["telemetry"] = t
	}
	var l Lease
	code, err := h.do(ctx, "/worker/v1/poll", body, wait+pollGrace, &l)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	if l.JobID == "" || l.LeaseToken == "" {
		return nil, ErrRetry
	}
	return &l, nil
}

// Heartbeat implements Hub.
func (h *HTTPHub) Heartbeat(ctx context.Context, jobID, leaseToken string, t *behavior.WorkerTelemetry) (HeartbeatReply, error) {
	return h.HeartbeatAck(ctx, jobID, leaseToken, t, nil)
}

// HeartbeatAck implements InstructionAcker.
func (h *HTTPHub) HeartbeatAck(ctx context.Context, jobID, leaseToken string, t *behavior.WorkerTelemetry, ackIDs []string) (HeartbeatReply, error) {
	body := map[string]any{"lease_token": leaseToken}
	if t != nil {
		body["telemetry"] = t
	}
	if len(ackIDs) > 0 {
		if len(ackIDs) > MaxAckInstructionIDs {
			ackIDs = ackIDs[len(ackIDs)-MaxAckInstructionIDs:]
		}
		body["ack_instruction_ids"] = ackIDs
	}
	var r HeartbeatReply
	_, err := h.do(ctx, jobPath(jobID, "heartbeat"), body, hubCallTimeout, &r)
	return r, err
}

// Progress implements Hub.
func (h *HTTPHub) Progress(ctx context.Context, jobID, leaseToken string, p behavior.WorkerProgress) error {
	body := map[string]any{"lease_token": leaseToken, "seq": p.Seq, "phase": p.Phase, "counts": p.Counts}
	_, err := h.do(ctx, jobPath(jobID, "progress"), body, hubCallTimeout, nil)
	return err
}

// Result implements Hub.
func (h *HTTPHub) Result(ctx context.Context, jobID, leaseToken, idempotencyKey string, r behavior.WorkerResult) error {
	if r.ArtifactRefs == nil {
		r.ArtifactRefs = []string{}
	}
	body := struct {
		LeaseToken     string `json:"lease_token"`
		IdempotencyKey string `json:"idempotency_key"`
		behavior.WorkerResult
	}{leaseToken, idempotencyKey, r}
	_, err := h.do(ctx, jobPath(jobID, "result"), body, hubCallTimeout, nil)
	return err
}

// idempotencyKey is the result key of one job attempt.
func idempotencyKey(jobID string, attempt int) string {
	return jobID + ":" + strconv.Itoa(attempt) + ":result"
}
