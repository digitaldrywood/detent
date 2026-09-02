package runner

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	pathRemovalAttempts         = 4
	pathRemovalRetryDelay       = 10 * time.Millisecond
	admissionWorkspacePathLimit = 20
)

type admissionWorkspace struct {
	logger    *slog.Logger
	leaks     *admissionWorkspaceLeakTracker
	path      string
	removeAll func(string) error
	wait      func(context.Context, time.Duration) error
}

type admissionWorkspaceLeakTracker struct {
	mu    sync.Mutex
	paths map[string]struct{}
}

type admissionWorkspaceLeakSnapshot struct {
	count int
	bytes int64
}

func (w *admissionWorkspace) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	if err := ctx.Err(); err != nil {
		return workspace.Info{}, err
	}
	path, err := os.MkdirTemp("", "detent-admission-")
	if err != nil {
		return workspace.Info{}, err
	}
	w.path = path
	return workspace.Info{
		Path:    path,
		Key:     strings.TrimSpace(issue.ID),
		Created: true,
	}, nil
}

func (w *admissionWorkspace) Cleanup(ctx context.Context, path string) error {
	if w.path == "" || filepath.Clean(path) != filepath.Clean(w.path) {
		return errors.New("backlog admission workspace cleanup path does not match the created workspace")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	removeAll := w.removeAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	wait := w.wait
	if wait == nil {
		wait = waitForPathRemovalRetry
	}
	if _, err := removePathWithRetry(ctx, w.path, removeAll, wait); err != nil {
		return err
	}
	if w.leaks != nil {
		w.leaks.remove(w.path)
	}
	w.path = ""
	return nil
}

func removePathWithRetry(
	ctx context.Context,
	path string,
	removeAll func(string) error,
	wait func(context.Context, time.Duration) error,
) (int, error) {
	var err error
	for attempt := range pathRemovalAttempts {
		err = removeAll(path)
		if err == nil {
			return attempt + 1, nil
		}
		if !errors.Is(err, fs.ErrExist) || attempt == pathRemovalAttempts-1 {
			return attempt + 1, err
		}
		if waitErr := wait(ctx, pathRemovalRetryDelay<<attempt); waitErr != nil {
			return attempt + 1, errors.Join(err, waitErr)
		}
	}
	return pathRemovalAttempts, err
}

func waitForPathRemovalRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (*admissionWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (w *admissionWorkspace) AfterRun(ctx context.Context, info workspace.Info, _ workspace.Issue) {
	if err := w.Cleanup(ctx, info.Path); err != nil && w.logger != nil {
		tracker := w.leaks
		if tracker == nil {
			tracker = &admissionWorkspaceLeakTracker{}
		}
		snapshot := tracker.record(info.Path)
		w.logger.Warn(
			"remove backlog admission workspace failed after retries",
			"workspace_path", info.Path,
			"leaked_workspace_count", snapshot.count,
			"leaked_workspace_bytes", snapshot.bytes,
			"surviving_paths", survivingAdmissionWorkspacePaths(info.Path, admissionWorkspacePathLimit),
			"error", err,
		)
	}
}

func (t *admissionWorkspaceLeakTracker) record(path string) admissionWorkspaceLeakSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.paths == nil {
		t.paths = make(map[string]struct{})
	}
	t.paths[path] = struct{}{}
	snapshot := admissionWorkspaceLeakSnapshot{}
	for leakedPath := range t.paths {
		bytes, err := admissionWorkspaceSize(leakedPath)
		if errors.Is(err, fs.ErrNotExist) {
			delete(t.paths, leakedPath)
			continue
		}
		snapshot.count++
		snapshot.bytes += bytes
	}
	return snapshot
}

func (t *admissionWorkspaceLeakTracker) remove(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.paths, path)
}

func admissionWorkspaceSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func survivingAdmissionWorkspacePaths(path string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	paths := make([]string, 0, min(limit, 8))
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if len(paths) >= limit {
			return fs.SkipAll
		}
		if walkErr != nil {
			paths = append(paths, current+": "+walkErr.Error())
			return nil
		}
		if current == path || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			relative = current
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil && len(paths) < limit {
		paths = append(paths, path+": "+err.Error())
	}
	return paths
}

func (*admissionWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}
