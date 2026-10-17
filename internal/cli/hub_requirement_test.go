package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestHubStartupRequirement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		url         string
		kinds       []string
		loadErr     error
		wantWarning bool
		wantRefusal bool
	}{
		{name: "Hub native with Hub", url: "https://hub.example.test", kinds: []string{"hub_native"}},
		{name: "GitHub with Hub", url: "https://hub.example.test", kinds: []string{"github"}},
		{name: "Hub native without Hub", kinds: []string{"hub_native"}, wantWarning: true},
		{name: "GitHub without Hub", kinds: []string{"github"}, wantWarning: true, wantRefusal: true},
		{name: "local without Hub", kinds: []string{"local"}, wantWarning: true, wantRefusal: true},
		{name: "memory without Hub", kinds: []string{"memory"}, wantWarning: true, wantRefusal: true},
		{name: "blank Hub URL", url: " \t", kinds: []string{"memory"}, wantWarning: true, wantRefusal: true},
		{name: "mixed trackers without Hub", kinds: []string{"hub_native", "memory"}, wantWarning: true, wantRefusal: true},
		{name: "no projects without Hub", wantWarning: true},
		{name: "unreadable project without Hub", kinds: []string{"unreadable"}, loadErr: os.ErrNotExist, wantWarning: true},
		{name: "unreadable and standalone projects", kinds: []string{"unreadable", "memory"}, loadErr: os.ErrPermission, wantWarning: true, wantRefusal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Config{Client: globalconfig.HubClient{URL: test.url}}
			kinds := make(map[string]string)
			for _, kind := range test.kinds {
				cfg.Projects = append(cfg.Projects, globalconfig.Project{ID: kind, Workflow: kind})
				kinds[kind] = kind
			}
			err := validateHubStartup(context.Background(), cfg, runtimeDeps{
				loadWorkflow: func(path string) (workflowconfig.Workflow, error) {
					if path == "unreadable" {
						return workflowconfig.Workflow{}, test.loadErr
					}
					if test.url != "" && !test.wantWarning {
						t.Fatal("Hub startup loaded a local project workflow")
					}
					return workflowconfig.Workflow{Config: workflowconfig.Config{Tracker: workflowconfig.Tracker{Kind: kinds[path]}}}, nil
				},
			})
			if (err != nil) != test.wantRefusal {
				t.Fatalf("startup error = %v, want refusal %v", err, test.wantRefusal)
			}
			if test.wantRefusal {
				problem := ProblemForError(err)
				if problem.Code != "validation" || problem.ExitCode != ExitValidation {
					t.Fatalf("startup problem = %#v, want validation failure", problem)
				}
				for _, want := range []string{"self-hosted Hub", "enroll a runner", "docs/hub-self-hosting.md"} {
					if !strings.Contains(problem.SuggestedFix, want) {
						t.Errorf("startup hint = %q, want %q", problem.SuggestedFix, want)
					}
				}
			}
			check := checkDoctorHubConnection(cfg)
			if (check.Status == doctorWarn) != test.wantWarning {
				t.Fatalf("doctor check = %#v, want warning %v", check, test.wantWarning)
			}
		})
	}
}

