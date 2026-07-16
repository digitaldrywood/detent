package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorConfigPreservesMissingWorkflowForDiagnosis(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repoRoot := filepath.Join(root, "repo")
	expectedPath := filepath.Join(repoRoot, "WORKFLOW.md")
	foundPath := filepath.Join(repoRoot, ".detent", "WORKFLOW.md")
	writeDoctorWorkflowLocationFile(t, foundPath)
	configPath := filepath.Join(root, "global.yaml")
	raw := fmt.Sprintf(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 1
  scheduling: weighted
projects:
  - id: alpha
    workflow: %q
    workdir: %q
    weight: 1
    priority: 0
`, expectedPath, repoRoot)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	_, cfg, _, _, configCheck := checkDoctorConfig(configPath, "", defaultOptions())
	if configCheck.Status != doctorOK || cfg == nil {
		t.Fatalf("checkDoctorConfig() = (%#v, %#v), want parsed config", cfg, configCheck)
	}
	checks := checkDoctorProjects(context.Background(), *cfg, successfulDoctorDeps(), RuntimeSecret{}, false)
	if len(checks) == 0 {
		t.Fatal("checkDoctorProjects() returned no checks")
	}
	for _, want := range []string{foundPath, expectedPath} {
		if !strings.Contains(checks[0].Detail, want) {
			t.Fatalf("workflow check detail = %q, want containing %q", checks[0].Detail, want)
		}
	}
}

func TestCheckDoctorWorkflowLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		rootWorkflow      bool
		detentWorkflow    bool
		configuredOutside bool
		wantFinding       bool
		wantFoundPath     bool
	}{
		{name: "root present", rootWorkflow: true},
		{name: "nonstandard present", detentWorkflow: true, wantFinding: true, wantFoundPath: true},
		{name: "both present", rootWorkflow: true, detentWorkflow: true},
		{name: "neither present", wantFinding: true},
		{name: "config points outside repo", configuredOutside: true, wantFinding: true, wantFoundPath: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			repoRoot := filepath.Join(parent, "repo")
			if err := os.MkdirAll(repoRoot, 0o755); err != nil {
				t.Fatalf("create repo root: %v", err)
			}
			expectedPath := filepath.Join(repoRoot, "WORKFLOW.md")
			configuredPath := expectedPath
			foundPath := filepath.Join(repoRoot, ".detent", "WORKFLOW.md")
			if tt.configuredOutside {
				configuredPath = filepath.Join(parent, "detent-config", "WORKFLOW.md")
				foundPath = configuredPath
			}
			if tt.rootWorkflow {
				writeDoctorWorkflowLocationFile(t, expectedPath)
			}
			if tt.detentWorkflow || tt.configuredOutside {
				writeDoctorWorkflowLocationFile(t, foundPath)
			}

			check, gotFinding := checkDoctorWorkflowLocation(globalconfig.Project{
				Workflow: configuredPath,
				Workdir:  repoRoot,
			})
			if gotFinding != tt.wantFinding {
				t.Fatalf("checkDoctorWorkflowLocation() finding = %t, want %t: %#v", gotFinding, tt.wantFinding, check)
			}
			if !tt.wantFinding {
				return
			}
			if check.Status != doctorFail {
				t.Fatalf("check.Status = %s, want %s", check.Status, doctorFail)
			}
			if !strings.Contains(check.Detail, expectedPath) {
				t.Fatalf("check.Detail = %q, want expected path %q", check.Detail, expectedPath)
			}
			if tt.wantFoundPath && !strings.Contains(check.Detail, foundPath) {
				t.Fatalf("check.Detail = %q, want found path %q", check.Detail, foundPath)
			}
			for _, want := range []string{"repository root", "checked in", "WORKFLOW.local.md"} {
				if !strings.Contains(check.Hint, want) {
					t.Fatalf("check.Hint = %q, want containing %q", check.Hint, want)
				}
			}
		})
	}
}

func TestCheckDoctorWorkflowLocationClearsAfterMove(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	configuredPath := filepath.Join(repoRoot, "WORKFLOW.md")
	misplacedPath := filepath.Join(repoRoot, ".detent", "WORKFLOW.md")
	writeDoctorWorkflowLocationFile(t, misplacedPath)
	project := globalconfig.Project{Workflow: configuredPath, Workdir: repoRoot}

	if _, ok := checkDoctorWorkflowLocation(project); !ok {
		t.Fatal("checkDoctorWorkflowLocation() did not report misplaced workflow")
	}
	if err := os.Rename(misplacedPath, configuredPath); err != nil {
		t.Fatalf("move workflow to repository root: %v", err)
	}
	if check, ok := checkDoctorWorkflowLocation(project); ok {
		t.Fatalf("checkDoctorWorkflowLocation() = %#v, want finding cleared", check)
	}
}

func writeDoctorWorkflowLocationFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create workflow directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("---\n---\n"), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}
