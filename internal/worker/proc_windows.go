//go:build windows

package worker

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows (no POSIX process groups).
func setProcessGroup(cmd *exec.Cmd) {}

func terminateGroup(pid int) { killGroup(pid) }

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
