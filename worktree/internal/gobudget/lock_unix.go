//go:build unix

package gobudget

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(path string) (func() error, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errSlotBusy
		}
		return nil, errors.Join(err, file.Close())
	}
	return file.Close, nil
}
