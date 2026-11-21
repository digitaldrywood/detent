//go:build unix

package hookproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/procstart"
)

func NewLifetime() (*Lifetime, error) {
	dir, err := os.MkdirTemp("", "detent-hook-")
	if err != nil {
		return nil, err
	}
	l := &Lifetime{dir: dir, starts: make(map[int]string)}
	if err = unix.Mkfifo(l.path(), 0600); err == nil {
		l.reader, err = os.OpenFile(l.path(), os.O_RDONLY|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return l, nil
}

func (l *Lifetime) Capture(ctx context.Context, path, owner string) (bool, error) {
	ctx = l.withOwner(ctx, owner)
	verified, _ := verifiedProcessTargets(ctx, path, ReapProcessScanner, procstart.Identity, processEnvironment, nil)
	pids, err := verified(ctx, path)
	if err == nil && l.unidentifiedWriter {
		err = errors.New("observed hook descriptor writer lacks a verified process identity")
	}
	for pid, start := range l.witnesses {
		if l.starts[pid] == start {
			continue
		}
		env, inspectErr := processEnvironment(pid)
		alive, matchErr := procstart.Matches(pid, start)
		if alive && inspectErr == nil && !hookDescriptorAllowed(owner, path, env) {
			err = errors.Join(err, errors.New("hook descriptor writer belongs to another owner"))
		}
		err = errors.Join(err, matchErr)
	}
	return len(l.witnesses) > 0 || len(pids) > 0, err
}

type writerSnapshotKey struct{}

func (l *Lifetime) recordWriters(ctx context.Context) (context.Context, error) {
	retained := l.retainedWriters(ctx)
	holders, err := l.candidates(ctx)
	ctx = context.WithValue(ctx, writerSnapshotKey{}, holders)
	matched, verifyErr := l.recordWriterCandidates(ctx, holders)
	err = errors.Join(err, verifyErr)
	if !matched && err == nil {
		matched, err = l.stillHoldsWriter(ctx, retained)
	}
	if !matched || err != nil {
		active, activeErr := l.Active()
		if active || activeErr != nil {
			l.unidentifiedWriter = true
		}
		err = errors.Join(err, activeErr)
	}
	return ctx, err
}

func (l *Lifetime) retainedWriters(ctx context.Context) map[int]string {
	retained := make(map[int]string, len(l.starts))
	for pid, start := range l.starts {
		if observed, err := l.writerIdentity(ctx, pid); err == nil && observed != "" && observed == start {
			retained[pid] = observed
		}
	}
	return retained
}

func (l *Lifetime) stillHoldsWriter(ctx context.Context, before map[int]string) (bool, error) {
	for pid, start := range before {
		observed, err := l.writerIdentity(ctx, pid)
		if err != nil || observed == start {
			return observed == start, err
		}
	}
	return false, nil
}

func (l *Lifetime) recordWriterCandidates(ctx context.Context, candidates []int) (bool, error) {
	matched := false
	var err error
	for _, pid := range candidates {
		start, inspectErr := l.writerIdentity(ctx, pid)
		err = errors.Join(err, inspectErr)
		if start == "" {
			continue
		}
		matched = true
		if l.starts[pid] != start {
			l.recordWitness(pid, start)
		}
	}
	return matched, err
}

func (l *Lifetime) writerIdentity(ctx context.Context, pid int) (string, error) {
	start, err := procstart.Identity(pid)
	if errors.Is(err, procstart.ErrNotRunning) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	writer, err := l.holdsWriter(ctx, pid)
	if err != nil || !writer {
		return "", err
	}
	current, err := procstart.Matches(pid, start)
	if err != nil || !current {
		return "", err
	}
	return start, nil
}

func (l *Lifetime) Active() (bool, error) {
	var data [4096]byte
	n, err := unix.Read(int(l.reader.Fd()), data[:])
	if errors.Is(err, unix.EAGAIN) || n > 0 {
		return true, nil
	}
	return false, err
}

func (l *Lifetime) candidates(ctx context.Context) ([]int, error) {
	if snapshot, ok := ctx.Value(writerSnapshotKey{}).([]int); ok {
		return snapshot, nil
	}
	active, err := l.Active()
	if err != nil || !active {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		return l.platformWriters(ctx)
	}
	if runtime.GOOS == "linux" {
		pids, err := l.procDescriptorHolders(ctx, "/proc")
		if len(pids) != 0 || err != nil {
			return pids, err
		}
		active, err = l.Active()
		if err != nil || !active {
			return nil, err
		}
	}
	if err := l.checkPath(); err != nil {
		return nil, err
	}
	data, err := lsofOutput(ctx, "-nP", "-w", "-F", "p", "--", l.path())
	if err != nil {
		if active, checkErr := l.Active(); checkErr == nil && !active {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect hook descriptor: %w", err)
	}
	var pids []int
	for line := range strings.Lines(string(data)) {
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "p")))
		if err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (l *Lifetime) procDescriptorHolders(ctx context.Context, root string) ([]int, error) {
	marker, err := l.reader.Stat()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if hookDescriptorMatches(root, pid, marker) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (l *Lifetime) holdsWriter(ctx context.Context, pid int) (bool, error) {
	if runtime.GOOS == "darwin" {
		return l.platformHoldsWriter(pid)
	}
	if runtime.GOOS == "linux" {
		marker, err := l.reader.Stat()
		if err != nil || !hookDescriptorMatches("/proc", pid, marker) {
			return false, err
		}
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "fdinfo", "4"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for line := range strings.Lines(string(data)) {
			if value, ok := strings.CutPrefix(line, "flags:"); ok {
				flags, err := strconv.ParseUint(strings.TrimSpace(value), 8, 64)
				return flags&unix.O_ACCMODE == unix.O_WRONLY, err
			}
		}
		return false, errors.New("hook descriptor access mode unavailable")
	}
	if err := l.checkPath(); err != nil {
		return false, err
	}
	data, err := lsofOutput(ctx, "-nP", "-w", "-a", "-p", strconv.Itoa(pid), "-d", "4", "-F", "a", "--", l.path())
	if lsofNoMatches(err) && ctx.Err() == nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect hook descriptor access: %w", err)
	}
	return slices.Contains(strings.Fields(string(data)), "aw"), nil
}

func hookDescriptorMatches(root string, pid int, marker os.FileInfo) bool {
	fd, err := os.Stat(filepath.Join(root, strconv.Itoa(pid), "fd", "4"))
	return err == nil && os.SameFile(fd, marker)
}
