package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
)

func TestCheckDoctorBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		lookPath   func(string) (string, error)
		runCommand func(context.Context, string, ...string) error
		want       doctorStatus
		wantDetail string
	}{
		{
			name: "missing from path",
			lookPath: func(string) (string, error) {
				return "", errors.New("missing")
			},
			runCommand: func(context.Context, string, ...string) error {
				return nil
			},
			want:       doctorFail,
			wantDetail: "not found on PATH",
		},
		{
			name: "not runnable",
			lookPath: func(string) (string, error) {
				return "/usr/bin/codex", nil
			},
			runCommand: func(context.Context, string, ...string) error {
				return errors.New("permission denied")
			},
			want:       doctorFail,
			wantDetail: "permission denied",
		},
		{
			name: "runnable",
			lookPath: func(string) (string, error) {
				return "/usr/bin/codex", nil
			},
			runCommand: func(context.Context, string, ...string) error {
				return nil
			},
			want:       doctorOK,
			wantDetail: "is runnable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkDoctorBinary(context.Background(), doctorDeps{
				lookPath:   tt.lookPath,
				runCommand: tt.runCommand,
			}, "codex", "codex binary", "--version", "install codex")
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s", got.Status, tt.want)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Fatalf("Detail = %q, want containing %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestCheckDoctorProjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		projects   []globalconfig.Project
		workflow   workflowconfig.Workflow
		loadErr    error
		gitErr     error
		wantStatus []doctorStatus
		wantDetail []string
	}{
		{
			name:       "no projects configured",
			wantStatus: []doctorStatus{doctorWarn},
			wantDetail: []string{"no projects configured"},
		},
		{
			name: "workflow cannot load",
			projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			},
			loadErr:    errors.New("missing workflow"),
			wantStatus: []doctorStatus{doctorFail, doctorWarn},
			wantDetail: []string{"missing workflow", "skipped"},
		},
		{
			name: "workflow invalid",
			projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			},
			workflow:   workflowconfig.Workflow{Config: workflowconfig.Config{}},
			wantStatus: []doctorStatus{doctorFail, doctorWarn},
			wantDetail: []string{"tracker.kind", "skipped"},
		},
		{
			name: "source repo missing",
			projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			},
			workflow:   workflowconfig.Workflow{Config: validDoctorWorkflow("/repo")},
			gitErr:     errors.New("not a git worktree"),
			wantStatus: []doctorStatus{doctorOK, doctorFail},
			wantDetail: []string{"is valid", "not a git worktree"},
		},
		{
			name: "workflow and source repo valid",
			projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			},
			workflow:   workflowconfig.Workflow{Config: validDoctorWorkflow("/repo")},
			wantStatus: []doctorStatus{doctorOK, doctorOK},
			wantDetail: []string{"is valid", "is a git worktree"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkDoctorProjects(context.Background(), globalconfig.Config{Projects: tt.projects}, doctorDeps{
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return tt.workflow, tt.loadErr
				},
				gitWorkTree: func(context.Context, string) error {
					return tt.gitErr
				},
			})
			if len(got) != len(tt.wantStatus) {
				t.Fatalf("len(checks) = %d, want %d: %#v", len(got), len(tt.wantStatus), got)
			}
			for i, check := range got {
				if check.Status != tt.wantStatus[i] {
					t.Fatalf("checks[%d].Status = %s, want %s", i, check.Status, tt.wantStatus[i])
				}
				if !strings.Contains(check.Detail, tt.wantDetail[i]) {
					t.Fatalf("checks[%d].Detail = %q, want containing %q", i, check.Detail, tt.wantDetail[i])
				}
			}
		})
	}
}

func TestCheckDoctorProjectsExpandsSourceRootBeforeGit(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir() error = %v", err)
	}

	workflow := validDoctorWorkflow("~/repo")
	var gotPath string
	checks := checkDoctorProjects(context.Background(), globalconfig.Config{
		Projects: []globalconfig.Project{{ID: "alpha", Workflow: "WORKFLOW.md"}},
	}, doctorDeps{
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: workflow}, nil
		},
		gitWorkTree: func(_ context.Context, path string) error {
			gotPath = path
			return nil
		},
	})

	wantPath := filepath.Join(home, "repo")
	if gotPath != wantPath {
		t.Fatalf("git path = %q, want %q", gotPath, wantPath)
	}
	if len(checks) != 2 || checks[1].Status != doctorOK {
		t.Fatalf("checks = %#v, want source repo OK", checks)
	}
}

