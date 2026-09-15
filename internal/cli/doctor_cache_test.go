package cli

import (
	"context"
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
			deps := doctorDeps{inspectCaches: func(context.Context) toolcache.Report {
				return toolcache.Report{BuildPath: "/host/build", ModulePath: "/host/modules", BuildBytes: 12, ModuleBytes: 34, LastTrim: "2026-09-14T12:00:00Z", Error: tt.reportError}
			}}
			got := checkDoctorNativeCaches(t.Context(), "test", root, deps)
			if got.Status != tt.want {
				t.Fatalf("check = %+v", got)
			}
			for _, want := range []string{"GOCACHE=/host/build", "GOMODCACHE=/host/modules", "last reaper trim: 2026-09-14T12:00:00Z"} {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q missing %q", got.Detail, want)
				}
			}
			if tt.reportError == "" && (!strings.Contains(got.Detail, "12 bytes") || !strings.Contains(got.Detail, "34 bytes")) {
				t.Errorf("sizes missing: %+v", got)
			}
			if tt.legacy != "" {
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
				if check.Name == "Project test native toolchain caches" || check.Name == "Project second native toolchain caches" {
					count++
					if check.Status != doctorOK || !strings.Contains(check.Detail, "last reaper trim: not recorded") {
						t.Fatalf("check = %+v", check)
					}
				}
			}
			if count != 2 {
				t.Fatalf("native cache checks = %d, want 2", count)
			}
		})
	}
}
