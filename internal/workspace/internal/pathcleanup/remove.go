package pathcleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

type ProcessError struct{ Err error }

func (e *ProcessError) Error() string { return e.Err.Error() }
func (e *ProcessError) Unwrap() error { return e.Err }

type RemovalError struct{ Err error }

func (e *RemovalError) Error() string { return e.Err.Error() }
func (e *RemovalError) Unwrap() error { return e.Err }

func Remove(ctx context.Context, path string, removeAll func(string) error, reap func(context.Context, string, time.Duration) (int, error)) error {
	permissionFixed := false
	for attempt := 0; ; attempt++ {
		err := removeAll(path)
		if err == nil {
			break
		}
		if errors.Is(err, fs.ErrPermission) && !permissionFixed {
			if chmodErr := MakeRemovable(path); chmodErr != nil {
				return &RemovalError{Err: errors.Join(err, fmt.Errorf("remediate workspace permissions: %w", chmodErr))}
			}
			permissionFixed = true
			err = removeAll(path)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return &RemovalError{Err: err}
			}
		} else if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return &RemovalError{Err: err}
		}
		if attempt >= 3 {
			return &RemovalError{Err: err}
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &ProcessError{Err: ctx.Err()}
		case <-timer.C:
		}
		if _, reapErr := reap(ctx, path, 0); reapErr != nil {
			return &ProcessError{Err: fmt.Errorf("reap workspace writers: %w", reapErr)}
		}
	}
	return nil
}

func LogFailure(logger *slog.Logger, path string, err error) {
	var entries []string
	var listErr error
	if dir, openErr := os.Open(path); openErr != nil {
		listErr = openErr
	} else {
		entries, listErr = dir.Readdirnames(32)
		if errors.Is(listErr, io.EOF) {
			listErr = nil
		}
		listErr = errors.Join(listErr, dir.Close())
	}
	logger.Warn("validation scratch cleanup deferred to retention", "error", err, "surviving_entries", entries, "list_error", listErr)
}

func MakeRemovable(path string) error {
	return filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		mode := info.Mode().Perm()
		switch {
		case entry.IsDir():
			mode |= 0o700
		case runtime.GOOS == "windows":
			mode |= 0o600
		default:
			return nil
		}
		if err := os.Chmod(path, mode); err != nil { // #nosec G122 -- the path is confined by validateWorkspacePath and symlinks are skipped before chmod.
			return fmt.Errorf("chmod %s: %w", path, err)
		}
		return nil
	})
}
