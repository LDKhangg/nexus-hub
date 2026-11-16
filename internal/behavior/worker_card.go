package behavior

import (
	"fmt"
	"strings"
	"time"
)

// WorkerDeviceInfo is one device with its latest telemetry, as shown by the
// Telegram /worker card and printed by nh worker devices --json (REQ-18).
// Every telemetry value is optional: nil means unknown.
type WorkerDeviceInfo struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	State        string           `json:"state"`
	Capabilities []string         `json:"capabilities"`
	LastSeenAt   *time.Time       `json:"last_seen_at"`
	CreatedAt    time.Time        `json:"created_at"`
	RevokedAt    *time.Time       `json:"revoked_at"`
	ReportedAt   *time.Time       `json:"reported_at,omitempty"`
	Telemetry    *WorkerTelemetry `json:"telemetry,omitempty"`
}

// BuildWorkerDeviceInfos joins devices with their telemetry snapshots. The
// reported state of a live device (idle, busy) follows its telemetry; an
// offline or revoked device keeps its display state.
func BuildWorkerDeviceInfos(devices []WorkerDevice, records []WorkerTelemetryRecord, now time.Time) []WorkerDeviceInfo {
	byDevice := make(map[string]WorkerTelemetryRecord, len(records))
	for _, r := range records {
		byDevice[r.DeviceID] = r
	}
	out := make([]WorkerDeviceInfo, 0, len(devices))
	for _, d := range devices {
		caps := d.Capabilities
		if caps == nil {
			caps = []string{}
		}
		info := WorkerDeviceInfo{
			ID: d.ID, Name: d.Name, State: d.DisplayState(now), Capabilities: caps,
			LastSeenAt: d.LastSeenAt, CreatedAt: d.CreatedAt, RevokedAt: d.RevokedAt,
		}
		if rec, ok := byDevice[d.ID]; ok {
			snap, at := rec.Snapshot, rec.ReceivedAt
			info.Telemetry, info.ReportedAt = &snap, &at
		}
		out = append(out, info)
	}
	return out
}

const unknownText = "không rõ"

var workerStateText = map[string]string{
	"online": "online", "busy": "đang bận", "offline": "offline", "revoked": "đã thu hồi",
}

// FormatWorkerCard renders one device as a short card: state, CPU, RAM,
// disk, Claude limits, tokens, jobs, failures, busy hours and last seen.
func FormatWorkerCard(info WorkerDeviceInfo) string {
	state := workerStateText[info.State]
	if state == "" {
		state = info.State
	}
	t := info.Telemetry
	lines := []string{fmt.Sprintf("%s · %s", info.Name, state)}
	if t == nil {
		lines = append(lines, "CPU: "+unknownText+" · RAM: "+unknownText+" · Ổ đĩa: "+unknownText)
		lines = append(lines, "Hạn mức Claude: "+unknownText)
	} else {
		lines = append(lines, "CPU: "+cpuText(t)+" · RAM: "+usedTotalText(t.RAMUsedBytes, t.RAMTotalBytes)+" · Ổ đĩa: "+usedTotalText(t.DiskUsedBytes, t.DiskTotalBytes))
		lines = append(lines, "Hạn mức Claude: phiên "+claudeLeftText(t.ClaudeSessionLeftPct, t.ClaudeSessionResetAt)+" · tuần "+claudeLeftText(t.ClaudeWeeklyLeftPct, t.ClaudeWeeklyResetAt))
		c := t.Counters
		lines = append(lines, fmt.Sprintf("Token: %s · Việc: %d · Lỗi: %d · Giờ chạy: %.1f", tokenText(c.ExecutorTokens), c.Jobs, c.Failures, float64(c.BusySeconds)/3600))
		if t.Executor != "" {
			exec := t.Executor
			if t.ExecutorVersion != "" {
				exec += " " + t.ExecutorVersion
			}
			lines = append(lines, "Trình thực thi: "+exec)
		}
	}
	seen := "chưa từng kết nối"
	if info.LastSeenAt != nil {
		seen = info.LastSeenAt.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	lines = append(lines, "Lần cuối: "+seen)
	return strings.Join(lines, "\n")
}

func cpuText(t *WorkerTelemetry) string {
	switch {
	case t.CPULoad != nil && t.CPUCores != nil:
		return fmt.Sprintf("tải %.2f / %d nhân", *t.CPULoad, *t.CPUCores)
	case t.CPULoad != nil:
		return fmt.Sprintf("tải %.2f", *t.CPULoad)
	case t.CPUCores != nil:
		return fmt.Sprintf("%d nhân", *t.CPUCores)
	}
	return unknownText
}

func usedTotalText(used, total *int64) string {
	switch {
	case used != nil && total != nil && *total > 0:
		return fmt.Sprintf("%s / %s (%d%%)", gibText(*used), gibText(*total), *used*100 / *total)
	case total != nil:
		return "tổng " + gibText(*total)
	}
	return unknownText
}

func gibText(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

func claudeLeftText(left *int64, reset *time.Time) string {
	if left == nil {
		return unknownText
	}
	s := fmt.Sprintf("còn %d%%", *left)
	if reset != nil {
		s += " (reset " + reset.UTC().Format("01-02 15:04") + " UTC)"
	}
	return s
}

func tokenText(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
