package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrimSharedCache(t *testing.T) {
	for _, tt := range []struct {
		name               string
		active             bool
		age                time.Duration
		budget             int64
		wantBuild, wantMod int64
	}{
		{"active over budget", true, time.Hour, 5, 4, 4},
		{"active expired", true, 48 * time.Hour, 20, 0, 4},
		{"inactive expired", false, 48 * time.Hour, 20, 0, 0},
		{"inactive recent", false, time.Hour, 20, 8, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Now()
			cache := SharedCacheRoot(root, "project")
			for i, name := range []string{"go-build/00/old-d", "go-build/01/new-d", "go-mod/example/mod.go"} {
				path := filepath.Join(cache, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-tt.age).Add(time.Duration(i) * time.Second)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return os.Chtimes(path, now.Add(-tt.age), now.Add(-tt.age))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got := trimSharedCache(t.Context(), root, "project", 24*time.Hour, tt.active, tt.budget, now)
			if got.Error != "" || got.BuildBytes != tt.wantBuild || got.ModuleBytes != tt.wantMod {
				t.Fatalf("usage = %+v", got)
			}
		})
	}
}

func TestInspectSharedCache(t *testing.T) {
	for _, name := range []string{"missing", "cancelled", "symlink"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ctx := t.Context()
			if name == "symlink" {
				cache := SharedCacheRoot(root, "project")
				if err := os.MkdirAll(cache, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(cache, "go-build")); err != nil {
					t.Skip(err)
				}
			}
			if name == "cancelled" {
				if err := os.MkdirAll(SharedCacheRoot(root, "project"), 0o700); err != nil {
					t.Fatal(err)
				}
				var cancel func()
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got := InspectSharedCache(ctx, root, "project")
			if (got.Error != "") != (name != "missing") {
				t.Fatalf("usage = %+v", got)
			}
		})
	}
}
