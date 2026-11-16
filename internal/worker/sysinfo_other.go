//go:build !linux && !darwin

package worker

// cpuRAM is unknown on this platform.
func cpuRAM() (load *float64, used, total *int64) { return nil, nil, nil }
