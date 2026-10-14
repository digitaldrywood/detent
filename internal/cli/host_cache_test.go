package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/store"
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
	for _, failed := range []bool{false, true} {
		t.Run(strconv.FormatBool(failed), func(t *testing.T) {
			root := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
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
			gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			owner := &cacheMaintenanceProcessStore{gate: gate, candidate: candidate}
			if failed {
				owner.err = errors.New("consumer recovery unavailable")
			}
			managed := &runnerBuildCache{cache: cache, root: root, registry: project.NewRegistry(), gate: gate, processes: owner, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			err = managed.trim(t.Context(), toolcache.Policy{MaxBytes: 1000}, time.Now())
			if !errors.Is(err, owner.err) {
				t.Fatalf("maintenance: %v want %v", err, owner.err)
			}
			if !owner.paused {
				t.Fatal("consumer recovery ran without dispatch pause")
			}
			report := managed.observe()
			if report == nil || !failed && (report.SizeEvictedBytes != 2000 || report.RetainedBytes > 1000) {
				t.Fatalf("maintenance result: %+v", report)
			}
			_, statErr := os.Stat(output)
			if failed && statErr != nil || !failed && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("reclamation after failed=%t: %v", failed, statErr)
			}
			slot, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("dispatch did not resume: acquired=%t err=%v", acquired, err)
			}
			if err := gate.Release(slot); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type cacheMaintenanceProcessStore struct {
	gate      *scheduler.PoolRegistry
	candidate scheduler.ProjectCandidate
	paused    bool
	err       error
}

func (s *cacheMaintenanceProcessStore) ListActiveWorkerProcesses(ctx context.Context) ([]store.WorkerProcess, error) {
	_, acquired, decision, err := s.gate.TryAcquireWithDecision(ctx, s.candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
	s.paused = err == nil && !acquired && decision.Reason == scheduler.DispatchGateReasonPaused
	return nil, s.err
}

func (*cacheMaintenanceProcessStore) MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error {
	return nil
}
