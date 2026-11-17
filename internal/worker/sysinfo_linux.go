//go:build linux

package worker

import (
	"os"
	"strconv"
	"strings"
)

// cpuRAM reads the 1-minute load average and memory from /proc.
func cpuRAM() (load *float64, used, total *int64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil && v >= 0 {
				load = &v
			}
		}
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return load, nil, nil
	}
	var memTotal, memAvail int64 = -1, -1
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			memTotal = v * 1024
		case "MemAvailable:":
			memAvail = v * 1024
		}
	}
	if memTotal > 0 {
		total = &memTotal
		if memAvail >= 0 && memAvail <= memTotal {
			u := memTotal - memAvail
			used = &u
		}
	}
	return load, used, total
}
