//go:build !windows

package cloudentry

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func verifyPrivateSocket(path string) error {
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 {
		return errors.New("tenant socket directory must be private to the service user")
	}
	socket, err := os.Lstat(path)
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		return errors.New("tenant endpoint is not a socket")
	}
	uid := os.Getuid()
	for _, info := range []os.FileInfo{directory, socket} {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != uid {
			return errors.New("tenant socket must be owned by the service user")
		}
	}
	return nil
}
