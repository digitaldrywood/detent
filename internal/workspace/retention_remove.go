package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Completed work is published to the Hub before its item reaches a terminal
// state, so an expired workspace is removed without a local copy.
func (l *LocalGit) removeExpiredWorkspace(ctx context.Context, root *os.Root, record cleanupOwnershipRecord, total, ownership *RemovalTotal) (bool, error) {
	release, ok := l.uses.TryCleanup(record.Path)
	if !ok {
		return false, nil
	}
	defer release()
	return l.removeExpiredWorkspaceLocked(ctx, root, record, total, ownership)
}

func (l *LocalGit) removeExpiredWorkspaceLocked(ctx context.Context, root *os.Root, record cleanupOwnershipRecord, total, ownership *RemovalTotal) (bool, error) {
	if !takeCleanupSlot(ctx) {
		return false, nil
	}
	relativePath, err := filepath.Rel(l.root, record.Path)
	if err != nil {
		return false, err
	}
	size, err := retentionBytes(root, relativePath)
	if err != nil {
		return false, err
	}
	if !l.isSourceWorktree(ctx, record.Path) {
		entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(relativePath))
		if err != nil {
			return false, err
		}
		if l.isGitWorkspace(ctx, record.Path) || len(entries) != 1 || entries[0].Name() != ".detent" || !entries[0].IsDir() {
			return false, fmt.Errorf("cannot remove unmanaged workspace %s", record.Path)
		}
	}
	if err := l.removePath(ctx, record.Path); err != nil {
		return false, err
	}
	total.Count++
	total.Bytes += size
	return true, removeRetentionPath(root, cleanupOwnershipRecordRelativePath(record.Path), ownership)
}
