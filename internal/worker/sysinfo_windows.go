//go:build windows

package worker

import "golang.org/x/sys/windows"

// diskUsage is the used and total bytes of the volume holding dir.
func diskUsage(dir string) (used, total uint64, ok bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, false
	}
	var avail, tot, free uint64
	if windows.GetDiskFreeSpaceEx(p, &avail, &tot, &free) != nil || free > tot {
		return 0, 0, false
	}
	return tot - free, tot, true
}
