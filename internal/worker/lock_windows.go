//go:build windows

package worker

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, non-blocking LockFileEx lock on path; it is
// released when the process exits.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("lock file not writable")
	}
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, errors.New("lock is held")
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
		f.Close()
	}, nil
}

// secretModeOK: Windows has no POSIX modes; the file lives in the user's
// profile, protected by its ACL.
func secretModeOK(os.FileMode) bool { return true }
