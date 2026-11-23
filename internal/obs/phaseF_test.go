package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type phaseFStringer string

func (s phaseFStringer) String() string { return string(s) }

func phaseFLogLines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// TC-F-1 (REQ-1): process, context-enriched and bridged standard-library
// records share one JSON envelope and retain the required identifiers.
func TestPhaseFStructuredLoggingAndLegacyBridge(t *testing.T) {
	oldDefault := slog.Default()
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(oldDefault)
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	var out bytes.Buffer
	logger := Setup("info", &out)
	corr := NewCorrelationID()
	if id, err := uuid.Parse(corr); err != nil || id.Version() != 7 {
		t.Fatalf("NewCorrelationID() = %q, want UUIDv7", corr)
	}
	ctx := WithExecution(WithCorrelation(context.Background(), corr), 91)
	Logger(ctx).Info("step", "component", "workflow", "state", "running")
	BridgeStdLog(logger)
	log.Print("old package")

	records := phaseFLogLines(t, &out)
	if len(records) != 2 {
		t.Fatalf("records=%d, want 2", len(records))
	}
	for i, record := range records {
		for _, key := range []string{"time", "level", "msg", "component"} {
			if record[key] == nil || record[key] == "" {
				t.Errorf("record %d missing %s: %#v", i, key, record)
			}
		}
	}
	if records[0]["correlation_id"] != corr || records[0]["execution_id"] != float64(91) || records[0]["component"] != "workflow" {
		t.Fatalf("context record lost fields: %#v", records[0])
	}
	if records[1]["component"] != "legacy" || records[1]["msg"] != "old package" {
		t.Fatalf("legacy record=%#v", records[1])
	}
}

// TC-F-2 (REQ-1): configured levels filter lower-priority records and unknown
// settings use info, rather than silently enabling debug output.
func TestPhaseFLogLevelFilteringAndDefault(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	for _, tc := range []struct {
		level string
		want  []string
	}{
		{level: "debug", want: []string{"debug", "info", "warn", "error"}},
		{level: "warn", want: []string{"warn", "error"}},
		{level: "error", want: []string{"error"}},
		{level: "not-a-level", want: []string{"info", "warn", "error"}},
	} {
		t.Run(tc.level, func(t *testing.T) {
			var out bytes.Buffer
			logger := Setup(tc.level, &out)
			logger.Debug("debug")
			logger.Info("info")
			logger.Warn("warn")
			logger.Error("error")
			records := phaseFLogLines(t, &out)
			if len(records) != len(tc.want) {
				t.Fatalf("messages=%d, want %d: %s", len(records), len(tc.want), out.String())
			}
			for i, want := range tc.want {
				if records[i]["msg"] != want {
					t.Errorf("message %d=%v, want %q", i, records[i]["msg"], want)
				}
			}
		})
	}
}

// TC-F-8 (REQ-3, abuse): no supported slog value shape can bypass secret
// masking. Byte slices are discarded completely.
func TestPhaseFRedactingHandlerCoversMessagesAndAttributeShapes(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	const token = "github_pat_123456789012345678901234567890"
	const dsn = "postgres://owner:super-secret-password@127.0.0.1/db"
	var out bytes.Buffer
	logger := Setup("info", &out)
	logger.Info("Authorization: Bearer "+token,
		"string", token,
		"error", errors.New(dsn),
		"stringer", phaseFStringer("sk-ant-1234567890SECRET"),
		"bytes", []byte(token),
		slog.Group("nested", "token", token),
	)
	records := phaseFLogLines(t, &out)
	if len(records) != 1 {
		t.Fatalf("records=%d", len(records))
	}
	raw := out.String()
	for _, forbidden := range []string{token, "super-secret-password", "1234567890SECRET"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("secret %q leaked in %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, "[REDACTED]") || records[0]["bytes"] != "[REDACTED]" {
		t.Fatalf("redaction marker or byte masking absent: %s", raw)
	}
}
