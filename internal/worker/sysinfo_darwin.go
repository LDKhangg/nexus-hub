//go:build darwin

package worker

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// cpuRAM reads the 1-minute load average and total memory with sysctl.
// Memory in use needs Mach host statistics and is left unknown.
func cpuRAM() (load *float64, used, total *int64) {
	if raw, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(raw) >= 24 {
		ld := binary.LittleEndian.Uint32(raw[0:4])
		scale := int64(binary.LittleEndian.Uint64(raw[16:24]))
		if scale > 0 {
			v := float64(ld) / float64(scale)
			load = &v
		}
	}
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil && mem > 0 && mem < 1<<50 {
		t := int64(mem)
		total = &t
	}
	return load, nil, total
}
