package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RetentionRequest supplies authoritative tracker and process ownership evidence.
// Missing or failed evidence never authorizes deleting a workspace or scratch.
type RetentionRequest struct {
	Now          time.Time
	Active       []Issue
	Completed    func(context.Context, []Issue) (map[string]time.Time, error)
	ScratchState func(context.Context, string) (registered, terminal bool, err error)
}

type RemovalTotal struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

type RetentionTotals struct {
	Workdir    string       `json:"workdir"`
	At         time.Time    `json:"at"`
	Workspaces RemovalTotal `json:"workspaces"`
	Quarantine RemovalTotal `json:"quarantine"`
	HookLogs   RemovalTotal `json:"hook_logs"`
	Attempts   RemovalTotal `json:"attempts"`
	Ownership  RemovalTotal `json:"ownership"`
}

type RetentionSweeper interface {
	SweepRetention(context.Context, RetentionRequest) (RetentionTotals, error)
}

func (l *LocalGit) SweepRetention(ctx context.Context, request RetentionRequest) (RetentionTotals, error) {
	totals := RetentionTotals{Workdir: l.root, At: request.Now}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return totals, err
	}
	defer l.closeRetentionRoot(root)
	if err := retentionDirectory(root, ".detent"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return totals, err
	}
	var failures []error
	recordError := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	active := map[string]bool{}
	for _, issue := range request.Active {
		active[issueKey(issue)] = true
	}
	entries, err := os.ReadDir(filepath.Join(l.root, cleanupOwnershipRegistryDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		recordError(err)
	}
	var records []cleanupOwnershipRecord
	var issues []Issue
	for _, entry := range entries {
		relative := filepath.Join(cleanupOwnershipRegistryDir, entry.Name())
		record, err := l.readOwnershipRecord(relative)
		if err != nil || !l.validOwnershipRecord(ctx, relative, record) {
			continue
		}
		if _, err := root.Lstat(record.Key); errors.Is(err, fs.ErrNotExist) {
			recordError(removeRetentionPath(root, relative, &totals.Ownership))
			continue
		} else if err != nil {
			recordError(err)
			continue
		}
		if active[record.Key] {
			continue
		}
		records = append(records, record)
		issues = append(issues, Issue{ProjectID: record.ProjectID, ID: record.IssueID, Identifier: record.Identifier})
	}
	if len(issues) > 0 && request.Completed != nil {
		completed, err := request.Completed(ctx, issues)
		recordError(err)
		if err == nil {
			for _, record := range records {
				since, ok := completed[record.IssueID]
				if !ok || since.IsZero() || request.Now.Before(since.Add(7*24*time.Hour)) {
					continue
				}
				pids, err := scanOwnedWorkspaceProcessIDs(ctx, record.Path, l.scanWorkspacePaths)
				if err != nil {
					recordError(err)
					continue
				}
				if len(pids) > 0 {
					continue
				}
				recordError(l.removeExpiredWorkspace(ctx, root, record, &totals.Workspaces, &totals.Ownership))
			}
		}
	}
	recordError(l.sweepQuarantine(ctx, root, request.Now, &totals.Quarantine))
	recordError(sweepHookLogs(root, request.Now, &totals.HookLogs))
	recordError(l.sweepAttempts(ctx, root, request, &totals.Attempts))
	l.logger.Info("workspace retention sweep", "workdir", l.root, "totals", totals, "errors", len(failures))
	return totals, errors.Join(failures...)
}

func (l *LocalGit) closeRetentionRoot(root *os.Root) {
	if err := root.Close(); err != nil {
		l.logger.Warn("close retention root", "error", err)
	}
}

