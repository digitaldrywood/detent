package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

var ErrWorkspacePreserved = errors.New("workspace retained for recovery")

type Preservation struct {
	Path            string   `json:"path"`
	Branch          string   `json:"branch,omitempty"`
	Preserved       bool     `json:"preserved"`
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
	result.UnpushedCommits = recovery.UnpushedCommits
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
	if recovery.UnpushedCommits > 0 || len(recovery.TrackedPaths) > 0 || len(recovery.UntrackedPaths) > 0 {
		return fmt.Errorf("%w at %s: unpushed commits or uncommitted files remain", ErrWorkspacePreserved, info.Path)
	}
	return nil
}
