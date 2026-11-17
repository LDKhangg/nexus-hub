//go:build !linux && !darwin

package worker

import "log"

// systemInhibitor is a no-op here; the worker never changes power settings.
type systemInhibitor struct{}

// Start implements Inhibitor.
func (systemInhibitor) Start() func() {
	log.Print("worker: no sleep inhibitor on this platform; keep the machine awake while jobs run")
	return func() {}
}
