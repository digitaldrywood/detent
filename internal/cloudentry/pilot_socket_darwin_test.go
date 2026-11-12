package cloudentry

import (
	"errors"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func pilotSocketPath(path string, action func(string) error) error {
	if len(path) < len(syscall.RawSockaddrUnix{}.Path) {
		return action(path)
	}
	runtime.LockOSThread()
	if err := unix.PthreadChdir(filepath.Dir(path)); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	err := action(filepath.Base(path))
	if resetErr := unix.PthreadFchdir(-1); resetErr != nil {
		return errors.Join(err, resetErr)
	}
	runtime.UnlockOSThread()
	return err
}