func TestCheckDoctorGitHub(t *testing.T) {
	t.Parallel()

	githubWorkflow := validDoctorWorkflow("/repo")
	githubWorkflow.Tracker.Kind = workflowconfig.TrackerGitHub
	githubWorkflow.Tracker.APIKey = "$PROJECT_TOKEN"
	githubAppWorkflow := validDoctorWorkflow("/repo")
	githubAppWorkflow.Tracker.Kind = workflowconfig.TrackerGitHub
	githubAppWorkflow.Tracker.GitHubAppID = "$APP_ID"
	githubAppWorkflow.Tracker.GitHubAppInstallationID = "$INSTALLATION_ID"
	githubAppWorkflow.Tracker.GitHubAppPrivateKey = "$PRIVATE_KEY"

	tests := []struct {
		name       string
		cfg        *globalconfig.Config
		env        map[string]string
		scopes     []string
		scopeErr   error
		workflow   workflowconfig.Config
		want       doctorStatus
		wantDetail string
	}{
		{
			name:       "missing token",
			want:       doctorFail,
			wantDetail: "GITHUB_TOKEN is not set",
		},
		{
			name:       "scope check fails",
			env:        map[string]string{"GITHUB_TOKEN": "token"},
			scopeErr:   errors.New("unauthorized"),
			want:       doctorFail,
			wantDetail: "scope check failed",
		},
		{
			name:       "missing required scope",
			env:        map[string]string{"GITHUB_TOKEN": "token"},
			scopes:     []string{"repo"},
			want:       doctorFail,
			wantDetail: "read:org",
		},
		{
			name: "non github projects skip token scopes",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			workflow:   validDoctorWorkflow("/repo"),
			want:       doctorWarn,
			wantDetail: "token scope check skipped",
		},
		{
			name: "github app credentials skip token scopes",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			env: map[string]string{
				"APP_ID":          "12345",
				"INSTALLATION_ID": "67890",
				"PRIVATE_KEY":     "private key",
			},
			scopeErr:   errors.New("scope check should not run"),
			workflow:   githubAppWorkflow,
			want:       doctorOK,
			wantDetail: "GitHub App credentials configured",
		},
		{
			name: "workflow token has required scopes",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			env:        map[string]string{"PROJECT_TOKEN": "token"},
			scopes:     []string{"project", "read:org", "repo"},
			want:       doctorOK,
			wantDetail: "PROJECT_TOKEN has required scopes",
		},
		{
			name:       "environment token has required scopes",
			env:        map[string]string{"GITHUB_TOKEN": "token"},
			scopes:     []string{"workflow", "project", "read:org", "repo"},
			want:       doctorOK,
			wantDetail: "GITHUB_TOKEN has required scopes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow := githubWorkflow
			if tt.workflow.Tracker.Kind != "" {
				workflow = tt.workflow
			}
			got := checkDoctorGitHub(context.Background(), tt.cfg, doctorDeps{
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: workflow}, nil
				},
				lookupEnv: func(key string) string {
					return tt.env[key]
				},
				githubScopes: func(context.Context, string) ([]string, error) {
					return tt.scopes, tt.scopeErr
				},
			})
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s", got.Status, tt.want)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Fatalf("Detail = %q, want containing %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestExpandDoctorWorkspacePath(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir() error = %v", err)
	}

	got, err := expandDoctorWorkspacePath("~/repo")
	if err != nil {
		t.Fatalf("expandDoctorWorkspacePath() error = %v", err)
	}
	want := filepath.Join(home, "repo")
	if got != want {
		t.Fatalf("expandDoctorWorkspacePath() = %q, want %q", got, want)
	}
}

