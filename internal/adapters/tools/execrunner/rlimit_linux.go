//go:build linux

package execrunner

import "golang.org/x/sys/unix"

// applyLimits sets rlimits and niceness on the started child. There is a
// short window before the limits apply; the timeout and group kill bound it.
func applyLimits(pid int, cpuSeconds, asBytes, fsizeBytes uint64, nice int) error {
	for _, l := range []struct {
		res int
		val uint64
	}{{unix.RLIMIT_CPU, cpuSeconds}, {unix.RLIMIT_AS, asBytes}, {unix.RLIMIT_FSIZE, fsizeBytes}} {
		lim := unix.Rlimit{Cur: l.val, Max: l.val}
		if err := unix.Prlimit(pid, l.res, &lim, nil); err != nil {
			return err
		}
	}
	return unix.Setpriority(unix.PRIO_PROCESS, pid, nice)
}
