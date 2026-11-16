//go:build unix

package worker

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking flock on path. The kernel
// drops it when the process dies, so a crash never leaves a stale lock.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("lock file not writable")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("lock is held")
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

func secretModeOK(m os.FileMode) bool { return m.Perm()&0o077 == 0 }
