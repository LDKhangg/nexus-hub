//go:build unix

package worker

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the child in its own process group so a cancel
// stops the executor and everything it started. No Pdeathsig: it fires
// when the forking OS thread exits, which Go may do.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminateGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGTERM) }

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }
