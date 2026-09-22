package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestINV12DoctorNativeCaches(t *testing.T) {
	for _, tt := range []struct {
		name, legacy, reportError string
		want                      doctorStatus
	}{
		{"native", "", "", doctorOK},
		{"legacy root", ".detent/cache", "", doctorWarn},
		{"tilde legacy root", ".detent/cache", "", doctorWarn},
		{"legacy attempt", "attempt/.detent/cache", "", doctorWarn},
		{"legacy isolated", "workdir/.detent/worker-tmp/attempt-old/go-build", "", doctorWarn},
		{"legacy isolated modules", "workdir/.detent/worker-tmp/attempt-old/go-mod", "", doctorWarn},
		{"discovery failure", "", "Go unavailable", doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.legacy != "" {
				if err := os.MkdirAll(filepath.Join(root, tt.legacy), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			deps := doctorDeps{cacheCapacityBytes: func(string) (uint64, error) { return 1 << 50, nil }, inspectCaches: func(context.Context) toolcache.Report {
				return toolcache.Report{BuildPath: "/host/build", ModulePath: "/host/modules", BuildBytes: 12, ModuleBytes: 34, LastTrim: "2026-09-14T12:00:00Z", Error: tt.reportError}
			}}
			configured := root
			if tt.name == "tilde legacy root" {
				t.Setenv("HOME", filepath.Dir(root))
				t.Setenv("USERPROFILE", filepath.Dir(root))
				configured = "~/" + filepath.Base(root)
			}
			got := checkDoctorNativeCaches(t.Context(), deps, toolcache.Policy{})
			legacy := checkDoctorLegacyCaches("test", configured)
			if tt.legacy != "" {
				got = legacy
			}
			if got.Status != tt.want {
				t.Fatalf("check = %+v", got)
			}
			if tt.legacy == "" && !strings.Contains(got.Detail, "GOCACHE=/host/build") {
				t.Fatalf("missing host report: %+v", got)
			}

			if tt.legacy != "" {
				if strings.Contains(got.Detail, "no Detent-owned cache roots") {
					t.Fatalf("contradictory detail: %+v", got)
				}
				if !strings.Contains(got.Detail, filepath.Join(root, tt.legacy)) {
					t.Errorf("legacy path missing: %+v", got)
				}
				if _, err := os.Stat(filepath.Join(root, tt.legacy)); err != nil {
					t.Fatalf("doctor modified legacy cache: %v", err)
				}
			}
		})
	}
}

func TestINV12DoctorWorkspaceKinds(t *testing.T) {
	for _, kind := range []string{workflowconfig.WorkspaceLocalGit, workflowconfig.WorkspaceFilesystem} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			cfg := validDoctorWorkflow(root)
			cfg.Workspace.Root, cfg.Workspace.Kind = root, kind
			if kind == workflowconfig.WorkspaceFilesystem {
				cfg.Deliverable.Kind = workflowconfig.DeliverableArtifact
				cfg.Deliverable.OutputRoot = t.TempDir()
				cfg.Workspace.AutoBranch = false
			}
			deps := successfulDoctorDeps()
			deps.cacheCapacityBytes = func(string) (uint64, error) { return 1 << 50, nil }
			inspections := 0
			deps.inspectCaches = func(context.Context) toolcache.Report {
				inspections++
				return toolcache.Report{BuildPath: "/cache/build", ModulePath: "/cache/modules"}
			}
			deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) { return workflowconfig.Workflow{Config: cfg}, nil }
			deps.gitWorkTree = func(context.Context, string) error { return nil }
			jobs := doctorProjectCheckJobs(globalconfig.Config{Projects: []globalconfig.Project{{ID: "test", Workflow: "WORKFLOW.md"}, {ID: "second", Workflow: "WORKFLOW.md"}}}, deps, RuntimeSecret{}, false, doctorWorkflowDefaultTokenThreshold)
			var checks []doctorCheck
			for _, job := range jobs {
				checks = append(checks, job.Run(t.Context())...)
			}
			if inspections != 1 {
				t.Fatalf("cache inspections = %d, want 1", inspections)
			}
			count := 0
			for _, check := range checks {
				if check.Name == "Host native toolchain caches" {
					count++
					if check.Status != doctorOK || !strings.Contains(check.Detail, "last reaper trim: not recorded") {
						t.Fatalf("check = %+v", check)
					}
				}
			}
			if count != 1 {
				t.Fatalf("native cache checks = %d, want 1", count)
			}
		})
	}
}

