package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestRunnerBuildCacheMaintenanceResumesDispatch(t *testing.T) {
	for _, tt := range []struct {
		name           string
		maxBytes       int64
		freeAfter      uint64
		separateVolume bool
		wantEvicted    int64
		wantAdmission  bool
	}{
		{name: "over budget", maxBytes: 1000, freeAfter: 2500, wantEvicted: 2000, wantAdmission: true},
		{name: "within budget below reserve", maxBytes: 4000, freeAfter: 2500, wantEvicted: 2000, wantAdmission: true},
		{name: "insufficient reclaimable cache", maxBytes: 4000, freeAfter: 700, wantEvicted: 2000},
		{name: "separate workspace filesystem", maxBytes: 4000, separateVolume: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := cache.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			other, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, secondRelease, err := other.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer secondRelease()
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0o700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "00", strings.Repeat("a", 64)+"-d")
			if err := os.WriteFile(output, make([]byte, 2000), 0o600); err != nil {
				t.Fatal(err)
			}
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(output, pin); err != nil {
				t.Fatal(err)
			}
			warm := filepath.Join(root, "00", strings.Repeat("d", 64)+"-d")
			if err := os.WriteFile(warm, []byte("warm output"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(warm, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			warmPin := filepath.Join(t.TempDir(), "active-warm-output")
			if err := os.Link(warm, warmPin); err != nil {
				t.Fatal(err)
			}
			candidate := scheduler.ProjectCandidate{ID: "example-project"}
			gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 2}}}, []scheduler.ProjectCandidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			first, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("first dispatch: %t %v", acquired, err)
			}
			defer gate.Release(first)
			native := t.TempDir()
			if err := os.WriteFile(filepath.Join(native, "trim.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(native, "00"), 0o700); err != nil {
				t.Fatal(err)
			}
			staleNative := filepath.Join(native, "00", strings.Repeat("b", 64)+"-d")
			freshNative := filepath.Join(native, "00", strings.Repeat("c", 64)+"-d")
			for _, entry := range []string{staleNative, freshNative} {
				if err := os.WriteFile(entry, make([]byte, 100), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(staleNative, time.Now().Add(-10*24*time.Hour), time.Now().Add(-10*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			managed := &runnerBuildCache{cache: cache, root: root, native: native}
			workspace := root
			if tt.separateVolume {
				workspace = filepath.Join(t.TempDir(), "workspace")
			}
			volume := func(path string) (uint64, uint64, error) {
				if path != root && path != workspace {
					t.Fatalf("observed unrelated filesystem: %s", path)
				}
				if tt.separateVolume {
					if path == root {
						return 2500, 10000, nil
					}
					return 500, 10000, nil
				}
				if _, err := os.Stat(output); err == nil {
					return 500, 10000, nil
				}
				return tt.freeAfter, 10000, nil
			}
			storageConstraint := func() string {
				return runnerStorageConstraintForPaths(globalconfig.Config{WorkspaceRoot: workspace}, toolcache.Paths{Build: root}, volume)
			}
			if got := storageConstraint(); !strings.Contains(got, "reserve") {
				t.Fatalf("low free disk did not constrain capacity: %q", got)
			}
			for _, active := range []int{2, 1, 0} {
				err := managed.trimForVolume(t.Context(), toolcache.Policy{MaxBytes: tt.maxBytes}, time.Now(), volume)
				if active > 0 {
					if err != nil {
						t.Fatalf("maintenance with %d active consumers did not yield: %v", active, err)
					}
					if report := managed.observe(); report == nil || report.Deferred || report.SizeEvictedBytes != 0 {
						t.Fatalf("busy maintenance did not retain pinned output: %#v", report)
					}
					if data, err := os.ReadFile(output); err != nil || len(data) != 2000 {
						t.Fatalf("active cache changed: bytes=%d err=%v", len(data), err)
					}
					if got := storageConstraint(); !strings.Contains(got, "reserve") {
						t.Fatalf("active cache pressure lost its reserve constraint: %q", got)
					}
					if active == 2 {
						if err := release(); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := secondRelease(); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(pin); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(warmPin); err != nil {
							t.Fatal(err)
						}
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if report := managed.observe(); report == nil || report.Deferred || report.SizeEvictedBytes != tt.wantEvicted || report.RetainedBytes > tt.maxBytes {
					t.Fatalf("maintenance result: %+v, want evicted=%d", report, tt.wantEvicted)
				}
				if _, err := os.Stat(output); os.IsNotExist(err) != (tt.wantEvicted != 0) {
					t.Fatalf("cache reclamation: stat=%v, want evicted=%d", err, tt.wantEvicted)
				}
				if _, err := os.Stat(staleNative); !os.IsNotExist(err) {
					t.Fatalf("stale native Go cache entry retained: %v", err)
				}
				if _, err := os.Stat(freshNative); err != nil {
					t.Fatalf("fresh native Go cache entry removed: %v", err)
				}
			}
			if got := storageConstraint(); (got == "") != tt.wantAdmission {
				t.Fatalf("admission after reclamation: %q, want available=%v", got, tt.wantAdmission)
			}
			if !tt.wantAdmission {
				return
			}
			_, nextRelease, err := cache.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer nextRelease()
			if data, err := os.ReadFile(warm); err != nil || string(data) != "warm output" {
				t.Fatalf("retained output not reusable: %q %v", data, err)
			}
			slot, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("second dispatch: %t %v", acquired, err)
			}
			defer gate.Release(slot)
		})
	}
}
