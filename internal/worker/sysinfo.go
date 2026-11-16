package worker

import (
	"runtime"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// systemInfo reads metrics from the OS. It never collects paths,
// hostnames, addresses or process lists.
type systemInfo struct{}

// Snapshot implements SysInfo.
func (systemInfo) Snapshot(dir string) behavior.WorkerTelemetry {
	var t behavior.WorkerTelemetry
	cores := int64(runtime.NumCPU())
	if cores >= 1 {
		t.CPUCores = &cores
	}
	t.CPULoad, t.RAMUsedBytes, t.RAMTotalBytes = cpuRAM()
	if used, total, ok := diskUsage(dir); ok && total > 0 && used <= total {
		u, tt := int64(used), int64(total)
		t.DiskUsedBytes, t.DiskTotalBytes = &u, &tt
	}
	return t
}
