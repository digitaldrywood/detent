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
	"slices"
	"strings"
	"time"
)

type Policy struct {
	MaxAge       time.Duration `yaml:"max_age"`
	MaxBytes     int64         `yaml:"max_bytes,omitempty"`
	reclaimBytes int64
}

func (p Policy) WithReclaimBytes(bytes int64) Policy {
	p.reclaimBytes = max(0, bytes)
	return p
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

func Trim(ctx context.Context, root string, policy Policy, now time.Time) (int64, error) {
	return trim(ctx, root, policy, now, nil, false)
}

// TrimWithReport reuses the trim walk to report retained build-cache bytes.
// It never inspects the module cache. Concurrent builds can change the size
// during or after the walk, so the report is an estimate.
func TrimWithReport(ctx context.Context, root string, policy Policy, now time.Time) (Report, error) {
	report := Report{BuildPath: root}
	_, err := trim(ctx, root, policy, now, &report, false)
	if err != nil {
		report.Error = err.Error()
	}
	return report, err
}

type trimEntry struct {
	name      string
	bytes     int64
	modified  time.Time
	directory bool
}

func trim(ctx context.Context, root string, policy Policy, now time.Time, report *Report, managed bool) (reclaimed int64, err error) {
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
	if !marked && !managed {
		slog.Warn("skip host Go cache trim: cache marker absent", "path", root)
		return 0, nil
	}
	var total int64
	defer func() { report.BuildBytes = total }()
	var candidates []trimEntry
	err = fs.WalkDir(cache.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		candidate := path.Dir(path.Dir(name)) == "." && cacheShardName.MatchString(path.Base(path.Dir(name))) && cacheEntryName.MatchString(entry.Name())
		if entry.IsDir() {
			if name == "." || path.Dir(name) == "." && cacheShardName.MatchString(entry.Name()) {
				return nil
			}
			if !candidate || !strings.HasSuffix(entry.Name(), "-d") {
				return fs.SkipDir
			}
		} else if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		bytes := info.Size()
		if entry.IsDir() {
			bytes, err = sizeFS(ctx, cache.FS(), name)
			if err != nil {
				return err
			}
		}
		total += bytes
		if candidate && (managed || info.ModTime().Before(now.Add(-policy.MaxAge))) {
			candidates = append(candidates, trimEntry{name: name, bytes: bytes, modified: info.ModTime(), directory: entry.IsDir()})
		}
		if entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return reclaimed, err
	}
	report.RetainedBytes = total
	if managed {
		if policy.reclaimBytes > 0 {
			retained := total
			for _, candidate := range candidates {
				if candidate.modified.Before(now.Add(-policy.MaxAge)) {
					retained -= candidate.bytes
				}
			}
			policy.MaxBytes = min(policy.MaxBytes, max(1, retained-policy.reclaimBytes))
		}
		slices.SortFunc(candidates, func(a, b trimEntry) int { return a.modified.Compare(b.modified) })
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	marker := "detent-trim.txt"
	var previousMarkerBytes int64
	if info, err := cache.Lstat(marker); err == nil && info.Mode().IsRegular() {
		previousMarkerBytes = info.Size()
	}
	metadata := func(retained int64) string {
		return fmt.Sprintf("%s\n%d %d %020d\n", stamp, report.AgeExpiredBytes, report.SizeEvictedBytes, retained)
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}
		aged := candidate.modified.Before(now.Add(-policy.MaxAge))
		if !aged && (!managed || total-previousMarkerBytes+int64(len(metadata(0))) <= policy.MaxBytes) {
			continue
		}
		current, err := cache.Lstat(candidate.name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return reclaimed, err
		}
		if current.IsDir() != candidate.directory || !current.IsDir() && !current.Mode().IsRegular() || !current.ModTime().Equal(candidate.modified) {
			continue
		}
		if candidate.directory {
			err = cache.RemoveAll(candidate.name)
		} else {
			err = cache.Remove(candidate.name)
		}
		if err != nil && !os.IsNotExist(err) {
			return reclaimed, err
		}
		if aged {
			report.AgeExpiredBytes += candidate.bytes
		} else {
			report.SizeEvictedBytes += candidate.bytes
		}
		reclaimed += candidate.bytes
		total -= candidate.bytes
		report.RetainedBytes = total
	}
	total -= previousMarkerBytes
	data := metadata(total + int64(len(metadata(0))))
	err = cache.WriteFile(marker, []byte(data), 0o600)
	if err == nil {
		total += int64(len(data))
		report.LastTrim = stamp
		report.RetainedBytes = total
	}
	return reclaimed, err
}
