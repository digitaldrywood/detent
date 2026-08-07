package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDefaultDoctorWorkflowSourcePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*testing.T, string, string, *globalconfig.Project)
		wantStatus doctorStatus
		wantDetail []string
		notDetail  []string
	}{
		{
			name: "drift present",
			mutate: func(t *testing.T, repo string, _ string, _ *globalconfig.Project) {
				runDoctorWorkflowSourceGit(t, repo, "checkout", "-b", "feature")
				writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), "schema: 1\ntracker:\n  kind: github\n")
			},
			wantStatus: doctorFail,
			wantDetail: []string{"project alpha", "omits workflow_ref", "branch feature differs from default branch main", "effective detent.yaml differs from origin/main"},
		},
		{
			name:       "no drift",
			wantStatus: doctorWarn,
			wantDetail: []string{"project alpha", "omits workflow_ref", "branch main matches default branch main", "effective detent.yaml matches origin/main"},
		},
		{
			name: "line ending difference is not drift",
			mutate: func(t *testing.T, repo string, _ string, _ *globalconfig.Project) {
				writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), "schema: 1\r\ntracker:\r\n  kind: memory\r\n")
			},
			wantStatus: doctorWarn,
			wantDetail: []string{"project alpha", "effective detent.yaml matches origin/main"},
		},
		{
			name: "workflow ref skips working tree",
			mutate: func(t *testing.T, repo string, _ string, project *globalconfig.Project) {
				project.WorkflowRef = "origin/main"
				runDoctorWorkflowSourceGit(t, repo, "checkout", "-b", "feature")
				writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), "schema: 1\ntracker:\n  kind: github\n")
			},
			wantStatus: doctorOK,
			wantDetail: []string{"project alpha", "workflow_ref origin/main is fresh", "ignores the working-tree branch"},
			notDetail:  []string{"mutable working tree", "detent.yaml"},
		},
		{
			name: "detached head",
			mutate: func(t *testing.T, repo string, _ string, _ *globalconfig.Project) {
				runDoctorWorkflowSourceGit(t, repo, "checkout", "--detach")
			},
			wantStatus: doctorWarn,
			wantDetail: []string{"project alpha", "checkout is detached", "effective detent.yaml matches origin/main"},
		},
		{
			name: "missing detent yaml",
			mutate: func(t *testing.T, repo string, _ string, _ *globalconfig.Project) {
				if err := os.Remove(filepath.Join(repo, "detent.yaml")); err != nil {
					t.Fatalf("Remove(detent.yaml) error = %v", err)
				}
			},
			wantStatus: doctorFail,
			wantDetail: []string{"project alpha", "effective detent.yaml is missing"},
		},
		{
			name: "unreadable detent yaml",
			mutate: func(t *testing.T, repo string, _ string, _ *globalconfig.Project) {
				path := filepath.Join(repo, "detent.yaml")
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(detent.yaml) error = %v", err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("Mkdir(detent.yaml) error = %v", err)
				}
			},
			wantStatus: doctorFail,
			wantDetail: []string{"project alpha", "read effective detent.yaml"},
		},
		{
			name: "stale workflow ref",
			mutate: func(t *testing.T, _ string, remote string, project *globalconfig.Project) {
				project.WorkflowRef = "origin/main"
				updater := filepath.Join(t.TempDir(), "updater")
				runDoctorWorkflowSourceGit(t, "", "clone", remote, updater)
				runDoctorWorkflowSourceGit(t, updater, "config", "user.name", "Detent Test")
				runDoctorWorkflowSourceGit(t, updater, "config", "user.email", "detent@example.com")
				writeDoctorWorkflowSourceFile(t, filepath.Join(updater, "detent.yaml"), "schema: 1\ntracker:\n  kind: github\n")
				runDoctorWorkflowSourceGit(t, updater, "add", "detent.yaml")
				runDoctorWorkflowSourceGit(t, updater, "commit", "-m", "advance workflow policy")
				runDoctorWorkflowSourceGit(t, updater, "push", "origin", "main")
			},
			wantStatus: doctorFail,
			wantDetail: []string{"project alpha", "workflow_ref origin/main is stale", "remote counterpart origin/main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, remote := initDoctorWorkflowSourceRepository(t)
			project := globalconfig.Project{
				ID:       "alpha",
				Workflow: filepath.Join(repo, "WORKFLOW.md"),
				Workdir:  repo,
			}
			if tt.mutate != nil {
				tt.mutate(t, repo, remote, &project)
			}

			check := defaultDoctorWorkflowSourcePolicy(context.Background(), "alpha", project, repo, "main")
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s; detail = %q", check.Status, tt.wantStatus, check.Detail)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			for _, unwanted := range tt.notDetail {
				if strings.Contains(check.Detail, unwanted) {
					t.Fatalf("Detail = %q, want not containing %q", check.Detail, unwanted)
				}
			}
		})
	}
}