func TestRootCommandWarnsAndRefusesStandalone(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n---\nWork the issue.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := globalconfig.Config{Path: filepath.Join(root, "global.yaml"), Projects: []globalconfig.Project{{ID: "example", Workflow: workflowPath}}}
	var logs bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	cmd := NewRootCommand(context.Background(), func(opts *options) {
		opts.read = func(string) (globalconfig.Config, error) { return cfg, nil }
		opts.lookupEnv = func(string) string { return "" }
		opts.ghAuthToken = func(context.Context) (string, error) { return "", errors.New("unavailable") }
		opts.stdoutTTY = func() bool { return false }
		opts.configureLog = func(RuntimeSettings, io.Writer, io.Writer, bool) *slog.LevelVar {
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			return nil
		}
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--config", cfg.Path, "--headless"})
	err := cmd.Execute()
	if problem := ProblemForError(err); err == nil || problem.Code != "validation" {
		t.Fatalf("startup error = %v, want standalone validation failure", err)
	}
	for _, want := range []string{"level=WARN", "deprecated", "self-hosted Hub", "enroll a runner"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("startup log missing %q: %s", want, logs.String())
		}
	}
}

func TestDoctorReportsHubRequirement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		url    string
		status doctorStatus
		text   string
	}{
		{name: "standalone", status: doctorWarn, text: "deprecated"},
		{name: "Hub", url: "https://hub.example.test", status: doctorOK, text: "client.hub_url is configured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "global.yaml")
			opts := successfulDoctorOptions(path)
			read := opts.read
			opts.read = func(path string) (globalconfig.Config, error) {
				cfg, err := read(path)
				cfg.Client.URL = test.url
				return cfg, err
			}
			report := runDoctor(context.Background(), doctorConfig{ConfigPath: path}, opts, successfulDoctorDeps())
			assertDoctorCheck(t, report, "Hub connection", test.status, test.text)
			if test.status == doctorWarn {
				check := doctorCheckByName(t, report, "Hub connection")
				for _, want := range []string{"self-hosted Hub", "enroll a runner", "docs/hub-self-hosting.md"} {
					if !strings.Contains(check.Hint, want) {
						t.Errorf("doctor hint = %q, want %q", check.Hint, want)
					}
				}
			}
		})
	}
}

func TestStartCommandHubRequirement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, url, kind string
		wantRefusal     bool
	}{
		{name: "standalone", kind: "memory", wantRefusal: true},
		{name: "Hub native without Hub", kind: "hub_native"},
		{name: "Hub native with Hub", url: "https://hub.example.test", kind: "hub_native"},
		{name: "memory with Hub", url: "https://hub.example.test", kind: "memory"},
		{name: "unreadable workflow", kind: "missing"},
	} {
		for _, restart := range []bool{false, true} {
			name := test.name
			if restart {
				name += " restart"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				workflow := filepath.Join(root, "WORKFLOW.md")
				if test.kind != "missing" {
					if err := os.WriteFile(workflow, []byte("---\ntracker:\n  kind: "+test.kind+"\n---\nWork the issue.\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(root, "global.yaml")
				cfg := globalconfig.Config{Path: path, Client: globalconfig.HubClient{URL: test.url}, Projects: []globalconfig.Project{{ID: "example", Workflow: workflow}}}
				runner := &serviceRunnerStub{startResults: []servicepkg.StartResult{{Action: servicepkg.ActionStarted, State: servicepkg.StateRunning}}}
				opts := defaultOptions()
				opts.readOrDefault = func(string) (globalconfig.Config, error) { return cfg, nil }
				opts.lookupEnv = func(string) string { return "" }
				WithServiceFactory(func(servicepkg.Config) (ServiceRunner, error) {
					if test.wantRefusal {
						t.Fatal("standalone startup reached the service manager")
					}
					return runner, nil
				})(&opts)
				host, port := "", 0
				cmd := newStartCommand(&path, &host, &port, opts)
				args := []string{"--yes"}
				if restart {
					args = append(args, "--restart")
				}
				cmd.SetArgs(args)
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				err := cmd.ExecuteContext(t.Context())
				if test.wantRefusal {
					problem := ProblemForError(err)
					if err == nil || problem.Code != "validation" || !strings.Contains(problem.SuggestedFix, "docs/hub-self-hosting.md") {
						t.Fatalf("startup error = %v, problem = %#v", err, problem)
					}
				} else if err != nil || len(runner.startOptions) != 1 || runner.startOptions[0].Restart != restart {
					t.Fatalf("startup error = %v, options = %#v", err, runner.startOptions)
				}
			})
		}
	}
}
