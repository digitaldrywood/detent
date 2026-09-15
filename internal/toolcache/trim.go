package toolcache

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Policy bounds build artifacts by age and total cache size.
type Policy struct {
	MaxAge   time.Duration `yaml:"max_age"`
	MaxBytes int64         `yaml:"max_bytes"`
}

func (p Policy) Normalized() Policy {
	if p.MaxAge == 0 {
		p.MaxAge = 48 * time.Hour
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = 1000 * 1024 * 1024 * 1024
	}
	return p
}

var cacheEntryName = regexp.MustCompile(`^[0-9a-f]{64}-[ad]$`)
var cacheShardName = regexp.MustCompile(`^[0-9a-f]{2}$`)

// Trim expires old entries, then evicts oldest-first to enforce MaxBytes.
// Metadata is counted toward the size but is never removed.
func Trim(ctx context.Context, root string, policy Policy, now time.Time) (int64, error) {
	policy = policy.Normalized()
	if root == "" || root == "off" {
		return 0, nil
	}
	root = filepath.Clean(root)
	marked := false
	for _, name := range []string{"README", "trim.txt"} {
		if info, err := os.Lstat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
			marked = true
			break
		}
	}
	if !marked {
		slog.Warn("skip host Go cache trim: cache marker absent", "path", root)
		return 0, nil
	}
	type candidate struct {
		path string
		info fs.FileInfo
	}
	var remaining []candidate
	var reclaimed, total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			if filepath.Dir(path) != root || !cacheShardName.MatchString(entry.Name()) {
				return filepath.SkipDir
			}
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		total += info.Size()
		if filepath.Dir(filepath.Dir(path)) != root || !cacheShardName.MatchString(filepath.Base(filepath.Dir(path))) || !cacheEntryName.MatchString(entry.Name()) {
			return nil
		}
		if !info.ModTime().Before(now.Add(-policy.MaxAge)) {
			remaining = append(remaining, candidate{path, info})
			return nil
		}
		// Recheck immediately before removal in case a concurrent Go build touched it.
		current, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() {
			return nil
		}
		if !current.ModTime().Before(now.Add(-policy.MaxAge)) {
			remaining = append(remaining, candidate{path, current})
			return nil
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		reclaimed += current.Size()
		total -= info.Size()
		return nil
	})
	if err != nil {
		return reclaimed, err
	}
	slices.SortFunc(remaining, func(a, b candidate) int {
		if order := a.info.ModTime().Compare(b.info.ModTime()); order != 0 {
			return order
		}
		return strings.Compare(a.path, b.path)
	})
	for _, entry := range remaining {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}
		if total <= policy.MaxBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil {
			if os.IsNotExist(err) {
				total -= entry.info.Size()
				continue
			}
			return reclaimed, err
		}
		reclaimed += entry.info.Size()
		total -= entry.info.Size()
	}
	return reclaimed, os.WriteFile(filepath.Join(root, "detent-trim.txt"), []byte(now.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}
