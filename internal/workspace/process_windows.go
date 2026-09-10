package workspace

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func workspaceProcessIDs(ctx context.Context, root string) ([]int, error) {
	canonical, err := canonicalExistingPath(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if filepath.Dir(canonical) == canonical {
		return nil, errors.New("workspace path must not resolve to a filesystem root")
	}
	root = canonical
	current, err := process.NewProcessWithContext(ctx, int32(os.Getpid()))
	if err != nil {
		return nil, err
	}
	owner, err := current.UsernameWithContext(ctx)
	if err != nil {
		return nil, err
	}
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, err
	}
	var owned []int
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pid <= 0 || int(pid) == os.Getpid() {
			continue
		}
		matches, err := windowsScratchProcessMatches(ctx, int(pid), root, owner)
		if err != nil {
			return nil, err
		}
		if matches {
			owned = append(owned, int(pid))
		}
	}
	return owned, nil
}

func windowsScratchProcessMatches(ctx context.Context, pid int, root string, owner string) (bool, error) {
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if errors.Is(err, process.ErrorProcessNotRunning) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	username, err := p.UsernameWithContext(ctx)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner != "" && !strings.EqualFold(username, owner) {
		return false, nil
	}
	environment, err := p.EnvironWithContext(ctx)
	if err != nil {
		return false, scratchProcessInspectionError(ctx, pid, "scratch ownership", err, p.IsRunningWithContext)
	}
	if workerScratchEnvironmentMatches(root, environment) {
		return true, nil
	}
	cwd, err := p.CwdWithContext(ctx)
	if err != nil {
		return false, scratchProcessInspectionError(ctx, pid, "directory", err, p.IsRunningWithContext)
	}
	return filepath.IsAbs(cwd) && pathWithin(root, cwd), nil
}

func reapWorkspaceProcesses(ctx context.Context, path string, logger *slog.Logger) int {
	reaped, err := ReapProcesses(ctx, path, procgroup.DefaultTerminationGrace)
	if err != nil && logger != nil {
		logger.Warn("workspace process reap failed", "path", path, "error", err)
	}
	return reaped
}

func ReapProcesses(ctx context.Context, path string, grace time.Duration) (int, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) || filepath.Dir(path) == path {
		return 0, errors.New("workspace path must be absolute and must not be a filesystem root")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if grace <= 0 {
		grace = procgroup.DefaultTerminationGrace
	}
	ctx, cancel := context.WithTimeout(ctx, max(2*grace, 5*time.Second))
	defer cancel()
	reaped := 0
	for {
		pids, err := scanOwnedWorkspaceProcessIDs(ctx, path, workspaceProcessIDs)
		if err != nil || len(pids) == 0 {
			return reaped, err
		}
		for _, pid := range pids {
			if err := terminateWindowsScratchProcess(ctx, pid, path, grace); err != nil {
				return reaped, err
			}
			reaped++
		}
	}
}

func terminateWindowsScratchProcess(ctx context.Context, pid int, root string, grace time.Duration) error {
	owned, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer owned.Release()
	identity, err := procgroup.Inspect(&exec.Cmd{Process: owned})
	if errors.Is(err, procgroup.ErrProcessNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	matches, err := windowsScratchProcessMatches(ctx, pid, root, "")
	if err != nil || !matches {
		return err
	}
	_, err = procgroup.Terminate(ctx, identity, grace)
	return err
}
