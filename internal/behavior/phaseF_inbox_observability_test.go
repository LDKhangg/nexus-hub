package behavior

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/obs"
)

type phaseFInboxStore struct {
	u         *InboundUpdate
	finishErr error
	finished  bool
}

func (s *phaseFInboxStore) RecordUpdate(context.Context, string, InboundUpdate) (bool, error) {
	return true, nil
}

func (s *phaseFInboxStore) ClaimUpdate(context.Context, time.Time, time.Duration) (*InboundUpdate, error) {
	u := s.u
	s.u = nil
	return u, nil
}

func (s *phaseFInboxStore) FinishUpdate(context.Context, int64, string, string) error {
	s.finished = true
	return s.finishErr
}

type phaseFFailingApprovals struct{ *p4Approvals }

func (s *phaseFFailingApprovals) GetApprovalByNonce(context.Context, string, string) (Approval, error) {
	return Approval{}, errors.New("phase-f planted storage failure")
}

// TC-F-18 (REQ-1, REQ-2): retry and finish-failure records retain the inbound
// correlation as a structured field with the update dimensions needed to act.
func TestPhaseFInboxFailureLogsCarryCorrelation(t *testing.T) {
	const correlation = "019a6690-abcd-7000-8000-0000000000f3"
	for _, tc := range []struct {
		name      string
		update    InboundUpdate
		finishErr error
		message   string
		code      string
		wantErr   bool
	}{
		{
			name: "retry", update: InboundUpdate{ID: 41, WorkspaceID: "workspace", IntegrationID: "integration", ChatID: 7,
				Kind: UpdateText, Args: "nonce", Attempts: 1, ClaimToken: "claim", CorrelationID: correlation},
			message: "telegram inbound update retry", code: "storage",
		},
		{
			name: "finish_failure", update: InboundUpdate{ID: 42, Kind: UpdateOther, Attempts: 2, ClaimToken: "claim", CorrelationID: correlation},
			finishErr: errors.New("phase-f planted finish failure"), message: "telegram inbound update finish failed", wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			oldDefault := slog.Default()
			t.Cleanup(func() { slog.SetDefault(oldDefault) })
			obs.Setup("info", &logs)
			inbox := &phaseFInboxStore{u: &tc.update, finishErr: tc.finishErr}
			worker := NewInboxWorker(InboxDeps{Inbox: inbox, Approvals: &phaseFFailingApprovals{newP4Approvals()}}, InboxSettings{})
			worked, err := worker.processNext(context.Background())
			if !worked || (err != nil) != tc.wantErr {
				t.Fatalf("worked=%v err=%v", worked, err)
			}
			if tc.message == "telegram inbound update retry" && inbox.finished {
				t.Fatal("retry update was finished")
			}
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Fatalf("failure log is not one JSON record: %q: %v", logs.String(), err)
			}
			if record["msg"] != tc.message || record["component"] != "telegram" || record["correlation_id"] != correlation ||
				record["update_id"] != float64(tc.update.ID) || record["kind"] != string(tc.update.Kind) ||
				record["code"] != tc.code || record["attempt"] != float64(tc.update.Attempts) {
				t.Fatalf("failure log shape: %#v", record)
			}
			if tc.wantErr && record["error_code"] != "finish_update_failed" {
				t.Fatalf("finish error code absent: %#v", record)
			}
		})
	}
}
