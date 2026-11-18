//go:build unix

package hookproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/digitaldrywood/detent/internal/procstart"
	"golang.org/x/sys/unix"
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
	if err == nil {
		l.writer, err = os.OpenFile(l.path(), os.O_WRONLY|unix.O_NONBLOCK, 0)
	}
	if err == nil {
		err = l.writer.Chmod(0400)
	}
	if err != nil {
		err = errors.Join(err, l.CloseWriter())
		if l.reader != nil {
			err = errors.Join(err, l.reader.Close())
		}
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return l, nil
}

func (l *Lifetime) Capture(ctx context.Context, path, owner string) (bool, error) {
	ctx = l.withOwner(ctx, owner)
	verified, _ := verifiedProcessTargets(ctx, path, ReapProcessScanner, procstart.Identity, processEnvironment, syscall.Kill)
	pids, err := verified(ctx, path)
	if err != nil || len(pids) > 0 {
		return len(pids) > 0, err
	}
	return l.Active()
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
	active, err := l.Active()
	if err != nil || !active {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		return l.darwinWriters(ctx, 0)
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
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-w", "-F", "p", "--", l.path())
	data, err := workspaceScanOutput(ctx, cmd)
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
		writers, err := l.darwinWriters(ctx, pid)
		return slices.Contains(writers, pid), err
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
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-w", "-a", "-p", strconv.Itoa(pid), "-d", "4", "-F", "a", "--", l.path())
	data, err := workspaceScanOutput(ctx, cmd)
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ExitCode() == 1 && ctx.Err() == nil {
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