func TestCheckDoctorSQLite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		openErr    error
		closeErr   error
		want       doctorStatus
		wantDetail string
	}{
		{
			name:       "missing config path",
			want:       doctorFail,
			wantDetail: "global config path is unavailable",
		},
		{
			name:       "open fails",
			path:       "/tmp/symphony/global.yaml",
			openErr:    errors.New("readonly"),
			want:       doctorFail,
			wantDetail: "readonly",
		},
		{
			name:       "close fails",
			path:       "/tmp/symphony/global.yaml",
			closeErr:   errors.New("close failed"),
			want:       doctorFail,
			wantDetail: "close failed",
		},
		{
			name:       "database reachable",
			path:       "/tmp/symphony/global.yaml",
			want:       doctorOK,
			wantDetail: "symphony.db is reachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkDoctorSQLite(context.Background(), globalconfig.PathResolution{Path: tt.path}, doctorDeps{
				openSQLite: func(_ context.Context, path string) (doctorStore, error) {
					if tt.openErr != nil {
						return nil, tt.openErr
					}
					if got := filepath.Base(path); got != "symphony.db" {
						t.Fatalf("store path base = %q, want symphony.db", got)
					}
					return fakeDoctorStore{closeErr: tt.closeErr}, nil
				},
			})
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s", got.Status, tt.want)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Fatalf("Detail = %q, want containing %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestCheckDoctorServerPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		listenErr  error
		closeErr   error
		want       doctorStatus
		wantDetail string
	}{
		{
			name:       "port unavailable",
			listenErr:  errors.New("address already in use"),
			want:       doctorFail,
			wantDetail: "address already in use",
		},
		{
			name:       "close fails after bind",
			closeErr:   errors.New("close failed"),
			want:       doctorWarn,
			wantDetail: "close failed",
		},
		{
			name:       "port available",
			want:       doctorOK,
			wantDetail: "is available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			port := 0
			got := checkDoctorServerPort(BootConfig{Host: "127.0.0.1", Port: &port}, doctorDeps{
				listen: func(_, address string) (net.Listener, error) {
					if address != "127.0.0.1:0" {
						t.Fatalf("listen address = %q, want 127.0.0.1:0", address)
					}
					if tt.listenErr != nil {
						return nil, tt.listenErr
					}
					return fakeDoctorListener{addr: fakeDoctorAddr("127.0.0.1:49152"), closeErr: tt.closeErr}, nil
				},
			})
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s", got.Status, tt.want)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Fatalf("Detail = %q, want containing %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestDoctorCommandExitStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		deps       doctorDeps
		wantErr    error
		wantOutput string
	}{
		{
			name:       "passes with warnings only",
			deps:       successfulDoctorDeps(),
			wantOutput: "Result: PASS",
		},
		{
			name: "fails when any check fails",
			deps: doctorDeps{
				runCommand: func(_ context.Context, path string, _ ...string) error {
					if strings.HasSuffix(path, "codex") {
						return errors.New("not runnable")
					}
					return nil
				},
			},
			wantErr:    ErrDoctorFailed,
			wantOutput: "Result: FAIL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "global.yaml")
			host := "127.0.0.1"
			port := 0
			opts := successfulDoctorOptions(configPath)
			deps := successfulDoctorDeps()
			if tt.deps.runCommand != nil {
				deps.runCommand = tt.deps.runCommand
			}
			if tt.deps.lookPath != nil {
				deps.lookPath = tt.deps.lookPath
			}

			cmd := newDoctorCommandWithDeps(&configPath, &host, &port, opts, deps)
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})

			err := cmd.Execute()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Execute() error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("output missing %q:\n%s", tt.wantOutput, stdout.String())
			}
		})
	}
}

func validDoctorWorkflow(sourceRoot string) workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = sourceRoot
	return cfg
}

func successfulDoctorOptions(configPath string) options {
	return options{
		resolvePath: func(string) (globalconfig.PathResolution, error) {
			return globalconfig.PathResolution{Path: configPath, Rule: globalconfig.PathRuleFlag}, nil
		},
		read: func(string) (globalconfig.Config, error) {
			return globalconfig.Config{
				Path:       configPath,
				APIVersion: globalconfig.APIVersion,
				Kind:       globalconfig.Kind,
				Global: globalconfig.Settings{
					MaxConcurrentAgents: 1,
					Scheduling:          globalconfig.SchedulingWeighted,
				},
				Projects: []globalconfig.Project{},
			}, nil
		},
	}
}

func successfulDoctorDeps() doctorDeps {
	return doctorDeps{
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: validDoctorWorkflow("/repo")}, nil
		},
		lookupEnv: func(key string) string {
			if key == "GITHUB_TOKEN" {
				return "token"
			}
			return ""
		},
		lookPath: func(binary string) (string, error) {
			return "/usr/bin/" + binary, nil
		},
		runCommand: func(context.Context, string, ...string) error {
			return nil
		},
		githubScopes: func(context.Context, string) ([]string, error) {
			return []string{"repo", "read:org", "project"}, nil
		},
		listen: func(string, string) (net.Listener, error) {
			return fakeDoctorListener{addr: fakeDoctorAddr("127.0.0.1:49152")}, nil
		},
		openSQLite: func(context.Context, string) (doctorStore, error) {
			return fakeDoctorStore{}, nil
		},
		gitWorkTree: func(context.Context, string) error {
			return nil
		},
	}
}

type fakeDoctorStore struct {
	closeErr error
}

func (s fakeDoctorStore) Close() error {
	return s.closeErr
}

type fakeDoctorListener struct {
	addr     net.Addr
	closeErr error
}

func (l fakeDoctorListener) Accept() (net.Conn, error) {
	return nil, errors.New("not implemented")
}

func (l fakeDoctorListener) Close() error {
	return l.closeErr
}

func (l fakeDoctorListener) Addr() net.Addr {
	return l.addr
}

type fakeDoctorAddr string

func (a fakeDoctorAddr) Network() string {
	return "tcp"
}

func (a fakeDoctorAddr) String() string {
	return string(a)
}
