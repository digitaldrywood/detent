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
func (l *LocalGit) removeExpiredWorkspace(ctx context.Context, root *os.Root, record cleanupOwnershipRecord, total, ownership *RemovalTotal) error {
	release, ok := l.uses.cleanup(record.Path)
	if !ok {
		return nil
	}
	defer release()
	if !takeCleanupSlot(ctx) {
		return nil
	}
	relativePath, err := filepath.Rel(l.root, record.Path)
	if err != nil {
		return err
	}
	size, err := retentionBytes(root, relativePath)
	if err != nil {
		return err
	}
	if !l.isSourceWorktree(ctx, record.Path) {
		entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(relativePath))
		if err != nil {
			return err
		}
		if l.isGitWorkspace(ctx, record.Path) || len(entries) != 1 || entries[0].Name() != ".detent" || !entries[0].IsDir() {
			return fmt.Errorf("cannot remove unmanaged workspace %s", record.Path)
		}
	}
	if err := l.removePath(ctx, record.Path); err != nil {
		return err
	}
	total.Count++
	total.Bytes += size
	return removeRetentionPath(root, cleanupOwnershipRecordRelativePath(record.Path), ownership)
}
