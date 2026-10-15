package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := managed.trim(t.Context(), toolcache.Policy{MaxBytes: 1000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if report := managed.observe(); report == nil || report.SizeEvictedBytes != 0 || report.RetainedBytes < 2000 {
		t.Fatalf("maintenance result: %+v", report)
	}
	if data, err := os.ReadFile(output); err != nil || len(data) != 2000 {
		t.Fatalf("recent cache changed: bytes=%d err=%v", len(data), err)
	}
	slot, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
	if err != nil || !acquired {
		t.Fatalf("second dispatch: %t %v", acquired, err)
	}
	defer gate.Release(slot)
}