func TestCheckDoctorWorkflowSourcePolicyUsesGitHubDefaultBranch(t *testing.T) {
	t.Parallel()

	var gotRepository string
	var gotDefaultBranch string
	var gotSourceRoot string
	workdir := filepath.Join(t.TempDir(), "repo")
	check, ok := checkDoctorWorkflowSourcePolicy(
		context.Background(),
		"alpha",
		globalconfig.Project{Workflow: filepath.Join(workdir, "WORKFLOW.md"), Workdir: workdir},
		workflowconfig.Config{Tracker: workflowconfig.Tracker{Repository: "fallback/repository"}},
		"/workspace-root",
		doctorDeps{
			gitRemoteURL: func(context.Context, string) (string, error) {
				return "git@github.com:source/repository.git", nil
			},
			githubRepositoryInfo: func(_ context.Context, _ workflowconfig.Config, repository string) (ghconnector.RepositoryInfo, error) {
				gotRepository = repository
				return ghconnector.RepositoryInfo{DefaultBranch: "trunk"}, nil
			},
			workflowSourcePolicy: func(_ context.Context, projectID string, _ globalconfig.Project, sourceRoot string, defaultBranch string) doctorCheck {
				gotDefaultBranch = defaultBranch
				gotSourceRoot = sourceRoot
				return doctorCheck{Name: "Project " + projectID + " workflow source policy", Status: doctorWarn}
			},
		},
	)
	if !ok {
		t.Fatal("checkDoctorWorkflowSourcePolicy() ok = false, want true")
	}
	if check.Name != "Project alpha workflow source policy" || gotRepository != "source/repository" || gotDefaultBranch != "trunk" || gotSourceRoot != workdir {
		t.Fatalf("check = %#v, repository = %q, default branch = %q, source root = %q", check, gotRepository, gotDefaultBranch, gotSourceRoot)
	}
}

func initDoctorWorkflowSourceRepository(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	repo := filepath.Join(root, "checkout")
	runDoctorWorkflowSourceGit(t, "", "init", "--bare", "--initial-branch=main", remote)
	runDoctorWorkflowSourceGit(t, "", "init", "--initial-branch=main", seed)
	runDoctorWorkflowSourceGit(t, seed, "config", "user.name", "Detent Test")
	runDoctorWorkflowSourceGit(t, seed, "config", "user.email", "detent@example.com")
	writeDoctorWorkflowSourceFile(t, filepath.Join(seed, "WORKFLOW.md"), "Agent direction.\n")
	writeDoctorWorkflowSourceFile(t, filepath.Join(seed, "detent.yaml"), "schema: 1\ntracker:\n  kind: memory\n")
	runDoctorWorkflowSourceGit(t, seed, "add", "WORKFLOW.md", "detent.yaml")
	runDoctorWorkflowSourceGit(t, seed, "commit", "-m", "initial workflow policy")
	runDoctorWorkflowSourceGit(t, seed, "remote", "add", "origin", remote)
	runDoctorWorkflowSourceGit(t, seed, "push", "-u", "origin", "main")
	runDoctorWorkflowSourceGit(t, "", "clone", remote, repo)
	runDoctorWorkflowSourceGit(t, repo, "config", "user.name", "Detent Test")
	runDoctorWorkflowSourceGit(t, repo, "config", "user.email", "detent@example.com")
	return repo, remote
}

func writeDoctorWorkflowSourceFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", filepath.Base(path), err)
	}
}

func runDoctorWorkflowSourceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
