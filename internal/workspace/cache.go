package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SharedBuildCacheBudget bounds retained build data between cleanup sweeps.
const SharedBuildCacheBudget int64 = 20 << 30

// CacheUsage describes logical file sizes measured during a cleanup or doctor run.
type CacheUsage struct {
	ProjectID    string    `json:"project_id"`
	Path         string    `json:"path"`
	BuildBytes   int64     `json:"go_build_bytes"`
	ModuleBytes  int64     `json:"go_mod_bytes"`
	BinBytes     int64     `json:"go_bin_bytes"`
	LintBytes    int64     `json:"golangci_lint_bytes"`
	RemovedBytes int64     `json:"removed_bytes"`
	TotalBytes   int64     `json:"total_bytes"`
	BudgetBytes  int64     `json:"go_build_budget_bytes"`
	ObservedAt   time.Time `json:"observed_at"`
	Error        string    `json:"error,omitempty"`
}

func SharedCacheRoot(root, projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "default"
	}
	sum := sha256.Sum256([]byte(projectID))
	key := SafeKey(projectID) + "-" + hex.EncodeToString(sum[:])[:12]
	return filepath.Join(root, ".detent", "cache", key)
}

type cacheFile struct {
	path     string
	size     int64
	modified time.Time
}

// InspectSharedCache measures only the named project's cache, without following symlinks.
func InspectSharedCache(ctx context.Context, root, projectID string) CacheUsage {
	usage, _, _ := scanSharedCache(ctx, root, projectID)
	return usage
}

func scanSharedCache(ctx context.Context, root, projectID string) (usage CacheUsage, files []cacheFile, newest time.Time) {
	usage = CacheUsage{ProjectID: projectID, Path: SharedCacheRoot(root, projectID), BudgetBytes: SharedBuildCacheBudget, ObservedAt: time.Now().UTC()}
	if strings.TrimSpace(root) == "" {
		return usage, nil, newest
	}
	resolved, err := canonicalExistingPath(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			usage.Error = err.Error()
		}
		return usage, nil, newest
	}
	root = resolved
	usage.Path = SharedCacheRoot(root, projectID)
	// Opening the workspace root first prevents traversal through an external cache symlink.
	base, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return usage, nil, newest
	}
	if err != nil {
		usage.Error = err.Error()
		return usage, nil, newest
	}
	defer closeCacheRoot(base, &usage)
	relative, err := filepath.Rel(root, usage.Path)
	if err != nil {
		usage.Error = err.Error()
		return usage, nil, newest
	}
	if err := cacheDirectoryPath(base, relative); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			usage.Error = err.Error()
		}
		return usage, nil, newest
	}
	cache, err := base.OpenRoot(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return usage, nil, newest
	}
	if err != nil {
		usage.Error = err.Error()
		return usage, nil, newest
	}
	defer closeCacheRoot(cache, &usage)
	err = fs.WalkDir(cache.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache contains symlink: %s", path)
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		usage.TotalBytes += size
		switch strings.Split(path, "/")[0] {
		case "go-build":
			usage.BuildBytes += size
		case "go-mod":
			usage.ModuleBytes += size
		case "go-bin":
			usage.BinBytes += size
		case "golangci-lint":
			usage.LintBytes += size
		}
		files = append(files, cacheFile{path: path, size: size, modified: info.ModTime()})
		return nil
	})
	if err != nil {
		usage.Error = err.Error()
	}
	return usage, files, newest
}

// TrimSharedCache extends workspace cleanup to rebuildable shared data. Active
// module and binary caches remain intact; Go build entries can be regenerated.
func TrimSharedCache(ctx context.Context, root, projectID string, idleTTL time.Duration, active bool) CacheUsage {
	return trimSharedCache(ctx, root, projectID, idleTTL, active, SharedBuildCacheBudget, time.Now())
}

func trimSharedCache(ctx context.Context, root, projectID string, idleTTL time.Duration, active bool, budget int64, now time.Time) (result CacheUsage) {
	usage, files, newest := scanSharedCache(ctx, root, projectID)
	if usage.Error != "" || newest.IsZero() {
		return usage
	}
	root, err := canonicalExistingPath(root)
	if err != nil {
		usage.Error = err.Error()
		return usage
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		usage.Error = err.Error()
		return usage
	}
	defer closeCacheRoot(base, &result)
	relative, err := filepath.Rel(root, usage.Path)
	if err != nil {
		usage.Error = err.Error()
		return usage
	}
	if err := cacheDirectoryPath(base, relative); err != nil {
		usage.Error = err.Error()
		return usage
	}
	cache, err := base.OpenRoot(relative)
	if err != nil {
		usage.Error = err.Error()
		return usage
	}
	defer closeCacheRoot(cache, &result)
	expired := func(at time.Time) bool { return idleTTL > 0 && !at.After(now.Add(-idleTTL)) }
	removeAll := !active && expired(newest)
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	buildBytes := usage.BuildBytes
	for _, file := range files {
		if err = ctx.Err(); err != nil {
			break
		}
		component := strings.Split(file.path, "/")[0]
		trim := removeAll || (component == "go-build" && (buildBytes > budget || expired(file.modified))) || (component == "golangci-lint" && expired(file.modified))
		if !trim {
			continue
		}
		info, statErr := cache.Lstat(file.path)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			err = statErr
			break
		}
		if !info.Mode().IsRegular() || info.Size() != file.size || !info.ModTime().Equal(file.modified) {
			continue
		}
		// Module cache directories are read-only. Only inactive full cleanup changes them.
		if removeAll {
			if err = cache.Chmod(filepath.Dir(file.path), 0o700); err != nil {
				break
			}
		}
		err = cache.Remove(file.path)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
			continue
		}
		if err != nil {
			break
		}
		usage.RemovedBytes += file.size
		usage.TotalBytes -= file.size
		switch component {
		case "go-build":
			buildBytes -= file.size
			usage.BuildBytes -= file.size
		case "go-mod":
			usage.ModuleBytes -= file.size
		case "go-bin":
			usage.BinBytes -= file.size
		case "golangci-lint":
			usage.LintBytes -= file.size
		}
	}
	// Remove only empty directories: a concurrent worker may have populated the
	// cache after measurement. Never recursively remove newly written content.
	if err == nil && removeAll {
		var dirs []string
		err = fs.WalkDir(cache.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if err := cache.Chmod(path, 0o700); err != nil {
					return err
				}
				dirs = append(dirs, path)
			}
			return nil
		})
		if err == nil {
			for i := len(dirs) - 1; i >= 0; i-- {
				if dirs[i] != "." {
					// Nonempty directories may contain concurrent writes; retain them.
					if removeErr := cache.Remove(dirs[i]); removeErr != nil && !errors.Is(removeErr, fs.ErrExist) && !errors.Is(removeErr, fs.ErrNotExist) {
						err = removeErr
						break
					}
				}
			}
		}
	}
	// Retain the single scan measurement, less successful removals, even on cancellation.
	result = usage
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// Reject symlink aliases even when they remain inside the workspace root:
// another project's cache is never this project's cleanup target.
func cacheDirectoryPath(root *os.Root, relative string) error {
	path := ""
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		path = filepath.Join(path, component)
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("cache path is not a directory: %s", path)
		}
	}
	return nil
}

func closeCacheRoot(root *os.Root, usage *CacheUsage) {
	if err := root.Close(); err != nil {
		if usage.Error == "" {
			usage.Error = err.Error()
		} else {
			usage.Error += "; " + err.Error()
		}
	}
}
