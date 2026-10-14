package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/intake"
)

func TestRunDoctorStartupPreflight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		loadErr     error
		wantStatus  doctorStatus
		wantDetail  string
		wantFailure bool
		selected    string
		unrelated   bool
	}{
		{
			name:       "valid project is startup compatible",
			wantStatus: doctorOK,
			wantDetail: "loaded and validated",
		},
		{
			name:        "invalid project is isolated without blocking host boot",
			loadErr:     errors.New("schedule ownership is invalid"),
			wantStatus:  doctorWarn,
			wantDetail:  "isolate this project as degraded",
			wantFailure: false,
		},
		{
			name:       "selected project excludes unrelated invalid startup",
			selected:   "alpha",
			unrelated:  true,
			wantStatus: doctorOK,
			wantDetail: "loaded and validated",
		},
		{
			name:        "selected invalid project fails readiness",
			selected:    "alpha",
			loadErr:     errors.New("schedule ownership is invalid"),
			wantStatus:  doctorFail,
			wantDetail:  "cannot start",
			wantFailure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := t.TempDir() + "/global.yaml"
			global := validDoctorGlobalWithProjects(configPath, "alpha")
			if tt.unrelated {
				global.Projects = append(global.Projects, globalconfig.Project{ID: "beta", Workflow: "beta/WORKFLOW.md", Workdir: "/beta", Weight: 1})
			}
			deps := successfulDoctorDeps()
			deps.loadWorkflow = func(path string) (workflowconfig.Workflow, error) {
				if path == "beta/WORKFLOW.md" {
					t.Fatal("selected startup readiness loaded unrelated project")
				}
				if tt.loadErr != nil {
					return workflowconfig.Workflow{}, tt.loadErr
				}
				return workflowconfig.Workflow{Config: validDoctorWorkflow("/alpha")}, nil
			}

			report := runDoctorStartupPreflight(t.Context(), doctorConfig{
				ConfigPath: configPath,
				ProjectID:  tt.selected,
				Flags: runtimeFlags{
					Port: runtimeIntFlag{Value: 0, Set: true},
				},
			}, successfulDoctorOptionsWithConfig(configPath, global), deps)

			assertDoctorCheck(t, report, "Candidate startup", doctorOK, "candidate resolved")
			assertDoctorCheck(t, report, "Project alpha startup", tt.wantStatus, tt.wantDetail)
			if got := report.HasFailures(); got != tt.wantFailure {
				t.Fatalf("HasFailures() = %t, want %t", got, tt.wantFailure)
			}
		})
	}
}

func TestRunDoctorStartupPreflightExplainsMappedNativeMigration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, feature, want string
		globalIntake        bool
		selected            bool
	}{
		{name: "intake", feature: "intake:\n  sources:\n    - name: errors\n      kind: webhook\n      secret: test-secret\n      creates:\n        status: Backlog\n", want: "intake.sources"},
		{name: "routines", feature: "schedule_ownership:\n  enabled: true\n  key: acme/alpha\n  repository: acme/alpha\nroutines:\n  - name: audit\n    schedule: '0 * * * *'\n    prompt: Inspect.\n", want: "routines"},
		{name: "global intake override", globalIntake: true, want: "intake.sources"},
		{name: "selected mapped intake", selected: true, globalIntake: true, want: "intake.sources"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "global.yaml")
			global := validDoctorGlobalWithProjects(configPath, "alpha")
			global.Client.NativeProjects = map[string]string{"alpha": "prj_test"}
			if test.globalIntake {
				global.Projects[0].IntakeConfigured = true
				global.Projects[0].Intake = intake.Config{Sources: []intake.Source{{Name: "errors", Kind: intake.KindWebhook, Secret: "test-secret", Creates: intake.Creates{Status: "Backlog"}}}}
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: github\n  project_slug: PVT_test\n  repository: acme/alpha\n  api_key: test-token\n" + test.feature + "---\nPrompt\n"))
			if err != nil {
				t.Fatal(err)
			}
			deps := successfulDoctorDeps()
			deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) { return workflow, nil }
			cfg := doctorConfig{ConfigPath: configPath}
			if test.selected {
				cfg.ProjectID = "alpha"
			}
			report := runDoctorStartupPreflight(t.Context(), cfg, successfulDoctorOptionsWithConfig(configPath, global), deps)
			assertDoctorCheck(t, report, "Candidate startup", doctorOK, "candidate resolved")
			assertDoctorCheck(t, report, "Project alpha startup", doctorFail, test.want)
			assertDoctorCheck(t, report, "Project alpha startup", doctorFail, "migrate")
			if !report.HasFailures() {
				t.Fatal("preflight accepted a mapped native project that cannot start")
			}
		})
	}
}

func TestRunDoctorStartupPreflightRejectsUnresolvableBootConfig(t *testing.T) {
	t.Parallel()

	configPath := t.TempDir() + "/global.yaml"
	opts := successfulDoctorOptions(configPath)
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{}, errors.New("candidate cannot resolve global config")
	}
	report := runDoctorStartupPreflight(context.Background(), doctorConfig{ConfigPath: configPath}, opts, successfulDoctorDeps())

	assertDoctorCheck(t, report, "Candidate startup", doctorFail, "cannot resolve global config")
	if !report.HasFailures() {
		t.Fatal("HasFailures() = false, want candidate rejection")
	}
}

func TestStartupPreflightDoesNotRetryCredentials(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed command", true: "empty credential"}[empty], func(t *testing.T) {
			dir := t.TempDir()
			workflow := filepath.Join(dir, "WORKFLOW.md")
			if err := os.WriteFile(workflow, []byte("---\ntracker:\n  kind: github\n  project_slug: PVT_test\n---\nPrompt\n"), 0600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dir, "global.yaml")
			global := validDoctorGlobalWithProjects(configPath, "alpha")
			global.GitHubToken = "gh"
			global.Projects[0].Workflow = workflow
			global.Projects[0].Workdir = dir
			opts := successfulDoctorOptionsWithConfig(configPath, global)
			opts.lookupEnv = func(string) string { return "" }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			opts.ghAuthToken = func(context.Context) (string, error) {
				calls++
				// A second call cancels a regressed retry loop instead of hanging the suite.
				if calls > 1 {
					cancel()
				}
				if empty {
					return "", nil
				}
				return "", errors.New("keyring unavailable")
			}
			report := runDoctorStartupPreflight(ctx, doctorConfig{ConfigPath: configPath}, opts, successfulDoctorDeps())
			if calls != 1 {
				t.Fatalf("credential calls=%d, want 1", calls)
			}
			assertDoctorCheck(t, report, "Candidate startup", doctorFail, "gh auth token")
		})
	}
}
