package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestDoctorSharedCache(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		want doctorStatus
	}{
		{"empty", 0, doctorOK}, {"small", 4, doctorOK}, {"over budget", workspace.SharedBuildCacheBudget + 1, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(workspace.SharedCacheRoot(root, "test"), "go-build")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(dir, "data"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			got := checkDoctorSharedCache(t.Context(), "test", root)
			if got.Status != tt.want || !strings.Contains(got.Detail, "go-build=") {
				t.Fatalf("check = %+v", got)
			}
		})
	}
}

func TestDoctorSharedCacheWorkspaceKinds(t *testing.T) {
	for _, kind := range []string{workflowconfig.WorkspaceLocalGit, workflowconfig.WorkspaceFilesystem} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			cfg := validDoctorWorkflow(root)
			cfg.Workspace.Root = root
			cfg.Workspace.Kind = kind
			if kind == workflowconfig.WorkspaceFilesystem {
				cfg.Deliverable.Kind = workflowconfig.DeliverableArtifact
				cfg.Deliverable.OutputRoot = t.TempDir()
				cfg.Workspace.AutoBranch = false
			}
			dir := filepath.Join(workspace.SharedCacheRoot(root, "test"), "go-build")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "data"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			checks := checkDoctorProjects(t.Context(), globalconfig.Config{Projects: []globalconfig.Project{{ID: "test", Workflow: "WORKFLOW.md"}}}, doctorDeps{
				loadWorkflow: func(string) (workflowconfig.Workflow, error) { return workflowconfig.Workflow{Config: cfg}, nil },
				gitWorkTree:  func(context.Context, string) error { return nil },
			}, RuntimeSecret{}, false)
			for _, check := range checks {
				if check.Name == "Project test shared cache" {
					if check.Status != doctorOK || !strings.Contains(check.Detail, "go-build=4") {
						t.Fatalf("check = %+v", check)
					}
					return
				}
			}
			t.Fatalf("shared cache check absent for %s", kind)
		})
	}
}
