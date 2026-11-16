//go:build windows

package codetools

import "golang.org/x/sys/windows"

// availableBytes is the space available to the caller on the volume
// holding dir.
func availableBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
