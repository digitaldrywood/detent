//go:build unix

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultProcessTerminationGrace = 250 * time.Millisecond

func reapWorkspaceProcesses(ctx context.Context, path string, logger *slog.Logger) int {
	reaped, err := ReapProcesses(ctx, path, defaultProcessTerminationGrace)
	if err != nil {
		if logger != nil {
			logger.Warn("workspace process reap failed", slog.String("path", path), slog.Any("error", err))
		}
	}
	return reaped
}

func ReapProcesses(ctx context.Context, path string, grace time.Duration) (int, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, errors.New("workspace path is required")
	}
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("workspace path must be absolute: %q", path)
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) == path {
		return 0, fmt.Errorf("workspace path must not be a filesystem root: %q", path)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if grace <= 0 {
		grace = defaultProcessTerminationGrace
	}
	return reapProcesses(ctx, path, grace, workspaceProcessIDs, syscall.Kill)
}

type workspaceProcessScanner func(context.Context, string) ([]int, error)
type workspaceProcessSignaler func(int, syscall.Signal) error
type workspaceProcessWaiter func(context.Context, string, time.Duration, workspaceProcessScanner) ([]int, error)

func reapProcesses(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan workspaceProcessScanner,
	signal workspaceProcessSignaler,
) (int, error) {
	return reapProcessesWithWait(ctx, path, grace, scan, signal, waitForWorkspaceProcesses)
}

func reapProcessesWithWait(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan workspaceProcessScanner,
	signal workspaceProcessSignaler,
	wait workspaceProcessWaiter,
) (int, error) {
	pids, err := scanOwnedWorkspaceProcessIDs(ctx, path, scan)
	if err != nil {
		return 0, fmt.Errorf("scan workspace processes: %w", err)
	}
	if len(pids) == 0 {
		return 0, nil
	}

	reaped := make(map[int]struct{}, len(pids))
	termErr := signalWorkspaceProcesses(pids, syscall.SIGTERM, signal, reaped)
	survivors, err := wait(ctx, path, grace, scan)
	if err != nil {
		return len(reaped), errors.Join(termErr, err)
	}
	if len(survivors) == 0 {
		return len(reaped), termErr
	}

	killErr := signalWorkspaceProcesses(survivors, syscall.SIGKILL, signal, reaped)
	survivors, waitErr := wait(ctx, path, grace, scan)
	if waitErr != nil {
		return len(reaped), errors.Join(termErr, killErr, waitErr)
	}
	if len(survivors) > 0 {
		return len(reaped), errors.Join(
			termErr,
			killErr,
			fmt.Errorf("workspace processes remained after SIGKILL: pids=%v", survivors),
		)
	}
	return len(reaped), errors.Join(termErr, killErr)
}

func scanOwnedWorkspaceProcessIDs(ctx context.Context, path string, scan workspaceProcessScanner) ([]int, error) {
	pids, err := scan(ctx, path)
	if err != nil {
		return nil, err
	}
	owned := make([]int, 0, len(pids))
	seen := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		owned = append(owned, pid)
	}
	return owned, nil
}

func signalWorkspaceProcesses(
	pids []int,
	sig syscall.Signal,
	signal workspaceProcessSignaler,
	reaped map[int]struct{},
) error {
	var result error
	for _, pid := range pids {
		if err := signal(pid, sig); errors.Is(err, syscall.ESRCH) {
			continue
		} else if err != nil {
			result = errors.Join(result, fmt.Errorf("signal workspace process %d with %s: %w", pid, sig, err))
			continue
		}
		reaped[pid] = struct{}{}
	}
	return result
}

func waitForWorkspaceProcesses(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan workspaceProcessScanner,
) ([]int, error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := scanOwnedWorkspaceProcessIDs(ctx, path, scan)
		if err != nil {
			return nil, fmt.Errorf("verify workspace processes: %w", err)
		}
		if len(pids) == 0 {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return pids, ctx.Err()
		case <-timer.C:
			return pids, nil
		case <-ticker.C:
		}
	}
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
	cmd := exec.CommandContext(ctx, "lsof", "-t", "+D", path) // #nosec G204 -- the workspace path is passed as an lsof argument without a shell.
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
