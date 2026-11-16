//go:build windows

package clirun

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows: there are no POSIX process groups.
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills the child itself; grandchildren are not tracked.
func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
