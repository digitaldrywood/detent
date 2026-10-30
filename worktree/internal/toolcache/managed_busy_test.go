package toolcache

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedCacheBusyTrimsOldestEligible(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cycles  int
		age     time.Duration
		reclaim int64
		old     bool
	}{
		{name: "oldest first", cycles: 1, age: 25 * time.Hour, old: true},
		{name: "oldest first under pressure", cycles: 1, age: 25 * time.Hour, reclaim: 4096, old: true},
		{name: "sustained fresh writes", cycles: 24, age: time.Second},
		{name: "sustained fresh writes under pressure", cycles: 24, age: time.Second, reclaim: 4096},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
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
			active := write(1, tt.age+4*time.Hour)
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(active, pin); err != nil {
				t.Fatal(err)
			}
			newest := write(2, tt.age+time.Hour)
			for cycle := range tt.cycles {
				oldest := write(3+cycle*2, tt.age+3*time.Hour)
				next := write(4+cycle*2, tt.age+2*time.Hour)
				policy := Policy{MaxBytes: 6000}.WithReclaimBytes(tt.reclaim)
				report, err := cache.Maintain(t.Context(), policy, now)
				var wantEvicted int64
				if tt.old {
					wantEvicted = 4096
					if tt.reclaim > 0 && cycle == 0 {
						wantEvicted += 2048
					}
				}
				if err != nil || report.Deferred || report.SizeEvictedBytes != wantEvicted || tt.old && report.RetainedBytes > policy.MaxBytes {
					t.Fatalf("cycle %d: busy cache violated gentle trim policy: %#v, %v", cycle, report, err)
				}
				for _, name := range []string{oldest, next} {
					if !tt.old {
						continue
					}
					if _, err := os.Stat(name); !os.IsNotExist(err) {
						t.Errorf("cycle %d: old entry retained: %v", cycle, err)
					}
				}
				protected := []string{active, pin}
				if tt.reclaim == 0 || !tt.old {
					protected = append(protected, newest)
				}
				if !tt.old {
					protected = append(protected, oldest, next)
					if report.RetainedBytes < int64(4+cycle*2)*2048 {
						t.Fatalf("cycle %d: recent working set lost: %#v", cycle, report)
					}
				}
				for _, name := range protected {
					if data, err := os.ReadFile(name); err != nil || len(data) != 2048 {
						t.Fatalf("cycle %d: protected or newer output lost: bytes=%d err=%v", cycle, len(data), err)
					}
				}
				if stored := ReadTrimReport(root); stored.SizeEvictedBytes != wantEvicted || stored.RetainedBytes != report.RetainedBytes {
					t.Fatalf("cycle %d: persisted report=%#v, observed=%#v", cycle, stored, report)
				}
			}
			if err := os.Remove(pin); err != nil {
				t.Fatal(err)
			}
			report, err := cache.Maintain(t.Context(), Policy{MaxBytes: 4000}.WithReclaimBytes(2048), now)
			wantEvicted := int64(0)
			if tt.old {
				wantEvicted = 2048
			}
			if err != nil || report.SizeEvictedBytes != wantEvicted {
				t.Fatalf("released output trim: %#v, %v", report, err)
			}
			if _, err := os.Stat(active); os.IsNotExist(err) != tt.old || err != nil && !os.IsNotExist(err) {
				t.Fatalf("released output: stat=%v, want removed=%t", err, tt.old)
			}
		})
	}
}
