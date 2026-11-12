//go:build !linux

package execrunner

func applyLimits(int, uint64, uint64, uint64, int) error { return nil }