func retentionBytes(root *os.Root, path string) (int64, error) {
	var size int64
	err := fs.WalkDir(root.FS(), filepath.ToSlash(path), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func removeRetentionPath(root *os.Root, path string, total *RemovalTotal) error {
	size, err := retentionBytes(root, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := root.RemoveAll(path); err != nil {
		return err
	}
	total.Count++
	total.Bytes += size
	return nil
}

func makeQuarantineWritable(root *os.Root, path string) error {
	return fs.WalkDir(root.FS(), filepath.ToSlash(path), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o700 == 0o700 {
			return nil
		}
		return root.Chmod(name, info.Mode().Perm()|0o700)
	})
}

func (l *LocalGit) sweepQuarantine(ctx context.Context, root *os.Root, now time.Time, total *RemovalTotal) error {
	const parent = ".detent/quarantine"
	if err := retentionDirectory(root, parent); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := fs.ReadDir(root.FS(), parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		name string
		at   time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// The rename preserves directory mtime; the quarantine name records entry time.
		name := entry.Name()
		for i := range name {
			if name[i] != '-' || len(name[i+1:]) < len(quarantineTimestampFormat) {
				continue
			}
			at, err := time.Parse(quarantineTimestampFormat, name[i+1:i+1+len(quarantineTimestampFormat)])
			if err == nil {
				candidates = append(candidates, candidate{name, at})
				break
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].name > candidates[j].name
		}
		return candidates[i].at.After(candidates[j].at)
	})
	var failures []error
	for i, candidate := range candidates {
		if i < 5 && now.Before(candidate.at.Add(3*24*time.Hour)) {
			continue
		}
		relative := filepath.Join(parent, candidate.name)
		path := filepath.Join(l.root, relative)
		pids, err := scanOwnedWorkspaceProcessIDs(ctx, path, l.scanWorkspacePaths)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if len(pids) > 0 {
			continue
		}
		if err := makeQuarantineWritable(root, relative); err != nil {
			failures = append(failures, &os.PathError{Op: "prepare quarantine", Path: relative, Err: err})
			continue
		}
		if err := removeRetentionPath(root, relative, total); err != nil {
			failures = append(failures, err)
		}
	}
	if total.Count > 0 {
		if _, err := l.runGit(ctx, "worktree", "prune"); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func sweepHookLogs(root *os.Root, now time.Time, total *RemovalTotal) error {
	if err := retentionDirectory(root, ".detent/hook-logs"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	err := fs.WalkDir(root.FS(), ".detent/hook-logs", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if now.Before(info.ModTime().Add(14 * 24 * time.Hour)) {
			return nil
		}
		return removeRetentionPath(root, path, total)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (l *LocalGit) sweepAttempts(ctx context.Context, root *os.Root, request RetentionRequest, total *RemovalTotal) error {
	if request.ScratchState == nil {
		return nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".detent" {
			continue
		}
		parent := filepath.Join(entry.Name(), workerScratchRelativePath)
		if err := retentionDirectory(root, parent); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, err)
			}
			continue
		}
		attempts, err := fs.ReadDir(root.FS(), filepath.ToSlash(parent))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, attempt := range attempts {
			if !attempt.IsDir() || !strings.HasPrefix(attempt.Name(), "attempt-") {
				continue
			}
			path := filepath.Join(parent, attempt.Name())
			registered, terminal, err := request.ScratchState(ctx, filepath.Join(l.root, path))
			if err != nil {
				failures = append(failures, err)
				continue
			}
			info, err := attempt.Info()
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if !terminal && (registered || request.Now.Before(info.ModTime().Add(time.Hour))) {
				continue
			}
			pids, err := scanOwnedWorkspaceProcessIDs(ctx, filepath.Join(l.root, path), l.scanWorkspacePaths)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if len(pids) > 0 {
				continue
			}
			if err := removeRetentionPath(root, path, total); err != nil {
				failures = append(failures, fmt.Errorf("remove scratch %s: %w", path, err))
			}
		}
	}
	return errors.Join(failures...)
}

// Artifact parents must be real directories: os.Root also permits symlinks
// within its root, which could otherwise redirect retention into user files.
func retentionDirectory(root *os.Root, path string) error {
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("retention directory is not a directory: %s", current)
		}
	}
	return nil
}
