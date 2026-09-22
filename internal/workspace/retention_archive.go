package workspace

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (l *LocalGit) removeExpiredWorkspace(ctx context.Context, root *os.Root, record cleanupOwnershipRecord, total, ownership *RemovalTotal) (returnErr error) {
	release, ok := l.uses.cleanup(record.Path)
	if !ok {
		return nil
	}
	defer release()
	if !takeCleanupSlot(ctx) {
		return nil
	}
	if !l.isSourceWorktree(ctx, record.Path) {
		return fmt.Errorf("cannot archive unmanaged workspace %s", record.Path)
	}
	size, err := retentionBytes(root, record.Key)
	if err != nil {
		return err
	}
	head, err := runGitAt(ctx, record.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := root.MkdirAll(".detent/retained", 0o700); err != nil {
		return err
	}
	if err := retentionDirectory(root, ".detent/retained"); err != nil {
		return err
	}
	archive, err := os.MkdirTemp(filepath.Join(l.root, ".detent/retained"), record.Key+"-")
	if err != nil {
		return err
	}
	temporaryArchive := archive
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(temporaryArchive)) }()
	unmerged, err := runGitAt(ctx, record.Path, "ls-files", "--unmerged")
	if err != nil {
		return err
	}
	if unmerged != "" {
		return fmt.Errorf("cannot archive unresolved index in %s", record.Path)
	}
	bundle := filepath.Join(archive, "commits.bundle")
	if _, err := runGitAt(ctx, record.Path, "bundle", "create", bundle, "HEAD"); err != nil {
		return fmt.Errorf("archive commits: %w", err)
	}
	if _, err := runGitAt(ctx, record.Path, "bundle", "verify", bundle); err != nil {
		return fmt.Errorf("verify archived commits: %w", err)
	}
	diff, err := runGitAt(ctx, record.Path, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD")
	if err != nil {
		return err
	}
	staged, err := runGitAt(ctx, record.Path, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--binary", "HEAD")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(archive, "staged.diff"), []byte(staged), 0o600); err != nil {
		return err
	}
	diffPath := filepath.Join(archive, "working-tree.diff")
	if err := os.WriteFile(diffPath, []byte(diff), 0o600); err != nil {
		return err
	}
	if err := archiveWorkingTree(record.Path, filepath.Join(archive, "working-tree.tar.gz")); err != nil {
		return err
	}
	for _, name := range []string{"commits.bundle", "working-tree.diff", "staged.diff"} {
		file, err := os.OpenFile(filepath.Join(archive, name), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	archive, err = publishRetentionArchive(root, archive, record.Key)
	if err != nil {
		return err
	}
	bundle = filepath.Join(archive, "commits.bundle")
	diffPath = filepath.Join(archive, "working-tree.diff")
	archiveRelative, err := filepath.Rel(l.root, archive)
	if err != nil {
		return err
	}
	archivedBytes, err := retentionBytes(root, archiveRelative)
	if err != nil {
		return err
	}
	// The bundle owns the complete HEAD ancestry; the archive also includes
	// untracked and ignored files, symlinks and the current working-tree contents.
	l.logger.Info("archived expired workspace", "path", record.Path, "head_sha", strings.TrimSpace(head), "bundle_path", bundle, "diff_path", diffPath, "archive_path", archive)
	if err := l.removePath(ctx, record.Path); err != nil {
		return err
	}
	total.Count++
	total.Bytes += max(0, size-archivedBytes)
	return removeRetentionPath(root, cleanupOwnershipRecordRelativePath(record.Path), ownership)
}

func archiveWorkingTree(path, destination string) (returnErr error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	walkErr := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if name == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = root.Readlink(name)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = name
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		source, err := root.Open(name)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, source)
		return errors.Join(copyErr, source.Close())
	})
	return errors.Join(walkErr, writer.Close(), compressed.Close(), file.Sync())
}

// Publish by content so failed removals cannot accumulate identical recovery
// copies. Keep distinct snapshots: removal may have partially deleted the source,
// in which case an earlier archive still owns files absent from the next snapshot.
func publishRetentionArchive(root *os.Root, temporary, key string) (string, error) {
	digest, err := retentionArchiveDigest(temporary)
	if err != nil {
		return "", err
	}
	relative := filepath.Join(".detent/retained", key+"-"+digest)
	destination := filepath.Join(filepath.Dir(temporary), key+"-"+digest)
	if _, err := root.Lstat(relative); err == nil {
		if err := retentionDirectory(root, relative); err != nil {
			return "", err
		}
		existing, err := retentionArchiveDigest(destination)
		if err != nil {
			return "", err
		}
		if existing != digest {
			return "", fmt.Errorf("retention archive content mismatch: %s", destination)
		}
		return destination, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func retentionArchiveDigest(path string) (string, error) {
	digest := sha256.New()
	for _, name := range []string{"commits.bundle", "working-tree.diff", "staged.diff", "working-tree.tar.gz"} {
		path := filepath.Join(path, name)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("not a regular archive file: %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		content := sha256.New()
		_, copyErr := io.Copy(content, file)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return "", err
		}
		// Each file contributes exactly one SHA-256 digest in fixed name order.
		digest.Write(content.Sum(nil))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
