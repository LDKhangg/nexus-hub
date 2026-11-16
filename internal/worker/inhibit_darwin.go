//go:build darwin

package worker

import (
	"log"
	"os"
	"os/exec"
	"strconv"
)

// systemInhibitor runs `caffeinate -i -w <worker pid>` for the job's
// lifetime (best effort); caffeinate also exits if the worker dies.
type systemInhibitor struct{}

// Start implements Inhibitor.
func (systemInhibitor) Start() func() {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		log.Print("worker: sleep inhibitor unavailable; the machine may sleep during the job")
		return func() {}
	}
	pid := cmd.Process.Pid
	return func() {
		terminateGroup(pid)
		_ = cmd.Wait()
	}
}
