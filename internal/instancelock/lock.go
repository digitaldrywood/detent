package instancelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrHeld = errors.New("instance lock is held")

var localLocks = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

type Lock struct {
	path      string
	file      *os.File
	closeOnce sync.Once
	closeErr  error
}

func Acquire(path string) (*Lock, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve instance lock path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return nil, fmt.Errorf("create instance lock directory: %w", err)
	}
	if !claimLocal(absolutePath) {
		return nil, fmt.Errorf("%w: %s", ErrHeld, absolutePath)
	}
	releaseClaim := true
	defer func() {
		if releaseClaim {
			releaseLocal(absolutePath)
		}
	}()

	file, err := os.OpenFile(absolutePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := tryLock(file); err != nil {
		closeErr := file.Close()
		if errors.Is(err, ErrHeld) {
			return nil, errors.Join(fmt.Errorf("%w: %s", ErrHeld, absolutePath), closeErr)
		}
		return nil, fmt.Errorf("acquire instance lock: %w", errors.Join(err, closeErr))
	}
	if err := writeOwner(file); err != nil {
		return nil, errors.Join(err, unlock(file), file.Close())
	}

	releaseClaim = false
	return &Lock{path: absolutePath, file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		unlockErr := unlock(l.file)
		closeErr := l.file.Close()
		releaseLocal(l.path)
		l.closeErr = errors.Join(unlockErr, closeErr)
	})
	return l.closeErr
}

func writeOwner(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate instance lock: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek instance lock: %w", err)
	}
	if _, err := fmt.Fprintf(file, "pid=%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("write instance lock owner: %w", err)
	}
	return nil
}

func claimLocal(path string) bool {
	localLocks.Lock()
	defer localLocks.Unlock()
	if _, exists := localLocks.paths[path]; exists {
		return false
	}
	localLocks.paths[path] = struct{}{}
	return true
}

func releaseLocal(path string) {
	localLocks.Lock()
	defer localLocks.Unlock()
	delete(localLocks.paths, path)
}
