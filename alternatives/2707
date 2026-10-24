package profiling

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"
)

func TestWriteBundle(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	for _, name := range []string{"complete", "canceled", "cancel during CPU", "CPU busy", "unwritable directory"} {
		t.Run(name, func(t *testing.T) {
			config := Default().Capture
			config.Dir = filepath.Join(t.TempDir(), "profiles")
			config.CPUDuration = time.Millisecond
			ctx := t.Context()
			switch name {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "cancel during CPU":
				config.CPUDuration = time.Hour
				canceled, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				ctx = canceled
			case "CPU busy":
				file, err := os.Create(filepath.Join(t.TempDir(), "busy.pprof"))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := pprof.StartCPUProfile(file); err != nil {
					t.Fatal(err)
				}
				defer pprof.StopCPUProfile()
			case "unwritable directory":
				if err := os.WriteFile(config.Dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			at := time.Now()
			err := writeBundle(ctx, config, at)
			if name != "complete" {
				if err == nil {
					t.Fatal("failed capture succeeded")
				}
				if name != "unwritable directory" {
					entries, readErr := os.ReadDir(config.Dir)
					if !errors.Is(readErr, os.ErrNotExist) && readErr != nil {
						t.Fatal(readErr)
					}
					if len(entries) != 0 {
						t.Fatalf("incomplete bundle retained: %v", entries)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(config.Dir, at.UTC().Format(bundleTimeFormat))
			entries, err := os.ReadDir(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 6 {
				t.Fatalf("got %d profiles, want 6", len(entries))
			}
			for _, profile := range append([]string{"cpu"}, profileNames...) {
				file, err := os.Open(filepath.Join(bundle, profile+".pprof"))
				if err != nil {
					t.Fatal(err)
				}
				info, err := file.Stat()
				if err != nil {
					t.Fatal(err)
				}
				// Windows exposes the read-only attribute rather than Unix owner
				// permissions. Profile creation/content remain tested on every OS.
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Fatalf("profile permissions: %v", info.Mode())
				}
				reader, err := gzip.NewReader(file)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				if err != nil || len(data) == 0 {
					t.Fatalf("invalid %s profile: %v", profile, err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRetention(t *testing.T) {
	for _, test := range []struct {
		name      string
		age       time.Duration
		bytes     int64
		remaining int
	}{
		{"age", 2 * time.Hour, 100, 2}, {"size", 24 * time.Hour, 20, 2},
		{"combined", 2 * time.Hour, 10, 1}, {"oversized bundle", 24 * time.Hour, 5, 0},
		{"within bounds", 24 * time.Hour, 30, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now().UTC()
			for index := 3; index > 0; index-- {
				name := now.Add(-time.Duration(index) * time.Hour).Format(bundleTimeFormat)
				if index == 3 {
					name += ".partial"
				}
				path := filepath.Join(dir, name)
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "heap.pprof"), make([]byte, 10), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			unrelated := filepath.Join(dir, "operator-notes")
			if err := os.Mkdir(unrelated, 0o700); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			link := filepath.Join(dir, now.Add(-100*time.Hour).Format(bundleTimeFormat))
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := pruneBundles(dir, test.age, test.bytes, now); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != test.remaining+2 {
				t.Fatalf("retained %d entries, want %d", len(entries), test.remaining+2)
			}
			for _, path := range []string{unrelated, link, target} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("unowned path removed: %s: %v", path, err)
				}
			}
			for index := 1; index <= test.remaining; index++ {
				name := now.Add(-time.Duration(index) * time.Hour).Format(bundleTimeFormat)
				if index == 3 {
					name += ".partial"
				}
				path := filepath.Join(dir, name)
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("newest bundle missing: %v", err)
				}
			}
		})
	}
	if err := pruneBundles(filepath.Join(t.TempDir(), "missing"), time.Hour, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
}
