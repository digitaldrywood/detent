package toolcache

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedCacheBusyEvictsOldestUnpinned(t *testing.T) {
	for _, cycles := range []int{1, 3} {
		t.Run(fmt.Sprintf("cycles=%d", cycles), func(t *testing.T) {
			root := t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := cache.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := release(); err != nil {
					t.Error(err)
				}
			})
			now := time.Now()
			write := func(id int, age time.Duration) string {
				t.Helper()
				name := filepath.Join(root, "00", fmt.Sprintf("%064x-d", id))
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, make([]byte, 2048), 0o600); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-age)
				if err := os.Chtimes(name, at, at); err != nil {
					t.Fatal(err)
				}
				return name
			}
			active := write(1, 4*time.Hour)
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(active, pin); err != nil {
				t.Fatal(err)
			}
			newest := write(2, time.Hour)
			for cycle := range cycles {
				oldest := write(3+cycle*2, 3*time.Hour)
				next := write(4+cycle*2, 2*time.Hour)
				policy := Policy{MaxBytes: 6000}
				report, err := cache.Maintain(t.Context(), policy, now)
				if err != nil || report.Deferred || report.SizeEvictedBytes != 4096 || report.RetainedBytes > policy.MaxBytes {
					t.Fatalf("cycle %d: busy cache did not evict to bound: %#v, %v", cycle, report, err)
				}
				for _, name := range []string{oldest, next} {
					if _, err := os.Stat(name); !os.IsNotExist(err) {
						t.Errorf("cycle %d: old entry retained: %v", cycle, err)
					}
				}
				for _, name := range []string{active, pin, newest} {
					if data, err := os.ReadFile(name); err != nil || len(data) != 2048 {
						t.Fatalf("cycle %d: protected or newer output lost: bytes=%d err=%v", cycle, len(data), err)
					}
				}
				if stored := ReadTrimReport(root); stored.SizeEvictedBytes != 4096 || stored.RetainedBytes != report.RetainedBytes {
					t.Fatalf("cycle %d: persisted report=%#v, observed=%#v", cycle, stored, report)
				}
			}
			if err := os.Remove(pin); err != nil {
				t.Fatal(err)
			}
			report, err := cache.Maintain(t.Context(), Policy{MaxBytes: 4000}, now)
			if err != nil || report.SizeEvictedBytes != 2048 {
				t.Fatalf("released output not reclaimable: %#v, %v", report, err)
			}
			if _, err := os.Stat(active); !os.IsNotExist(err) {
				t.Fatalf("released oldest output retained: %v", err)
			}
		})
	}
}
