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
	if err != nil {
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

func hookDescriptorMatches(root string, pid int, marker os.FileInfo) bool {
	fd, err := os.Stat(filepath.Join(root, strconv.Itoa(pid), "fd", "4"))
	return err == nil && os.SameFile(fd, marker)
}
