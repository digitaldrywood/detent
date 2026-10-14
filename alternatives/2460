package profiling

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"time"
)

const bundleTimeFormat = "20060102T150405.000000000Z"

var profileNames = []string{"heap", "allocs", "goroutine", "mutex", "block"}

func writeBundle(ctx context.Context, config CaptureConfig, at time.Time) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(config.Dir, 0o700); err != nil {
		return err
	}
	name := at.UTC().Format(bundleTimeFormat)
	partial := filepath.Join(config.Dir, name+".partial")
	if err := os.Mkdir(partial, 0o700); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			result = errors.Join(result, os.RemoveAll(partial))
		}
	}()
	if err := writeCPU(ctx, filepath.Join(partial, "cpu.pprof"), config.CPUDuration); err != nil {
		return err
	}
	for _, name := range profileNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(partial, name+".pprof"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		err = pprof.Lookup(name).WriteTo(file, 0)
		if err = errors.Join(err, file.Close()); err != nil {
			return fmt.Errorf("write %s profile: %w", name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(partial, filepath.Join(config.Dir, name)); err != nil {
		return err
	}
	published = true
	return nil
}

func writeCPU(ctx context.Context, path string, duration time.Duration) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		return errors.Join(err, file.Close())
	}
	timer := time.NewTimer(duration)
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	timer.Stop()
	pprof.StopCPUProfile()
	return errors.Join(ctx.Err(), file.Close())
}

type bundle struct {
	path string
	at   time.Time
	size int64
}

func pruneBundles(dir string, maxAge time.Duration, maxBytes int64, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var bundles []bundle
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".partial")
		at, err := time.Parse(bundleTimeFormat, name)
		if err != nil || at.Format(bundleTimeFormat) != name {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if now.Sub(at) > maxAge {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}
		var size int64
		if err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			return nil
		}); err != nil {
			return err
		}
		bundles = append(bundles, bundle{path: path, at: at, size: size})
		total += size
	}
	slices.SortFunc(bundles, func(a, b bundle) int { return a.at.Compare(b.at) })
	for _, bundle := range bundles {
		if total <= maxBytes {
			break
		}
		if err := os.RemoveAll(bundle.path); err != nil {
			return err
		}
		total -= bundle.size
	}
	return nil
}