func TestDoctorCacheBound(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bound int64
		free  uint64
		path  string
		err   error
		want  doctorStatus
	}{
		{name: "default on 92 GiB volume", free: 92 << 30, path: "/native/build", want: doctorOK},
		{name: "default ample space", free: 300 << 30, path: "/native/build", want: doctorOK},
		{name: "explicit at ten percent", bound: 100, free: 1000, path: "/native/build", want: doctorOK},
		{name: "explicit above ten percent", bound: 101, free: 1000, path: "/native/build", want: doctorWarn},
		{name: "capacity unavailable", bound: 1, path: "/native/build", want: doctorWarn},
		{name: "explicit lookup failure", bound: 50 << 30, path: "/native/build", err: errors.New("stat failed"), want: doctorWarn},
		{name: "lookup failure", path: "/native/build", err: errors.New("stat failed"), want: doctorWarn},
		{name: "disabled", path: "off", want: doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			deps := doctorDeps{
				inspectCaches: func(context.Context) toolcache.Report { return toolcache.Report{BuildPath: tt.path} },
				cacheCapacityBytes: func(path string) (uint64, error) {
					calls++
					if path != tt.path {
						t.Fatalf("volume path = %s", path)
					}
					return tt.free, tt.err
				},
			}
			got := checkDoctorNativeCaches(t.Context(), deps, toolcache.Policy{MaxBytes: tt.bound})
			if got.Status != tt.want {
				t.Fatalf("check = %+v", got)
			}
			if (tt.err != nil || tt.free == 0) && tt.path != "off" {
				if wantFallback := tt.bound == 0; strings.Contains(got.Detail, "fallback") != wantFallback {
					t.Fatalf("fallback diagnostic: %+v", got)
				}
			}
			if tt.path == "off" && calls != 0 {
				t.Fatal("inspected disabled cache")
			}
			if tt.want == doctorWarn && tt.err == nil && tt.free > 0 && !strings.Contains(got.Detail, "exceeds 10%") {
				t.Fatalf("missing threshold: %+v", got)
			}
		})
	}
}

func TestDoctorCacheLastSweep(t *testing.T) {
	for _, tt := range []struct {
		name      string
		age, size int64
		want      doctorStatus
	}{
		{"no eviction", 0, 0, doctorOK}, {"age only", 1024, 0, doctorOK}, {"size", 0, 1024, doctorWarn}, {"both", 1024, 1024, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps := doctorDeps{cacheCapacityBytes: func(string) (uint64, error) { return 1000 << 30, nil }, inspectCaches: func(context.Context) toolcache.Report {
				return toolcache.Report{BuildPath: "/cache", BuildBytes: 99 << 30, RetainedBytes: 20 << 30, AgeExpiredBytes: tt.age, SizeEvictedBytes: tt.size}
			}}
			got := checkDoctorNativeCaches(t.Context(), deps, toolcache.Policy{MaxBytes: 20 << 30})
			if got.Status != tt.want {
				t.Fatalf("check=%+v", got)
			}
			if tt.size > 0 && (!strings.Contains(got.Hint, "global.cache.max_bytes") || !strings.Contains(got.Hint, "20.0 GiB")) {
				t.Fatalf("hint=%q", got.Hint)
			}
		})
	}
}

func TestDoctorLegacyCacheInspectionFailure(t *testing.T) {
	check := checkDoctorLegacyCaches("test", "\x00")
	if check.Status != doctorWarn || strings.Contains(check.Detail, "no Detent-owned cache roots") {
		t.Fatalf("check=%+v", check)
	}
}
