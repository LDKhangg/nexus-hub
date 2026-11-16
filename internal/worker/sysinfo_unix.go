//go:build unix

package worker

import "golang.org/x/sys/unix"

// diskUsage is the used and total bytes of the filesystem holding dir.
func diskUsage(dir string) (used, total uint64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	total = uint64(st.Blocks) * bs
	free := uint64(st.Bfree) * bs
	if free > total {
		return 0, 0, false
	}
	return total - free, total, true
}
