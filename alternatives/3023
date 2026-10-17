package toolcache

import (
	"context"
	"errors"
	"fmt"
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

func TestRemoveLegacyContextCancellation(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, ".detent", "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cache, "entry")
	if err := os.WriteFile(entry, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := RemoveLegacyContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error = %v, want cancellation", err)
	}
	if content, err := os.ReadFile(entry); err != nil || string(content) != "preserved" {
		t.Fatalf("cancelled cleanup changed entry: %q, %v", content, err)
	}
}

func TestRemoveLegacyReadOnlyDirectories(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode os.FileMode
	}{
		{"writable", 0755},
		{"read-only module cache", 0555},
		{"read-only without search permission", 0444},
		{"search-only", 0111},
		{"no permissions", 0000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, ".detent", "cache")
			module := filepath.Join(cache, "project", "go-mod", "example.com", "module@v1.0.0")
			nested := filepath.Join(module, "pkg")
			if err := os.MkdirAll(nested, 0755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, dir := range []string{module, nested} {
					if err := os.Chmod(dir, 0755); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
				}
			})
			for _, dir := range []string{nested, module} {
				if err := os.WriteFile(filepath.Join(dir, "entry.go"), []byte("123"), 0444); err != nil {
					t.Fatal(err)
				}
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

// Inject the ownership-related chmod failure without requiring another uid.
func TestRemoveLegacyChmodFailure(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"removable despite chmod failure", 0700, false},
		{"read-only subtree", 0500, true},
		{"inaccessible subtree", 0000, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, ".detent", "cache")
			blocked := filepath.Join(cache, "a-blocked")
			for _, name := range []string{"a-blocked", "z-removable"} {
				dir := filepath.Join(cache, name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("123"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := os.Chmod(blocked, 0700); err != nil && !os.IsNotExist(err) {
					t.Error(err)
				}
			})
			if err := os.Chmod(blocked, tt.mode); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				if err := os.Remove(filepath.Join(blocked, "entry")); err == nil {
					t.Skip("filesystem does not enforce directory permissions")
				} else if !os.IsPermission(err) {
					t.Fatal(err)
				}
			}
			calls := 0
			reclaimed, _, err := removeLegacy(t.Context(), root, func(workspace *os.Root, path string, mode os.FileMode) error {
				if path == ".detent/cache/a-blocked" {
					calls++
					return &os.PathError{Op: "chmod", Path: path, Err: os.ErrPermission}
				}
				return workspace.Chmod(path, mode)
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %t", err, tt.wantErr)
			}
			if calls != 1 {
				t.Fatalf("chmod failures = %d, want 1", calls)
			}
			if _, err := os.Stat(filepath.Join(cache, "z-removable")); !os.IsNotExist(err) {
				t.Fatalf("removable sibling remains: %v", err)
			}
			if tt.wantErr {
				if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("error = %v, want permission error", err)
				}
				if _, err := os.Stat(blocked); err != nil {
					t.Fatalf("blocked subtree missing: %v", err)
				}
				if reclaimed != 0 {
					t.Fatalf("reported complete reclamation after partial failure: %d", reclaimed)
				}
			} else if reclaimed != 6 {
				t.Fatalf("reclaimed = %d, want 6", reclaimed)
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
		{"explicit", Policy{MaxAge: time.Hour, MaxBytes: 123}, Policy{MaxAge: 25 * time.Hour, MaxBytes: 123}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.input.NormalizedForCapacity(0); got != tt.want {
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

func TestTrimSizeTarget(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name                   string
		cap                    int64
		keepRecent, keepNewest bool
		reclaimed              int64
		repeat                 bool
		marker                 string
		managed                bool
	}{
		{"age only below cap", 100, true, true, 10, false, "", false},
		{"exact cap", 24, true, true, 10, false, "", false},
		{"recent retained above target", 14, true, true, 10, false, "", false},
		{"fresh entries and metadata retained", 1, true, true, 10, false, "", false},
		{"replacement marker boundary", 70, true, true, 10, true, "", false},
		{"counter width boundary", 61, true, true, 10, true, "", false},
		{"oversized previous marker", 70, true, true, 10, true, strings.Repeat("x", 1000), false},
		{"managed oldest first", 64, false, true, 20, true, "", true},
		{"managed metadata budget", 61, false, false, 30, true, "", true},
		{"managed oversized previous marker", 70, false, true, 20, true, strings.Repeat("x", 1000), true},
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
			if tt.marker != "" {
				if err := os.WriteFile(filepath.Join(root, "detent-trim.txt"), []byte(tt.marker), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var report Report
			reclaimed, err := trim(t.Context(), root, Policy{MaxAge: 48 * time.Hour, MaxBytes: tt.cap}, now, &report, tt.managed)
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
			if tt.repeat {
				previous := report.RetainedBytes
				report = Report{}
				if _, err := trim(t.Context(), root, Policy{MaxAge: 48 * time.Hour, MaxBytes: tt.cap}, now, &report, tt.managed); err != nil {
					t.Fatal(err)
				}
				if report.RetainedBytes > previous || report.SizeEvictedBytes != 0 {
					t.Fatalf("replacement changed retained cache: %+v; previously %d", report, previous)
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

func TestTrimExplicitReportedGrowth(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		size                        int64
		age                         time.Duration
		managed, removed, directory bool
	}{
		{"native recent growth", 47 << 10, 5 * time.Hour, false, false, false},
		{"managed recent growth", 47 << 10, 5 * time.Hour, true, true, false},
		{"managed recent directory", 47 << 10, 5 * time.Hour, true, true, true},
		{"below budget", 1 << 10, 5 * time.Hour, true, false, false},
		{"expired below budget", 1 << 10, 49 * time.Hour, true, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0700); err != nil {
				t.Fatal(err)
			}
			for cycle := range 3 {
				_, release, err := cache.Use(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "00", fmt.Sprintf("%064x-d", cycle))
				dataPath := path
				if tt.directory {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					dataPath = filepath.Join(path, "output")
				}
				writeErr := os.WriteFile(dataPath, make([]byte, tt.size), 0600)
				releaseErr := release()
				if err := errors.Join(writeErr, releaseErr); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				at := now.Add(-tt.age)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
				policy := Policy{MaxBytes: 20 << 10}
				var report Report
				if tt.managed {
					report, err = cache.Maintain(t.Context(), policy, now)
				} else {
					report, err = TrimWithReport(t.Context(), root, policy, now)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = os.Stat(path)
				if os.IsNotExist(err) != tt.removed {
					t.Fatalf("cycle %d: stat = %v; removed = %t", cycle, err, tt.removed)
				}
				if tt.removed && report.AgeExpiredBytes+report.SizeEvictedBytes != tt.size {
					t.Fatalf("cycle %d: report = %+v", cycle, report)
				}
				if tt.managed && report.RetainedBytes > policy.MaxBytes {
					t.Fatalf("cycle %d: over budget: %+v", cycle, report)
				}
				if tt.managed && tt.removed && tt.age < 25*time.Hour && report.SizeEvictedBytes != tt.size {
					t.Fatalf("recent entries not size evicted: %+v", report)
				}
				retained, err := Size(root)
				if err != nil || report.RetainedBytes != retained {
					t.Fatalf("retained bytes=%d report=%+v err=%v", retained, report, err)
				}
				stored := ReadTrimReport(root)
				if stored.Error != "" || stored.RetainedBytes != retained || stored.SizeEvictedBytes != report.SizeEvictedBytes || stored.AgeExpiredBytes != report.AgeExpiredBytes {
					t.Fatalf("persisted report=%+v, sweep=%+v", stored, report)
				}
				owner, err := os.ReadFile(filepath.Join(root, ".detent-owner"))
				if err != nil || strings.TrimSpace(string(owner)) != filepath.Join(root, "runtime.db") {
					t.Fatalf("cache ownership changed: %q, %v", owner, err)
				}
				t.Logf("cycle=%d retained_bytes=%d size_evicted_bytes=%d age_expired_bytes=%d", cycle, report.RetainedBytes, report.SizeEvictedBytes, report.AgeExpiredBytes)
			}
		})
	}
}

func TestPolicyCapacity(t *testing.T) {
	for _, tt := range []struct {
		name           string
		capacity       uint64
		explicit, want int64
	}{
		{"reference volume", 1858 << 30, 0, 40 << 30},
		{"small volume", 256 << 30, 0, int64(uint64(256<<30) / 10)},
		{"unavailable", 0, 0, 20 << 30}, {"explicit", 1858 << 30, 123, 123}, {"explicit unavailable", 0, 123, 123},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := (Policy{MaxBytes: tt.explicit}).NormalizedForCapacity(tt.capacity)
			if got.MaxBytes != tt.want || got.MaxAge != 48*time.Hour {
				t.Fatalf("policy=%+v", got)
			}
		})
	}
	paths, err := Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capacity, _ := CapacityBytes(paths.Build)
	if got, want := (Policy{}).Normalized(), (Policy{}).NormalizedForCapacity(capacity); got != want {
		t.Fatalf("normalized=%+v want %+v", got, want)
	}
}

func TestFormatBytes(t *testing.T) {
	for _, tt := range []struct {
		bytes int64
		want  string
	}{
		{0, "0 bytes"}, {34, "34 bytes"}, {1024, "1.0 KiB"}, {1536 << 10, "1.5 MiB"}, {20 << 30, "20.0 GiB"}, {2 << 40, "2.0 TiB"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatBytes(tt.bytes); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestManagedCacheConsumerLifetime(t *testing.T) {
	root := t.TempDir()
	owner := filepath.Join(root, "runtime.db")
	cache, err := NewManaged(t.Context(), root, owner)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewManaged(t.Context(), root, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManaged(t.Context(), root, filepath.Join(root, "other.db")); err == nil {
		t.Fatal("different cache owner accepted")
	}
	leased, release, err := cache.Use(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if ManagedBuildFromContext(leased) != root {
		t.Fatal("managed cache missing from context")
	}
	native, err := Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := Resolve(leased)
	if err != nil || managed.Build != root || managed.Modules != native.Modules {
		t.Fatalf("managed paths: %+v %v", managed, err)
	}
	_, secondRelease, err := other.Use(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer secondRelease()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := cache.Use(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consumer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "00", fmt.Sprintf("%064x-d", 1))
	if err := os.Mkdir(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	for _, lock := range []string{"admission", "consumers"} {
		t.Run("busy "+lock, func(t *testing.T) {
			if lock == "admission" {
				admission, err := cache.lock(t.Context(), "admission", false)
				if err != nil {
					t.Fatal(err)
				}
				defer admission.Close()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = WithWaitObserver(ctx, func(_ string, waiting bool) {
				if waiting {
					cancel()
				}
			})
			report, err := cache.Maintain(ctx, Policy{MaxBytes: 1024}, time.Now())
			if err != nil || report.Error != "" || report.SizeEvictedBytes != 0 || report.RetainedBytes < 2048 {
				t.Fatalf("busy maintenance did not yield with its retained-size observation: %+v, %v", report, err)
			}
			if data, err := os.ReadFile(entry); err != nil || len(data) != 2048 {
				t.Fatalf("busy maintenance changed protected entry: bytes=%d err=%v", len(data), err)
			}
		})
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	ctx = WithWaitObserver(ctx, func(_ string, waiting bool) {
		if waiting {
			stop()
		}
	})
	_, thirdRelease, err := other.Use(other.WithContext(ctx))
	if err != nil {
		t.Fatalf("busy maintenance retained admission: %v", err)
	}
	if err := errors.Join(thirdRelease(), secondRelease(), release()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Maintain(canceled, Policy{MaxBytes: 1024}, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled maintenance: %v", err)
	}
	report, err := cache.Maintain(ctx, Policy{MaxBytes: 1024}, time.Now())
	if err != nil || report.RetainedBytes > 1024 || report.SizeEvictedBytes != 2048 {
		t.Fatalf("idle maintenance: %+v, %v", report, err)
	}
	_, finalRelease, err := UseFromContext(cache.WithContext(ctx))
	if err != nil {
		t.Fatalf("settled maintenance retained locks: %v", err)
	}
	if err := finalRelease(); err != nil {
		t.Fatal(err)
	}
}
