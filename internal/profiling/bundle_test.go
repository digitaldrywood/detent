package profiling

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"
)

func TestCaptureBundle(t *testing.T) {
	for _, mode := range []string{"complete", "canceled", "cpu already active", "unwritable directory"} {
		t.Run(mode, func(t *testing.T) {
			cfg := DefaultCapture()
			cfg.Dir = filepath.Join(t.TempDir(), "profiles")
			cfg.CPUDuration = time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			if mode == "cpu already active" {
				if err := pprof.StartCPUProfile(io.Discard); err != nil {
					t.Fatal(err)
				}
				defer pprof.StopCPUProfile()
			}
			if mode == "unwritable directory" {
				if err := os.WriteFile(cfg.Dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			err := captureBundle(ctx, cfg, now)
			if (err != nil) != (mode != "complete") {
				t.Fatalf("captureBundle() = %v", err)
			}
			if mode == "unwritable directory" {
				return
			}
			entries, err := os.ReadDir(cfg.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "complete" {
				if len(entries) != 0 {
					t.Fatalf("failed capture left partial bundles: %v", entries)
				}
				return
			}
			if len(entries) != 1 || entries[0].Name() != now.UTC().Format(bundleTimeFormat) {
				t.Fatalf("bundle entries = %v", entries)
			}
			for _, name := range []string{"cpu", "heap", "allocs", "goroutine", "mutex", "block"} {
				path := filepath.Join(cfg.Dir, entries[0].Name(), name+".pprof")
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := gzip.NewReader(file)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				if err := errors.Join(err, reader.Close(), file.Close()); err != nil || len(data) == 0 {
					t.Fatalf("profile %s: bytes=%d error=%v", name, len(data), err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("profile permissions: info=%v error=%v", info, err)
				}
			}
		})
	}
}

func TestPruneBundles(t *testing.T) {
	for _, tt := range []struct {
		name string
		age  time.Duration
		cap  Bytes
		keep int
	}{
		{"age", 90 * time.Minute, 100, 2},
		{"size", 24 * time.Hour, 25, 2},
		{"age and size", 90 * time.Minute, 10, 1},
		{"oversized newest", 24 * time.Hour, 9, 0},
		{"all retained", 24 * time.Hour, 30, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Capture{Dir: t.TempDir(), MaxAge: tt.age, MaxBytes: tt.cap}
			now := time.Now().UTC()
			var paths []string
			for i := range 3 {
				path := filepath.Join(cfg.Dir, now.Add(time.Duration(i-2)*time.Hour).Format(bundleTimeFormat))
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "cpu.pprof"), make([]byte, 10), 0o600); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, path)
			}
			unrelated := filepath.Join(cfg.Dir, "operator-notes")
			partial := filepath.Join(cfg.Dir, ".capture-interrupted")
			if err := os.Mkdir(partial, 0o700); err != nil {
				t.Fatal(err)
			}
			past := now.Add(-48 * time.Hour)
			if err := os.Chtimes(partial, past, past); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(unrelated, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			link := filepath.Join(cfg.Dir, now.Add(-48*time.Hour).Format(bundleTimeFormat))
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if err := pruneBundles(t.Context(), cfg, now); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(partial); !os.IsNotExist(err) {
				t.Fatalf("interrupted capture escaped retention: %v", err)
			}
			for i, path := range paths {
				_, err := os.Stat(path)
				if exists := err == nil; exists != (i >= 3-tt.keep) {
					t.Fatalf("bundle %d exists=%v error=%v", i, exists, err)
				}
			}
			for _, path := range []string{unrelated, link, outside} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("unrelated path pruned: %v", err)
				}
			}
		})
	}
}
