package instancelock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrHeld = errors.New("instance lock is held")

var localLocks = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

type Lock struct {
	path      string
	file      *os.File
	recovered *Recovery
	closeOnce sync.Once
	closeErr  error
}

type Owner struct {
	PID       int
	Hostname  string
	StartedAt time.Time
}

type HeldError struct {
	Path          string
	Owner         Owner
	MetadataError error
}

func (e *HeldError) Error() string {
	if e.Owner.PID > 0 && !e.Owner.StartedAt.IsZero() {
		return fmt.Sprintf("%s: pid %d on %s, started %s", ErrHeld, e.Owner.PID, e.Owner.Hostname, e.Owner.StartedAt.Format(time.RFC3339))
	}
	if e.MetadataError != nil {
		return fmt.Sprintf("%s: %s: unreadable owner metadata: %v", ErrHeld, e.Path, e.MetadataError)
	}
	return fmt.Sprintf("%s: %s", ErrHeld, e.Path)
}

func (e *HeldError) Unwrap() error {
	return ErrHeld
}

type Status string

const (
	StatusMissing Status = "missing"
	StatusClear   Status = "clear"
	StatusStale   Status = "stale"
	StatusHeld    Status = "held"
)

type Inspection struct {
	Path          string
	Status        Status
	Owner         Owner
	MetadataError error
}

type Recovery struct {
	Owner         Owner
	MetadataError error
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
		owner, metadataErr := readLockedOwnerPath(absolutePath)
		return nil, &HeldError{Path: absolutePath, Owner: owner, MetadataError: metadataErr}
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
		owner, metadataErr := readLockedOwner(file)
		closeErr := file.Close()
		if errors.Is(err, ErrHeld) {
			return nil, errors.Join(&HeldError{Path: absolutePath, Owner: owner, MetadataError: metadataErr}, closeErr)
		}
		return nil, fmt.Errorf("acquire instance lock: %w", errors.Join(err, closeErr))
	}
	previousOwner, metadataErr := readOwner(file)
	owner, err := currentOwner()
	if err != nil {
		return nil, errors.Join(err, unlock(file), file.Close())
	}
	if err := writeOwner(file, owner); err != nil {
		return nil, errors.Join(err, unlock(file), file.Close())
	}

	releaseClaim = false
	lock := &Lock{path: absolutePath, file: file}
	if !errors.Is(metadataErr, errEmptyOwner) {
		lock.recovered = &Recovery{Owner: previousOwner, MetadataError: metadataErr}
	}
	return lock, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		clearErr := clearOwner(l.file)
		unlockErr := unlock(l.file)
		closeErr := l.file.Close()
		releaseLocal(l.path)
		l.closeErr = errors.Join(clearErr, unlockErr, closeErr)
	})
	return l.closeErr
}

func (l *Lock) Recovery() (Recovery, bool) {
	if l == nil || l.recovered == nil {
		return Recovery{}, false
	}
	return *l.recovered, true
}

func Inspect(path string) (Inspection, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Inspection{}, fmt.Errorf("resolve instance lock path: %w", err)
	}
	inspection := Inspection{Path: absolutePath, Status: StatusMissing}
	if isLocallyClaimed(absolutePath) {
		inspection.Status = StatusHeld
		inspection.Owner, inspection.MetadataError = readLockedOwnerPath(absolutePath)
		return inspection, nil
	}
	file, err := os.OpenFile(absolutePath, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return inspection, nil
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("open instance lock: %w", err)
	}

	if err := tryLock(file); err != nil {
		if !errors.Is(err, ErrHeld) {
			return Inspection{}, fmt.Errorf("inspect instance lock: %w", errors.Join(err, file.Close()))
		}
		inspection.Status = StatusHeld
		inspection.Owner, inspection.MetadataError = readLockedOwner(file)
		return inspection, file.Close()
	}

	inspection.Owner, inspection.MetadataError = readOwner(file)
	cleanupErr := errors.Join(unlock(file), file.Close())
	if cleanupErr != nil {
		return Inspection{}, fmt.Errorf("finish instance lock inspection: %w", cleanupErr)
	}
	if errors.Is(inspection.MetadataError, errEmptyOwner) {
		inspection.Status = StatusClear
		inspection.MetadataError = nil
		return inspection, nil
	}
	inspection.Status = StatusStale
	return inspection, nil
}

var errEmptyOwner = errors.New("instance lock owner is empty")

func currentOwner() (Owner, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Owner{}, fmt.Errorf("resolve instance lock hostname: %w", err)
	}
	return Owner{PID: os.Getpid(), Hostname: hostname, StartedAt: time.Now().UTC()}, nil
}

func writeOwner(file *os.File, owner Owner) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate instance lock: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek instance lock: %w", err)
	}
	if _, err := fmt.Fprintf(file, "\npid=%d\nhostname=%s\nstarted_at=%s\n", owner.PID, owner.Hostname, owner.StartedAt.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("write instance lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync instance lock owner: %w", err)
	}
	return nil
}

func clearOwner(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("clear instance lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync cleared instance lock owner: %w", err)
	}
	return nil
}

func readLockedOwnerPath(path string) (Owner, error) {
	file, err := os.Open(path)
	if err != nil {
		return Owner{}, err
	}
	owner, readErr := readLockedOwner(file)
	return owner, errors.Join(readErr, file.Close())
}

func readOwner(file *os.File) (Owner, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return Owner{}, fmt.Errorf("seek instance lock owner: %w", err)
	}
	return scanOwner(file)
}

func readLockedOwner(file *os.File) (Owner, error) {
	if err := seekLockedOwner(file); err != nil {
		return Owner{}, fmt.Errorf("seek locked instance lock owner: %w", err)
	}
	return scanOwner(file)
}

func scanOwner(file *os.File) (Owner, error) {
	values := make(map[string]string, 3)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return Owner{}, fmt.Errorf("read instance lock owner: %w", err)
	}
	if len(values) == 0 {
		return Owner{}, errEmptyOwner
	}
	pid, err := strconv.Atoi(values["pid"])
	if err != nil || pid <= 0 {
		return Owner{}, fmt.Errorf("invalid pid %q", values["pid"])
	}
	hostname := values["hostname"]
	if hostname == "" {
		return Owner{PID: pid}, errors.New("hostname is missing")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, values["started_at"])
	if err != nil {
		return Owner{PID: pid, Hostname: hostname}, fmt.Errorf("invalid started_at %q: %w", values["started_at"], err)
	}
	return Owner{PID: pid, Hostname: hostname, StartedAt: startedAt}, nil
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

func isLocallyClaimed(path string) bool {
	localLocks.Lock()
	defer localLocks.Unlock()
	_, exists := localLocks.paths[path]
	return exists
}
