package profiling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"time"
)

const bundleTimeFormat = "20060102T150405.000000000Z"

func captureBundle(ctx context.Context, cfg Capture, now time.Time) (resultErr error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return err
	}
	path, err := os.MkdirTemp(cfg.Dir, ".capture-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(path)) }()
	if err := writeCPU(ctx, filepath.Join(path, "cpu.pprof"), cfg.CPUDuration); err != nil {
		return err
	}
	for _, name := range []string{"heap", "allocs", "goroutine", "mutex", "block"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeProfile(filepath.Join(path, name+".pprof"), name); err != nil {
			return err
		}
	}
	return os.Rename(path, filepath.Join(cfg.Dir, now.UTC().Format(bundleTimeFormat)))
}

func writeCPU(ctx context.Context, path string, duration time.Duration) (resultErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	writer := &captureWriter{writer: file}
	if err := pprof.StartCPUProfile(writer); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	pprof.StopCPUProfile()
	return errors.Join(ctx.Err(), writer.err)
}

type captureWriter struct {
	writer io.Writer
	err    error
}

func (w *captureWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil {
		w.err = errors.Join(w.err, err)
	}
	return n, err
}

func writeProfile(path, name string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return pprof.Lookup(name).WriteTo(file, 0)
}

type bundle struct {
	path string
	at   time.Time
	size int64
}

func pruneBundles(ctx context.Context, cfg Capture, now time.Time) error {
	entries, err := os.ReadDir(cfg.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var bundles []bundle
	var total int64
	for _, entry := range entries {
		at, err := time.Parse(bundleTimeFormat, entry.Name())
		if !entry.IsDir() {
			continue
		}
		if err != nil {
			if !strings.HasPrefix(entry.Name(), ".capture-") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			at = info.ModTime()
		}
		path := filepath.Join(cfg.Dir, entry.Name())
		if now.Sub(at) > cfg.MaxAge {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("prune expired bundle: %w", err)
			}
			continue
		}
		var size int64
		if err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
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
		if total <= int64(cfg.MaxBytes) {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(bundle.path); err != nil {
			return fmt.Errorf("prune oversized bundles: %w", err)
		}
		total -= bundle.size
	}
	return nil
}
