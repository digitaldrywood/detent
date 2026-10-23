//go:build windows

package toolcache

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryCacheLock(file *os.File, shared bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errCacheInUse
	}
	return err
}
