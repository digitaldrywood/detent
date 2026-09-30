package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCheckDoctorWorkflowRefDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		workflowRef  string
		mutate       func(*testing.T, string)
		wantFinding  bool
		wantStatus   doctorStatus
		wantDetail   []string
		unwantDetail []string
	}{
		{
			name:        "config keys differ",
			workflowRef: "origin/main",
			mutate: func(t *testing.T, repo string) {
				writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), "schema: 1\ntracker:\n  kind: memory\nagent:\n  max_turns: 99\n")
			},
			wantFinding: true,
			wantStatus:  doctorWarn,
			wantDetail:  []string{"working-tree project definition differs", "detent.yaml keys differ: agent.max_turns"},
		},
		{
			name:        "workflow prompt differs",
			workflowRef: "origin/main",
			mutate: func(t *testing.T, repo string) {
				writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "Updated agent direction.\n")
			},
			wantFinding: true,
			wantStatus:  doctorWarn,
			wantDetail:  []string{"WORKFLOW.md prompt differs"},
		},
		{
			name:        "validation outcome differs",
			workflowRef: "origin/main",
			mutate:      writeDoctorWorkflowSourceValidationDrift,
			wantFinding: true,
			wantStatus:  doctorFail,
			wantDetail: []string{
				"detent.yaml keys differ: schedule_ownership.enabled, schedule_ownership.key, schedule_ownership.repository",
				"working-tree configuration passes validation while ref-backed configuration fails",
				"schedule_ownership.enabled must be true",
			},
		},
		{
			name:        "working tree matches ref",
			workflowRef: "origin/main",
			wantFinding: false,
		},
		{
			name:        "workflow ref is not configured",
			wantFinding: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, _ := initDoctorWorkflowSourceRepository(t)
			if tt.mutate != nil {
				tt.mutate(t, repo)
			}
			project := globalconfig.Project{
				ID:          "alpha",
				Workflow:    filepath.Join(repo, "WORKFLOW.md"),
				WorkflowRef: tt.workflowRef,
				Workdir:     repo,
			}
			effectiveWorkflow, err := loadDoctorProjectWorkflowUncached(context.Background(), project, doctorDeps{loadWorkflow: workflowconfig.LoadWorkflow})
			if err != nil {
				t.Fatalf("loadDoctorProjectWorkflowUncached() error = %v", err)
			}
			effectiveValidationErr := effectiveWorkflow.Config.Validate()

			check, found := checkDoctorWorkflowRefDrift(
				context.Background(),
				"alpha",
				project,
				repo,
				effectiveValidationErr,
				RuntimeSecret{},
				doctorDeps{loadWorkflow: workflowconfig.LoadWorkflow},
			)
			if found != tt.wantFinding {
				t.Fatalf("checkDoctorWorkflowRefDrift() found = %t, want %t; check = %#v", found, tt.wantFinding, check)
			}
			if !found {
				return
			}
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s; detail = %q", check.Status, tt.wantStatus, check.Detail)
			}
			revision := runDoctorWorkflowSourceGit(t, repo, "rev-parse", "origin/main")
			wantRevision := "workflow_ref origin/main at " + doctorShortRevision(revision)
			if !strings.Contains(check.Detail, wantRevision) {
				t.Fatalf("Detail = %q, want containing %q", check.Detail, wantRevision)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			for _, unwanted := range tt.unwantDetail {
				if strings.Contains(check.Detail, unwanted) {
					t.Fatalf("Detail = %q, want not containing %q", check.Detail, unwanted)
				}
			}
		})
	}
}

