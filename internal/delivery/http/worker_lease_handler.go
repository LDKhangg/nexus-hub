package http

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/labstack/echo/v4"
)

// Lease protocol endpoints (REQ-2, 3, 7, 9, 18, 19). Bodies are never logged
// or echoed; errors are fixed strings.

type pollRequest struct {
	WaitSeconds *int                      `json:"wait_seconds"`
	Telemetry   *behavior.WorkerTelemetry `json:"telemetry"`
}

type pollResponse struct {
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

type heartbeatRequest struct {
	LeaseToken string                    `json:"lease_token"`
	Telemetry  *behavior.WorkerTelemetry `json:"telemetry"`
}

type heartbeatResponse struct {
	State                    string                       `json:"state"`
	Cancel                   bool                         `json:"cancel"`
	LeaseExpiresAt           *time.Time                   `json:"lease_expires_at"`
	HeartbeatIntervalSeconds int                          `json:"heartbeat_interval_seconds"`
	Instructions             []behavior.WorkerInstruction `json:"instructions"`
}

type progressRequest struct {
	LeaseToken string                        `json:"lease_token"`
	Seq        int64                         `json:"seq"`
	Phase      string                        `json:"phase"`
	Counts     behavior.WorkerProgressCounts `json:"counts"`
}

type progressResponse struct {
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate"`
	Cancel    bool   `json:"cancel"`
}

type resultRequest struct {
	LeaseToken     string `json:"lease_token"`
	IdempotencyKey string `json:"idempotency_key"`
	behavior.WorkerResult
}

type resultResponse struct {
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate"`
}

func workerStoreError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, behavior.ErrNotFound):
		return workerError(c, http.StatusNotFound, "not found")
	case errors.Is(err, behavior.ErrConflict):
		return workerError(c, http.StatusConflict, "lease conflict")
	case errors.Is(err, behavior.ErrInvalid):
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	return workerError(c, http.StatusInternalServerError, "internal error")
}

// recordTelemetry stores a validated snapshot; a storage failure does not
// fail the request.
func (h *WorkerHandler) recordTelemetry(ctx context.Context, device behavior.WorkerDevice, t *behavior.WorkerTelemetry) {
	if t == nil {
		return
	}
	if err := h.leases.RecordTelemetry(ctx, device, *t); err != nil && ctx.Err() == nil {
		log.Print("worker: telemetry not stored")
	}
}

// Poll long-polls (≤ 55 s) for one job. 204 means no job; the worker polls
// again. A client disconnect ends the wait. A device that polls while it
// holds a LEASED job (never heartbeated or reported) gets that same job back
// with a new lease token, same attempt: its earlier response was lost. Only
// a poll that started after that lease was issued reissues it. A device
// holding a RUNNING or CANCELLING job gets 204.
func (h *WorkerHandler) Poll(c echo.Context) error {
	device, ok := WorkerDeviceFrom(c)
	if !ok || h.leases == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req pollRequest
	if err := decodeWorkerJSON(c, &req, true); err != nil {
		return workerBodyError(c, err)
	}
	wait := h.pollWait
	if req.WaitSeconds != nil {
		if *req.WaitSeconds < 0 || time.Duration(*req.WaitSeconds)*time.Second > h.pollWait {
			return workerError(c, http.StatusBadRequest, "invalid request")
		}
		wait = time.Duration(*req.WaitSeconds) * time.Second
	}
	if req.Telemetry != nil && req.Telemetry.Validate() != nil {
		return workerError(c, http.StatusBadRequest, "invalid telemetry")
	}
	ctx := c.Request().Context()
	started := h.leases.PollStart() // fixed for every recheck of this poll (R-5)
	h.recordTelemetry(ctx, device, req.Telemetry)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	recheck := time.NewTicker(h.pollRecheck)
	defer recheck.Stop()
	for {
		changed := h.leases.JobsChanged()
		lease, found, err := h.leases.LeasePoll(ctx, device, started)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return workerStoreError(c, err)
		}
		if found {
			job := lease.Job
			expires := time.Now().Add(behavior.WorkerLeaseDuration)
			if job.LeaseExpiresAt != nil {
				expires = *job.LeaseExpiresAt
			}
			payload := job.Payload
			if len(payload) == 0 {
				payload = json.RawMessage(`{}`)
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			return c.JSON(http.StatusOK, pollResponse{
				JobID: job.ID, ExecutionID: job.ExecutionID, StepID: job.StepID, Attempt: job.Attempt,
				RequiredCapabilities: job.RequiredCapabilities, Payload: payload, LeaseToken: lease.LeaseToken,
				LeaseExpiresAt: expires.UTC(), LeaseSeconds: int(behavior.WorkerLeaseDuration / time.Second),
				HeartbeatIntervalSeconds: int(behavior.WorkerHeartbeatInterval / time.Second),
			})
		}
		select {
		case <-ctx.Done():
			return nil // client went away
		case <-deadline.C:
			return c.NoContent(http.StatusNoContent)
		case <-h.stopping:
			return c.NoContent(http.StatusNoContent) // server shutting down
		case <-changed:
		case <-recheck.C:
		}
	}
}

