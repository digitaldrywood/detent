package toolcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"
)

// Policy selects safe build artifact expiry and a reported cache size target.
type Policy struct {
	MaxAge   time.Duration `yaml:"max_age"`
	MaxBytes int64         `yaml:"max_bytes,omitempty"`
}

func (p Policy) Normalized() Policy {
	if p.MaxBytes != 0 {
		return p.NormalizedForCapacity(0)
	}
	// Path discovery is host-wide and independently bounded by hostPaths.
	paths, err := hostPaths()
	if err != nil {
		return p.NormalizedForCapacity(0)
	}
	capacity, err := CapacityBytes(paths.Build)
	if err != nil {
		return p.NormalizedForCapacity(0)
	}
	return p.NormalizedForCapacity(capacity)
}

// NormalizedForCapacity uses a tenth of total volume capacity, at most 40 GiB; zero selects the fallback.
func (p Policy) NormalizedForCapacity(capacity uint64) Policy {
	if p.MaxAge == 0 {
		p.MaxAge = 48 * time.Hour
	}
	p.MaxAge = max(p.MaxAge, 25*time.Hour)
	if p.MaxBytes == 0 {
		p.MaxBytes = min(int64(capacity/10), 40<<30)
		if p.MaxBytes == 0 {
			p.MaxBytes = 20 << 30
		}
	}
	return p
}

var cacheEntryName = regexp.MustCompile(`^[0-9a-f]{64}-[ad]$`)
var cacheShardName = regexp.MustCompile(`^[0-9a-f]{2}$`)

// Trim expires old entries without evicting recent outputs to meet MaxBytes.
// Metadata is counted toward the size but is never removed.
func Trim(ctx context.Context, root string, policy Policy, now time.Time) (int64, error) {
	return trim(ctx, root, policy, now, nil)
}

// TrimWithReport reuses the trim walk to report retained build-cache bytes.
// It never inspects the module cache. Concurrent builds can change the size
// during or after the walk, so the report is an estimate.
func TrimWithReport(ctx context.Context, root string, policy Policy, now time.Time) (Report, error) {
	report := Report{BuildPath: root}
	_, err := trim(ctx, root, policy, now, &report)
	if err != nil {
		report.Error = err.Error()
	}
	return report, err
}

func trim(ctx context.Context, root string, policy Policy, now time.Time, report *Report) (reclaimed int64, err error) {
	capacity, capacityErr := CapacityBytes(root)
	if capacityErr != nil {
		capacity = 0
	}
	policy = policy.NormalizedForCapacity(capacity)
	if report == nil {
		report = &Report{}
	}
	if root == "" || root == "off" {
		return 0, nil
	}
	root = filepath.Clean(root)
	cache, err := os.OpenRoot(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, cache.Close()) }()
	marked := false
	for _, name := range []string{"README", "trim.txt"} {
		if info, err := cache.Lstat(name); err == nil && info.Mode().IsRegular() {
			marked = true
			break
		}
	}
	if !marked {
		slog.Warn("skip host Go cache trim: cache marker absent", "path", root)
		return 0, nil
	}
	var total int64
	defer func() { report.BuildBytes = total }()
	err = fs.WalkDir(cache.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() && name != "." {
			if path.Dir(name) != "." || !cacheShardName.MatchString(entry.Name()) {
				return fs.SkipDir
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
		if path.Dir(path.Dir(name)) != "." || !cacheShardName.MatchString(path.Base(path.Dir(name))) || !cacheEntryName.MatchString(entry.Name()) {
			return nil
		}
		if !info.ModTime().Before(now.Add(-policy.MaxAge)) {
			return nil
		}
		// Recheck immediately before removal in case a concurrent Go build touched it.
		current, err := cache.Lstat(name)
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
			return nil
		}
		if err := cache.Remove(name); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		report.AgeExpiredBytes += current.Size()
		reclaimed += current.Size()
		total -= info.Size()
		return nil
	})
	if err != nil {
		return reclaimed, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	marker := "detent-trim.txt"
	if info, err := cache.Lstat(marker); err == nil && info.Mode().IsRegular() {
		total -= info.Size()
	}
	// Fixed-width retained bytes keep metadata size independent of its value.
	metadata := func(retained int64) string {
		return fmt.Sprintf("%s\n%d %d %020d\n", stamp, report.AgeExpiredBytes, report.SizeEvictedBytes, retained)
	}
	data := metadata(total + int64(len(metadata(0))))
	err = cache.WriteFile(marker, []byte(data), 0o600)
	if err == nil {
		total += int64(len(data))
		report.LastTrim = stamp
		report.RetainedBytes = total
	}
	return reclaimed, err
}
