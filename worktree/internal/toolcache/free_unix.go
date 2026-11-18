//go:build darwin || linux

package toolcache

import "golang.org/x/sys/unix"

func volumeFreeBytes(root string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func volumeCapacityBytes(root string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return 0, err
	}
	return stat.Blocks * uint64(stat.Bsize), nil
}
