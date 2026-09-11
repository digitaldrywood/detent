package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestRunDoctorStartupPreflight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		loadErr     error
		wantStatus  doctorStatus
		wantDetail  string
		wantFailure bool
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := t.TempDir() + "/global.yaml"
			global := validDoctorGlobalWithProjects(configPath, "alpha")
			deps := successfulDoctorDeps()
			deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
				if tt.loadErr != nil {
					return workflowconfig.Workflow{}, tt.loadErr
				}
				return workflowconfig.Workflow{Config: validDoctorWorkflow("/alpha")}, nil
			}

			report := runDoctorStartupPreflight(context.Background(), doctorConfig{
				ConfigPath: configPath,
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
