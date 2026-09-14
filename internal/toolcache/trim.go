package toolcache

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Policy bounds unused build artifacts. Recent entries are always retained.
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

// Trim expires old entries. Recent entries can exceed MaxBytes: the age
// protection takes precedence over the size target.
func Trim(ctx context.Context, root string, policy Policy, now time.Time) (int64, error) {
	policy = policy.Normalized()
	if root == "" || root == "off" {
		return 0, nil
	}
	var reclaimed int64
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
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), "-a") && !strings.HasSuffix(entry.Name(), "-d") {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.ModTime().Before(now.Add(-policy.MaxAge)) {
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
		if !current.Mode().IsRegular() || !current.ModTime().Before(now.Add(-policy.MaxAge)) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		reclaimed += current.Size()
		return nil
	})
	return reclaimed, err
}
