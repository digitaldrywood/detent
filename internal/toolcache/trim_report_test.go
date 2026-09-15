package toolcache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrimWithReport(t *testing.T) {
	for _, tt := range []struct {
		name   string
		age    time.Duration
		cap    int64
		cancel bool
	}{
		{"retained", time.Hour, 1000, false},
		{"age eviction", 72 * time.Hour, 1000, false},
		{"size eviction", time.Hour, 1, false},
		{"cancelled", time.Hour, 1000, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			if err := os.Mkdir(filepath.Join(root, "00"), 0700); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(root, "00", strings.Repeat("0", 64)+"-d")
			for _, path := range []string{filepath.Join(root, "README"), entry} {
				if err := os.WriteFile(path, []byte("cache data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(entry, now.Add(-tt.age), now.Add(-tt.age)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			// A second pass verifies accounting when the trim timestamp already exists.
			for range 2 {
				report, err := TrimWithReport(ctx, root, Policy{MaxBytes: tt.cap}, now)
				if tt.cancel {
					if !errors.Is(err, context.Canceled) || report.Error == "" || report.LastTrim != "" {
						t.Fatalf("cancelled trim = %+v, %v", report, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				want, err := Size(root)
				if err != nil {
					t.Fatal(err)
				}
				if report.BuildPath != root || report.BuildBytes != want || report.LastTrim != now.Format(time.RFC3339Nano) {
					t.Fatalf("report = %+v, want %d bytes", report, want)
				}
				raw, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "module_") {
					t.Fatalf("unmeasured module cache published: %s", raw)
				}
			}
		})
	}
}

func TestTrimWithReportSkipsUnrelatedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The trim ignores non-cache directories. A follow-up recursive size scan
	// would count this file and change the result.
	unrelated := filepath.Join(root, "unrelated")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "large"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	report, err := TrimWithReport(t.Context(), root, Policy{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(now.Format(time.RFC3339Nano)) + 1); report.BuildBytes != want {
		t.Fatalf("build bytes = %d, want trim measurement %d", report.BuildBytes, want)
	}
}

func TestTrimWithReportSkipped(t *testing.T) {
	for _, root := range []string{"", "off", t.TempDir()} {
		t.Run(root, func(t *testing.T) {
			report, err := TrimWithReport(t.Context(), root, Policy{}, time.Now())
			if err != nil || report.LastTrim != "" || report.BuildBytes != 0 {
				t.Fatalf("skipped trim = %+v, %v", report, err)
			}
		})
	}
}
