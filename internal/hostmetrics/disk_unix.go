//go:build darwin || linux

package hostmetrics

import "golang.org/x/sys/unix"

func readDisk(root string) (uint64, uint64, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return 0, 0, false
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), true
}
