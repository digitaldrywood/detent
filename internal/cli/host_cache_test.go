package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/scheduler"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestStatusHostCacheReport(t *testing.T) {
	for _, tt := range []struct {
		name   string
		report toolcache.Report
		want   string
	}{
		{"sizes", toolcache.Report{BuildPath: "/build", BuildBytes: 12, ModulePath: "/modules", ModuleBytes: 34}, "GOCACHE=/build (12 bytes); GOMODCACHE=/modules (34 bytes)"},
		{"unavailable", toolcache.Report{Error: "no Go toolchain"}, "no Go toolchain"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := statusServiceRunner{ServiceRunner: &serviceRunnerStub{status: servicepkg.Status{}}, inspectCaches: func(context.Context) toolcache.Report { return tt.report }}
			status, err := runner.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.HostCache == nil || *status.HostCache != tt.report {
				t.Fatalf("report=%+v", status.HostCache)
			}
			var out bytes.Buffer
			if err := writeServiceStatusText(&out, status); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("output=%s", out.String())
			}
		})
	}
}

func TestRunnerBuildCacheMaintenanceResumesDispatch(t *testing.T) {
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
	managed := &runnerBuildCache{cache: cache, root: root}
	storageConstraint := func() string {
		return runnerStorageConstraintForPaths(globalconfig.Config{WorkspaceRoot: root}, toolcache.Paths{Build: root}, func(string) (uint64, uint64, error) {
			if _, err := os.Stat(output); err == nil {
				return 35 << 30, 1 << 40, nil
			}
			return 73 << 30, 1 << 40, nil
		})
	}
	if got := storageConstraint(); !strings.Contains(got, "reserve") {
		t.Fatalf("low free disk did not constrain capacity: %q", got)
	}
	for _, active := range []int{2, 1, 0} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err := managed.trim(ctx, toolcache.Policy{MaxBytes: 1000}, time.Now())
		cancel()
		if active > 0 {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("maintenance with %d active consumers: %v", active, err)
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
			} else if err := secondRelease(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if report := managed.observe(); report == nil || report.SizeEvictedBytes != 2000 || report.RetainedBytes > 1000 {
			t.Fatalf("maintenance result: %+v", report)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("recent over-budget cache retained: %v", err)
		}
	}
	if got := storageConstraint(); got != "" {
		t.Fatalf("capacity did not recover after reclamation: %q", got)
	}
	_, nextRelease, err := cache.Use(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	slot, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
	if err != nil || !acquired {
		t.Fatalf("second dispatch: %t %v", acquired, err)
	}
	defer gate.Release(slot)
}
