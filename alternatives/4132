//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const migrationLockPoll = 50 * time.Millisecond

func lockMigrations(ctx context.Context, path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return nil, errors.Join(err, file.Close())
		}
		timer := time.NewTimer(migrationLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-timer.C:
		}
	}
}
