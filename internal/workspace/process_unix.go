//go:build unix

package workspace

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func reapWorkspaceProcesses(ctx context.Context, path string, logger *slog.Logger) int {
	pids, err := workspaceProcessIDs(ctx, path)
	if err != nil {
		if logger != nil {
			logger.Warn("workspace process scan failed", slog.String("path", path), slog.Any("error", err))
		}
		return 0
	}

	killed := 0
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err == nil {
			killed++
			continue
		}
		if !errors.Is(err, syscall.ESRCH) && logger != nil {
			logger.Warn("workspace process kill failed", slog.String("path", path), slog.Int("pid", pid), slog.Any("error", err))
		}
	}
	return killed
}

func workspaceProcessIDs(ctx context.Context, path string) ([]int, error) {
	if runtime.GOOS == "linux" {
		return linuxWorkspaceProcessIDs(path)
	}
	return lsofWorkspaceProcessIDs(ctx, path)
}

func linuxWorkspaceProcessIDs(path string) ([]int, error) {
	root := filepath.Clean(path)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	seen := map[int]struct{}{}
	pids := []int{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if !pathInside(root, cwd) {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}

func lsofWorkspaceProcessIDs(ctx context.Context, path string) ([]int, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-t", "+D", path)
	output, err := cmd.Output()
	if err != nil && len(output) == 0 {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	seen := map[int]struct{}{}
	pids := []int{}
	for line := range strings.SplitSeq(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}

func pathInside(root string, path string) bool {
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
