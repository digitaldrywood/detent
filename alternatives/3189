//go:build unix

package toolcache

import (
	"errors"
	"os"
	"syscall"
)

func tryCacheLock(file *os.File, shared bool) error {
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errCacheInUse
	}
	return err
}

func cacheEntryPinned(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink > 1, nil
}
