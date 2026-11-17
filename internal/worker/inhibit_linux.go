//go:build linux

package worker

import (
	"log"
	"os/exec"
)

// systemInhibitor holds a logind sleep/idle inhibitor with a
// `systemd-inhibit --what=sleep:idle` child for the job's lifetime (best
// effort). It never changes power settings.
type systemInhibitor struct{}

// Start implements Inhibitor.
func (systemInhibitor) Start() func() {
	cmd := exec.Command("systemd-inhibit", "--what=sleep:idle", "--who=nexus-hub worker",
		"--why=a local worker job is running", "--mode=block", "sleep", "infinity")
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