func TestCheckDoctorProjectReportsWorkflowRefDriftWhenEffectiveConfigIsInvalid(t *testing.T) {
	t.Parallel()

	repo, _ := initDoctorWorkflowSourceRepository(t)
	writeDoctorWorkflowSourceValidationDrift(t, repo)
	project := globalconfig.Project{
		ID:          "alpha",
		Workflow:    filepath.Join(repo, "WORKFLOW.md"),
		WorkflowRef: "origin/main",
		Workdir:     repo,
	}
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow

	checks := checkDoctorProject(context.Background(), project, deps, RuntimeSecret{}, false)
	assertDoctorCheck(t, doctorReport{Checks: checks}, "Project alpha workflow", doctorFail, "schedule_ownership.enabled must be true")
	assertDoctorCheck(t, doctorReport{Checks: checks}, "Project alpha workflow ref drift", doctorFail, "working-tree configuration passes validation")
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

func TestCheckDoctorProjectRunsWorkflowSourcePolicyBeforeMutableKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*workflowconfig.Config)
	}{
		{
			name: "tracker kind drifted",
			mutate: func(cfg *workflowconfig.Config) {
				cfg.Tracker.Kind = workflowconfig.TrackerMemory
			},
		},
		{
			name: "deliverable kind drifted",
			mutate: func(cfg *workflowconfig.Config) {
				cfg.Deliverable.Kind = workflowconfig.DeliverableArtifact
			},
		},
		{
			name: "workspace kind drifted",
			mutate: func(cfg *workflowconfig.Config) {
				cfg.Workspace.Kind = workflowconfig.WorkspaceFilesystem
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sourceRoot := t.TempDir()
			workflow := validDoctorDependencyWorkflow(false)
			workflow.Workspace.Root = sourceRoot
			tt.mutate(&workflow)

			deps := successfulDoctorDeps()
			deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
				return workflowconfig.Workflow{Config: workflow}, nil
			}
			deps.gitRemoteURL = func(context.Context, string) (string, error) {
				return "https://github.com/source/repository.git", nil
			}
			called := false
			deps.workflowSourcePolicy = func(_ context.Context, projectID string, _ globalconfig.Project, gotSourceRoot string, defaultBranch string) doctorCheck {
				called = true
				if projectID != "alpha" || gotSourceRoot != sourceRoot || defaultBranch != "main" {
					t.Fatalf("policy inputs = project %q, root %q, branch %q", projectID, gotSourceRoot, defaultBranch)
				}
				return doctorCheck{Name: "Project alpha workflow source policy", Status: doctorFail, Detail: "policy drift detected"}
			}

			checks := checkDoctorProject(context.Background(), globalconfig.Project{
				ID:       "alpha",
				Workflow: filepath.Join(sourceRoot, "WORKFLOW.md"),
				Workdir:  sourceRoot,
			}, deps, RuntimeSecret{}, false)
			if !called {
				t.Fatal("workflow source policy was skipped")
			}
			assertDoctorCheck(t, doctorReport{Checks: checks}, "Project alpha workflow source policy", doctorFail, "policy drift detected")
		})
	}
}

func TestCheckDoctorWorkflowSourcePolicySkipsNonGitHubMutableProject(t *testing.T) {
	t.Parallel()

	_, ok := checkDoctorWorkflowSourcePolicy(
		context.Background(),
		"alpha",
		globalconfig.Project{Workflow: "WORKFLOW.md", Workdir: "/repo"},
		workflowconfig.Config{Tracker: workflowconfig.Tracker{Kind: workflowconfig.TrackerMemory}},
		"/repo",
		doctorDeps{
			gitRemoteURL: func(context.Context, string) (string, error) {
				return "file:///tmp/repository.git", nil
			},
			workflowSourcePolicy: func(context.Context, string, globalconfig.Project, string, string) doctorCheck {
				t.Fatal("workflow source policy should not run")
				return doctorCheck{}
			},
		},
	)
	if ok {
		t.Fatal("checkDoctorWorkflowSourcePolicy() ok = true, want false")
	}
}

func initDoctorWorkflowSourceRepository(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	t.Cleanup(func() { cleanupDoctorWorkflowSourceRepository(t, root) })
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

func writeDoctorWorkflowSourceValidationDrift(t *testing.T, repo string) {
	t.Helper()

	invalid := "schema: 1\ntracker:\n  kind: github\n  api_key: token\n  github_status_source: label\n  repository: example/issues\nroutines:\n  - name: audit\n    schedule: 0 * * * *\n    prompt: Inspect.\n"
	writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), invalid)
	runDoctorWorkflowSourceGit(t, repo, "add", "detent.yaml")
	runDoctorWorkflowSourceGit(t, repo, "commit", "-m", "add scheduled routine")
	runDoctorWorkflowSourceGit(t, repo, "push", "origin", "main")
	valid := invalid + "schedule_ownership:\n  enabled: true\n  key: example/production\n  repository: example/coordination\n"
	writeDoctorWorkflowSourceFile(t, filepath.Join(repo, "detent.yaml"), valid)
}

func cleanupDoctorWorkflowSourceRepository(t *testing.T, root string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	var err error
	for {
		err = os.RemoveAll(root)
		if err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	remaining := make([]string, 0)
	walkErr := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil {
			return relativeErr
		}
		remaining = append(remaining, relative)
		return nil
	})
	t.Errorf("RemoveAll(%s) error = %v; remaining paths = %q; inspect error = %v", root, err, remaining, walkErr)
}

func writeDoctorWorkflowSourceFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", filepath.Base(path), err)
	}
}

func runDoctorWorkflowSourceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	gitArgs := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(t.Context(), "git", gitArgs...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
