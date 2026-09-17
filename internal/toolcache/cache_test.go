package toolcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrim(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name   string
		age    time.Duration
		file   string
		remove bool
	}{
		{"old data", 49 * time.Hour, "00/" + strings.Repeat("0", 64) + "-d", true},
		{"old action", 49 * time.Hour, "00/" + strings.Repeat("0", 64) + "-a", true},
		{"recent below size cap", time.Hour, "00/" + strings.Repeat("0", 64) + "-d", false},
		{"boundary protected", 48 * time.Hour, "00/" + strings.Repeat("0", 64) + "-a", false},
		{"trim metadata", 72 * time.Hour, "trim.txt", false},
		{"readme", 72 * time.Hour, "README", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, tt.file)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("cache entry"), 0600); err != nil {
				t.Fatal(err)
			}
			at := now.Add(-tt.age)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			_, err := Trim(context.Background(), root, Policy{MaxAge: 48 * time.Hour, MaxBytes: 100}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(path)
			if os.IsNotExist(err) != tt.remove {
				t.Fatalf("stat = %v, remove = %t", err, tt.remove)
			}
		})
	}
}

func TestRemoveLegacy(t *testing.T) {
	for _, tt := range []struct {
		name           string
		present, tilde bool
	}{{"absent", false, false}, {"present", true, false}, {"tilde", true, true}} {
		t.Run(tt.name, func(t *testing.T) {
			present := tt.present
			root := t.TempDir()
			cache := filepath.Join(root, ".detent", "cache")
			if present {
				if err := os.MkdirAll(cache, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cache, "entry"), []byte("123"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			configured := root
			if tt.tilde {
				t.Setenv("HOME", filepath.Dir(root))
				t.Setenv("USERPROFILE", filepath.Dir(root))
				configured = "~/" + filepath.Base(root)
			}
			size, resolved, err := RemoveLegacy(configured)
			if resolved != root {
				t.Fatalf("resolved = %q, want %q", resolved, root)
			}
			if err != nil {
				t.Fatal(err)
			}
			if present && size != 3 {
				t.Fatalf("reclaimed %d", size)
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("cache remains: %v", err)
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoveLegacyReadOnlyDirectories(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode os.FileMode
	}{
		{"writable", 0755},
		{"read-only module cache", 0555},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, ".detent", "cache")
			module := filepath.Join(cache, "project", "go-mod", "example.com", "module@v1.0.0")
			nested := filepath.Join(module, "pkg")
			if err := os.MkdirAll(nested, 0755); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{module, nested} {
				if err := os.WriteFile(filepath.Join(dir, "entry.go"), []byte("123"), 0444); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0755); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
				})
				if err := os.Chmod(dir, tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			reclaimed, _, err := RemoveLegacy(root)
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed != 6 {
				t.Fatalf("reclaimed = %d, want 6", reclaimed)
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("cache remains: %v", err)
			}
		})
	}
}

func TestRemoveLegacySymlinks(t *testing.T) {
	for _, tt := range []struct {
		name, link string
		wantErr    bool
	}{
		{"metadata directory", ".detent", true},
		{"cache directory", ".detent/cache", true},
		{"cache child", ".detent/cache/child", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			workspace := filepath.Join(home, "workspace")
			outside := filepath.Join(home, "unrelated")
			sentinel := filepath.Join(outside, "cache", "keep")
			if err := os.MkdirAll(filepath.Dir(sentinel), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(workspace, tt.link)
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			reclaimed, _, err := RemoveLegacy("~/workspace")
			if data, readErr := os.ReadFile(sentinel); readErr != nil || string(data) != "keep" {
				t.Fatalf("external data changed: %q, %v", data, readErr)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoveLegacy error = %v, want error %t", err, tt.wantErr)
			}
			if reclaimed != 0 {
				t.Fatalf("reclaimed external bytes: %d", reclaimed)
			}
		})
	}
}

func TestPolicyDefaults(t *testing.T) {
	for _, tt := range []struct {
		name        string
		input, want Policy
	}{
		{"defaults", Policy{}, Policy{MaxAge: 48 * time.Hour, MaxBytes: 20 * 1024 * 1024 * 1024}},
		{"explicit", Policy{MaxAge: time.Hour, MaxBytes: 123}, Policy{MaxAge: time.Hour, MaxBytes: 123}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.input.Normalized(); got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

func TestInspect(t *testing.T) {
	for _, tt := range []struct {
		name           string
		build, modules string
	}{
		{"populated", "123", "12345"}, {"empty", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			build := t.TempDir()
			modules := t.TempDir()
			t.Setenv("GOCACHE", build)
			t.Setenv("GOMODCACHE", modules)
			if err := os.WriteFile(filepath.Join(build, "fixture-d"), []byte(tt.build), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(modules, "fixture"), []byte(tt.modules), 0600); err != nil {
				t.Fatal(err)
			}
			got := inspect(context.Background(), resolve)
			if got.Error != "" || got.BuildPath != build || got.ModulePath != modules || got.BuildBytes != int64(len(tt.build)) || got.ModuleBytes != int64(len(tt.modules)) {
				t.Fatalf("report=%+v", got)
			}
		})
	}
}

func TestTrimSizeCap(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name                   string
		cap                    int64
		keepRecent, keepNewest bool
		reclaimed              int64
	}{
		{"age only below cap", 100, true, true, 10},
		{"exact cap", 24, true, true, 10},
		{"oldest recent evicted", 14, false, true, 20},
		{"all entries evicted metadata retained", 1, false, false, 30},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range []struct {
				name string
				age  time.Duration
				data string
			}{
				{"00/" + strings.Repeat("0", 64) + "-d", 49 * time.Hour, "0123456789"},
				{"00/" + strings.Repeat("1", 64) + "-a", 2 * time.Hour, "0123456789"},
				{"00/" + strings.Repeat("2", 64) + "-d", time.Hour, "0123456789"},
				{"README", 72 * time.Hour, "12"},
				{"trim.txt", 72 * time.Hour, "12"},
			} {
				path := filepath.Join(root, f.name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(f.data), 0600); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-f.age)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			reclaimed, err := Trim(context.Background(), root, Policy{MaxAge: 48 * time.Hour, MaxBytes: tt.cap}, now)
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed != tt.reclaimed {
				t.Fatalf("reclaimed %d, want %d", reclaimed, tt.reclaimed)
			}
			for name, keep := range map[string]bool{"00/" + strings.Repeat("0", 64) + "-d": false, "00/" + strings.Repeat("1", 64) + "-a": tt.keepRecent, "00/" + strings.Repeat("2", 64) + "-d": tt.keepNewest, "README": true, "trim.txt": true} {
				_, err := os.Stat(filepath.Join(root, name))
				if keep && err != nil || !keep && !os.IsNotExist(err) {
					t.Errorf("%s: stat=%v, keep=%t", name, err, keep)
				}
			}
		})
	}
}

func TestTrimCandidateLayout(t *testing.T) {
	for _, tt := range []struct {
		name, file, marker string
		remove             bool
	}{
		{"root unrelated", "foo-a", "README", false},
		{"nested unrelated", "00/foo-a", "README", false},
		{"deep hash", "00/nested/" + strings.Repeat("0", 64) + "-a", "README", false},
		{"fuzz hash", "fuzz/" + strings.Repeat("0", 64) + "-a", "README", false},
		{"invalid shard", "zz/" + strings.Repeat("0", 64) + "-a", "README", false},
		{"missing marker", "00/" + strings.Repeat("0", 64) + "-a", "", false},
		{"valid readme", "00/" + strings.Repeat("0", 64) + "-a", "README", true},
		{"valid trim marker", "00/" + strings.Repeat("0", 64) + "-d", "trim.txt", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.marker != "" {
				if err := os.WriteFile(filepath.Join(root, tt.marker), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, tt.file)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("entry"), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			old := now.Add(-72 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			if _, err := Trim(t.Context(), root+string(os.PathSeparator), Policy{MaxBytes: 1}, now); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(path)
			if os.IsNotExist(err) != tt.remove {
				t.Fatalf("stat=%v, remove=%t", err, tt.remove)
			}
		})
	}
}

// Sparse files reproduce the reported 47 GiB growth without consuming that disk space.
func TestTrimDefaultReportedGrowth(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size    int64
		age     time.Duration
		removed bool
	}{
		{"reported recent growth", 47 << 30, 5 * time.Hour, true},
		{"below budget", 19 << 30, 5 * time.Hour, false},
		{"expired below budget", 1 << 20, 49 * time.Hour, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "00", strings.Repeat("0", 64)+"-d")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			truncateErr := f.Truncate(tt.size)
			closeErr := f.Close()
			if truncateErr != nil {
				t.Fatal(truncateErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			now := time.Now()
			at := now.Add(-tt.age)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			reclaimed, err := Trim(t.Context(), root, Policy{}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(path)
			if os.IsNotExist(err) != tt.removed {
				t.Fatalf("stat = %v; removed = %t", err, tt.removed)
			}
			if tt.removed && reclaimed != tt.size {
				t.Fatalf("reclaimed = %d", reclaimed)
			}
		})
	}
}
