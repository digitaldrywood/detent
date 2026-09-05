package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrWorkspacePreserved = errors.New("workspace retained for recovery")

type Preservation struct {
	Path            string   `json:"path"`
	Branch          string   `json:"branch,omitempty"`
	Preserved       bool     `json:"preserved"`
	Files           int      `json:"files,omitempty"`
	HeadSHA         string   `json:"head_sha,omitempty"`
	UnpushedCommits int      `json:"unpushed_commits,omitempty"`
	TrackedPaths    []string `json:"tracked_paths,omitempty"`
	UntrackedPaths  []string `json:"untracked_paths,omitempty"`
}

type IssuePreserver interface {
	PreserveIssue(context.Context, Issue) (Preservation, error)
}

func (l *LocalGit) PreserveIssue(ctx context.Context, issue Issue) (Preservation, error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return Preservation{}, err
	}
	result := Preservation{Path: info.Path, Branch: info.Branch}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, ErrMissingWorkspace
	}
	if err := l.recordCleanupOwnership(ctx, info, issue, isDir); err != nil {
		return result, err
	}
	record, err := l.readOwnershipRecord(cleanupOwnershipRecordRelativePath(info.Path))
	if err != nil {
		return result, err
	}
	record.Preserve = true
	if err := l.writeOwnershipRecord(record); err != nil {
		return result, fmt.Errorf("retain workspace ownership: %w", err)
	}
	result.Preserved = true
	recovery, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return result, fmt.Errorf("inspect retained workspace: %w", err)
	}
	result.HeadSHA = recovery.HeadSHA
	result.UnpushedCommits, err = retainedGitCommitCount(ctx, info.Path)
	if err != nil {
		return result, err
	}
	result.TrackedPaths = recovery.TrackedPaths
	result.UntrackedPaths = recovery.UntrackedPaths
	return result, nil
}

func (l *LocalGit) checkPreservedWorkspace(ctx context.Context, info Info, issue Issue) error {
	recordPath := cleanupOwnershipRecordRelativePath(info.Path)
	record, err := l.readOwnershipRecord(recordPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workspace retention: %w", err)
	}
	if !record.Preserve {
		return nil
	}
	if !l.validOwnershipRecord(ctx, recordPath, record) {
		return fmt.Errorf("invalid workspace retention record: %s", info.Path)
	}
	if !l.isSourceWorktree(ctx, info.Path) {
		return fmt.Errorf("%w at %s: worktree registration is unavailable", ErrWorkspacePreserved, info.Path)
	}
	recovery, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	unpushed, err := retainedGitCommitCount(ctx, info.Path)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	if unpushed > 0 || len(recovery.TrackedPaths) > 0 || len(recovery.UntrackedPaths) > 0 {
		return fmt.Errorf("%w at %s: unpushed commits or uncommitted files remain", ErrWorkspacePreserved, info.Path)
	}
	return nil
}

func retainedGitCommitCount(ctx context.Context, path string) (int, error) {
	output, err := runGitAt(ctx, path, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return 0, fmt.Errorf("inspect retained commits: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parse retained commit count: %w", err)
	}
	return count, nil
}

func (f *Filesystem) PreserveIssue(ctx context.Context, issue Issue) (Preservation, error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return Preservation{}, err
	}
	result := Preservation{Path: info.Path}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, ErrMissingWorkspace
	}
	if !isDir {
		return result, fmt.Errorf("filesystem workspace is not a directory: %s", info.Path)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return result, err
	}
	defer f.closeRoot("preservation", root)
	if err := root.MkdirAll(filesystemRetentionPath(info), 0o700); err != nil {
		return result, fmt.Errorf("retain filesystem workspace: %w", err)
	}
	result.Preserved = true
	stat, err := f.DiffStat(ctx, info, issue)
	if err != nil {
		return result, fmt.Errorf("inspect retained filesystem workspace: %w", err)
	}
	result.Files = stat.Files
	return result, nil
}

func (f *Filesystem) checkPreservedWorkspace(info Info) error {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return err
	}
	defer f.closeRoot("preservation", root)
	if _, err := root.Lstat(filesystemRetentionPath(info)); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read filesystem workspace retention: %w", err)
	}
	return fmt.Errorf("%w at %s: filesystem output requires explicit disposition", ErrWorkspacePreserved, info.Path)
}

func filesystemRetentionPath(info Info) string {
	return filepath.Join(info.Key, ".detent", "retained")
}