// Heartbeat extends the lease and tells the worker to cancel when the job is
// CANCELLING; pending owner instructions ride along.
func (h *WorkerHandler) Heartbeat(c echo.Context) error {
	device, ok := WorkerDeviceFrom(c)
	if !ok || h.leases == nil {
		return workerError(c, http.StatusNotFound, "not found")
	}
	var req heartbeatRequest
	if err := decodeWorkerBody(c, &req); err != nil {
		return workerBodyError(c, err)
	}
	if req.LeaseToken == "" {
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	if req.Telemetry != nil && req.Telemetry.Validate() != nil {
		return workerError(c, http.StatusBadRequest, "invalid telemetry")
	}
	ctx := c.Request().Context()
	job, err := h.leases.Heartbeat(ctx, device, c.Param("id"), req.LeaseToken)
	if err != nil {
		return workerStoreError(c, err)
	}
	h.recordTelemetry(ctx, device, req.Telemetry)
	instructions := []behavior.WorkerInstruction{}
	if h.instructions != nil {
		list, err := h.instructions.PendingWorkerInstructions(ctx, job)
		if err != nil {
			log.Print("worker: instructions unavailable")
		} else if list != nil {
			instructions = list
		}
	}
	return c.JSON(http.StatusOK, heartbeatResponse{
		State: job.State, Cancel: job.State == behavior.WorkerJobCancelling, LeaseExpiresAt: job.LeaseExpiresAt,
		HeartbeatIntervalSeconds: int(behavior.WorkerHeartbeatInterval / time.Second), Instructions: instructions,
	})
}

// Progress stores one bounded progress event, deduplicated by seq.
func (h *WorkerHandler) Progress(c echo.Context) error {
	device, ok := WorkerDeviceFrom(c)
	if !ok || h.leases == nil {
		return workerError(c, http.StatusNotFound, "not found")
	}
	var req progressRequest
	if err := decodeWorkerBody(c, &req); err != nil {
		return workerBodyError(c, err)
	}
	if req.LeaseToken == "" {
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	p := behavior.WorkerProgress{Seq: req.Seq, Phase: req.Phase, Counts: req.Counts}
	if p.Validate() != nil {
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	job, dup, err := h.leases.ReportProgress(c.Request().Context(), device, c.Param("id"), req.LeaseToken, p)
	if err != nil {
		return workerStoreError(c, err)
	}
	return c.JSON(http.StatusOK, progressResponse{State: job.State, Duplicate: dup, Cancel: job.State == behavior.WorkerJobCancelling})
}

// Result records the job result once per idempotency key (REQ-7).
func (h *WorkerHandler) Result(c echo.Context) error {
	device, ok := WorkerDeviceFrom(c)
	if !ok || h.leases == nil {
		return workerError(c, http.StatusNotFound, "not found")
	}
	var req resultRequest
	if err := decodeWorkerBody(c, &req); err != nil {
		return workerBodyError(c, err)
	}
	if req.LeaseToken == "" || !behavior.ValidWorkerIdempotencyKey(req.IdempotencyKey) || req.WorkerResult.Validate() != nil {
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	job, dup, err := h.leases.ReportResult(c.Request().Context(), device, c.Param("id"), req.LeaseToken, req.IdempotencyKey, req.WorkerResult)
	if err != nil {
		return workerStoreError(c, err)
	}
	return c.JSON(http.StatusOK, resultResponse{State: job.State, Duplicate: dup})
}
