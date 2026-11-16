//go:build unix

package execrunner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the child in its own process group.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }
