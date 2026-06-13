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
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
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
			}, "")
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

func TestProjectSourceRootPrefersProjectWorkdirBeforeWorkspaceRoot(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Workspace.Root = "/worktrees"
	project := globalconfig.Project{Workdir: "/source"}

	if got := projectSourceRoot(project, cfg); got != "/source" {
		t.Fatalf("projectSourceRoot() = %q, want /source", got)
	}

	cfg.Workspace.SourceRoot = "/configured-source"
	if got := projectSourceRoot(project, cfg); got != "/configured-source" {
		t.Fatalf("projectSourceRoot() with source_root = %q, want /configured-source", got)
	}
}

func TestCheckDoctorAutoPromote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	oldActivity := now.Add(-20 * time.Minute)
	prNumber := 42
	waitingIssue := doctorAutoPromoteIssue("issue-ci", &connector.PullRequest{
		Number:           41,
		URL:              "https://github.test/pull/41",
		State:            "OPEN",
		CIStatus:         "fail",
		CodexReviewState: "COMMENTED",
	})
	missingReviewIssue := doctorAutoPromoteIssue("issue-review", &connector.PullRequest{
		Number:   43,
		URL:      "https://github.test/pull/43",
		State:    "OPEN",
		CIStatus: "success",
	})
	linkedWithoutMetadata := doctorAutoPromoteIssue("issue-missing-pr", nil)
	linkedWithoutMetadata.PRNumber = &prNumber
	readyIssue := doctorAutoPromoteIssue("issue-ready", &connector.PullRequest{
		Number:                 44,
		URL:                    "https://github.test/pull/44",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldActivity,
	})

	tests := []struct {
		name        string
		cfg         workflowconfig.Config
		connector   *fakeDoctorAutoPromoteConnector
		want        doctorStatus
		wantDetails []string
	}{
		{
			name:        "disabled",
			cfg:         validDoctorWorkflow("/repo"),
			want:        doctorOK,
			wantDetails: []string{"disabled"},
		},
		{
			name: "human review observed state is not required",
			cfg: func() workflowconfig.Config {
				cfg := validDoctorAutoPromoteWorkflow()
				cfg.Tracker.ObservedStates = []string{"Blocked"}
				return cfg
			}(),
			connector:   &fakeDoctorAutoPromoteConnector{},
			want:        doctorOK,
			wantDetails: []string{"sampled 0 Human Review candidate"},
		},
		{
			name: "missing merging active state",
			cfg: func() workflowconfig.Config {
				cfg := validDoctorAutoPromoteWorkflow()
				cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}
				return cfg
			}(),
			want:        doctorFail,
			wantDetails: []string{"tracker.active_states", "Merging"},
		},
		{
			name: "status option verification fails",
			cfg:  validDoctorAutoPromoteWorkflow(),
			connector: &fakeDoctorAutoPromoteConnector{
				verifyErr: errors.New("github status option not found: Human Review maps to Reviewing"),
			},
			want:        doctorFail,
			wantDetails: []string{"status option", "Human Review", "Reviewing"},
		},
		{
			name: "linked pr missing metadata fails",
			cfg:  validDoctorAutoPromoteWorkflow(),
			connector: &fakeDoctorAutoPromoteConnector{
				issues: []connector.Issue{linkedWithoutMetadata},
			},
			want:        doctorFail,
			wantDetails: []string{"missing_pull_request", "linked PR #42", "issue-missing-pr"},
		},
		{
			name: "expected waiting reasons pass with counts",
			cfg:  validDoctorAutoPromoteWorkflow(),
			connector: &fakeDoctorAutoPromoteConnector{
				issues: []connector.Issue{waitingIssue, missingReviewIssue},
			},
			want:        doctorOK,
			wantDetails: []string{"automated_review_missing=1", "ci_not_green=1"},
		},
		{
			name: "ready candidate passes with count",
			cfg:  validDoctorAutoPromoteWorkflow(),
			connector: &fakeDoctorAutoPromoteConnector{
				issues: []connector.Issue{readyIssue},
			},
			want:        doctorOK,
			wantDetails: []string{"ready=1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := doctorDeps{}
			if tt.connector != nil {
				deps.autoPromoteConnector = func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
					return tt.connector, nil
				}
			}
			got := checkDoctorAutoPromote(context.Background(), "alpha", tt.cfg, deps, now)
			if got.Status != tt.want {
				t.Fatalf("Status = %s, want %s: %#v", got.Status, tt.want, got)
			}
			for _, want := range tt.wantDetails {
				if !strings.Contains(got.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", got.Detail, want)
				}
			}
		})
	}
}

func TestDoctorWorkflowDetailSurfacesIdentityAndAuthorization(t *testing.T) {
	t.Parallel()

	cfg := validDoctorWorkflow("/repo")
	cfg.Identity = workflowconfig.Identity{
		Name:          "release-captain",
		GitHubLogin:   "detent-bot",
		OwnershipMode: workflowconfig.IdentityOwnershipField,
		OwnerField:    "Owner",
	}
	cfg.Tracker.Authorization = selector.Selector{
		AssigneeIn: []string{"@me"},
	}
	project := globalconfig.Project{
		Authorization: selector.Selector{
			Labels: selector.Labels{Include: []string{"release"}},
		},
	}

	got := doctorWorkflowDetail("WORKFLOW.md", project, cfg)
	for _, want := range []string{
		"WORKFLOW.md is valid",
		"identity release-captain",
		"authorization selectors from global.yaml and WORKFLOW.md",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctorWorkflowDetail() = %q, want substring %q", got, want)
		}
	}
}

func TestCheckDoctorInstanceIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		identity   globalconfig.Identity
		want       doctorStatus
		wantDetail string
	}{
		{
			name:       "omitted identity is valid",
			want:       doctorOK,
			wantDetail: "not configured",
		},
		{
			name: "configured identity",
			identity: globalconfig.Identity{
				Name:          "release-captain",
				GitHubLogin:   "detent-bot",
				OwnershipMode: "field",
				OwnerField:    "Owner",
			},
			want:       doctorOK,
			wantDetail: "release-captain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkDoctorInstanceIdentity(globalconfig.Config{
				Global: globalconfig.Settings{Identity: tt.identity},
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
	}, "")

	wantPath := filepath.Join(home, "repo")
	if gotPath != wantPath {
		t.Fatalf("git path = %q, want %q", gotPath, wantPath)
	}
	if len(checks) != 2 || checks[1].Status != doctorOK {
		t.Fatalf("checks = %#v, want source repo OK", checks)
	}
}

func TestCheckDoctorRuntimeSettingsReportsSources(t *testing.T) {
	t.Parallel()

	got := checkDoctorRuntimeSettings(RuntimeSettings{
		ConfigPath:  RuntimeValue{Value: "/tmp/global.yaml", Source: string(globalconfig.PathRuleFlag)},
		Env:         RuntimeValue{Value: "prod", Source: runtimeSourceDefault},
		LogLevel:    RuntimeValue{Value: "debug", Source: "LOG_LEVEL"},
		Port:        RuntimeIntValue{Value: 4000, Source: runtimeSourceConfig},
		GitHubToken: RuntimeSecret{Value: "secret-token", Source: "github_token", ResolvedVia: "gh"},
	})

	if got.Status != doctorOK {
		t.Fatalf("Status = %s, want %s", got.Status, doctorOK)
	}
	for _, want := range []string{
		"config_path=/tmp/global.yaml (--config)",
		"env=prod (default)",
		"log_level=debug (LOG_LEVEL)",
		"port=4000 (config)",
		"github_token=resolved via gh",
	} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("Detail missing %q:\n%s", want, got.Detail)
		}
	}
	if strings.Contains(got.Detail, "secret-token") {
		t.Fatalf("Detail leaked token: %s", got.Detail)
	}
}

func TestCheckDoctorGitHub(t *testing.T) {
	t.Parallel()

	githubWorkflow := validDoctorWorkflow("/repo")
	githubWorkflow.Tracker.Kind = workflowconfig.TrackerGitHub
	githubWorkflow.Tracker.APIKey = "$PROJECT_TOKEN"

	tests := []struct {
		name       string
		cfg        *globalconfig.Config
		token      RuntimeSecret
		scopes     []string
		scopeErr   error
		workflow   workflowconfig.Config
		env        map[string]string
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
			token:      RuntimeSecret{Value: "token", Source: "GITHUB_TOKEN"},
			scopeErr:   errors.New("unauthorized"),
			want:       doctorFail,
			wantDetail: "scope check failed",
		},
		{
			name:       "missing required scope",
			token:      RuntimeSecret{Value: "token", Source: "GITHUB_TOKEN"},
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
			name: "github app projects skip token scopes",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			workflow: githubAppWorkflow(),
			env: map[string]string{
				"APP_ID":           "12345",
				"INSTALLATION_ID":  "67890",
				"PRIVATE_KEY_PATH": ".detent/github-app.pem",
			},
			want:       doctorWarn,
			wantDetail: "GitHub App credentials configured",
		},
		{
			name: "github app env refs missing require token",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			workflow:   githubAppWorkflow(),
			want:       doctorFail,
			wantDetail: "GITHUB_TOKEN is not set",
		},
		{
			name: "workflow token has required scopes",
			cfg: &globalconfig.Config{Projects: []globalconfig.Project{
				{ID: "alpha", Workflow: "WORKFLOW.md"},
			}},
			token:      RuntimeSecret{Value: "token", Source: "PROJECT_TOKEN"},
			scopes:     []string{"project", "read:org", "repo"},
			want:       doctorOK,
			wantDetail: "PROJECT_TOKEN has required scopes",
		},
		{
			name:       "environment token has required scopes",
			token:      RuntimeSecret{Value: "token", Source: "GITHUB_TOKEN"},
			scopes:     []string{"workflow", "project", "read:org", "repo"},
			want:       doctorOK,
			wantDetail: "GITHUB_TOKEN has required scopes",
		},
		{
			name:       "gh sentinel token has required scopes",
			token:      RuntimeSecret{Value: "token", Source: "github_token", ResolvedVia: "gh"},
			scopes:     []string{"project", "read:org", "repo"},
			want:       doctorOK,
			wantDetail: "github_token resolved via gh has required scopes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow := githubWorkflow
			if tt.workflow.Tracker.Kind != "" {
				workflow = tt.workflow
			}
			got := checkDoctorGitHub(context.Background(), tt.cfg, tt.token, doctorDeps{
				lookupEnv: mapLookup(tt.env),
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: workflow}, nil
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
			path:       "/tmp/detent/global.yaml",
			openErr:    errors.New("readonly"),
			want:       doctorFail,
			wantDetail: "readonly",
		},
		{
			name:       "close fails",
			path:       "/tmp/detent/global.yaml",
			closeErr:   errors.New("close failed"),
			want:       doctorFail,
			wantDetail: "close failed",
		},
		{
			name:       "database reachable",
			path:       "/tmp/detent/global.yaml",
			want:       doctorOK,
			wantDetail: "detent.db is reachable",
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
					if got := filepath.Base(path); got != "detent.db" {
						t.Fatalf("store path base = %q, want detent.db", got)
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

func TestDoctorSQLitePingErrorWrapsPingAndCloseErrors(t *testing.T) {
	t.Parallel()

	pingErr := errors.New("ping failed")
	closeErr := errors.New("close failed")

	err := doctorSQLitePingError(pingErr, closeErr)
	if !errors.Is(err, pingErr) {
		t.Fatalf("doctorSQLitePingError() error = %v, want ping error in chain", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("doctorSQLitePingError() error = %v, want close error in chain", err)
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
			env := ""
			logLevel := ""
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

			cmd := newDoctorCommandWithDeps(&configPath, &env, &logLevel, &host, &port, opts, deps)
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

func validDoctorAutoPromoteWorkflow() workflowconfig.Config {
	cfg := validDoctorWorkflow("/repo")
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.APIKey = "token"
	cfg.Tracker.ProjectSlug = "PVT_1"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	cfg.Tracker.ObservedStates = []string{"Backlog", "Human Review", "Blocked"}
	cfg.Agent.AutoPromote.Enabled = true
	cfg.Agent.AutoPromote.QuietSeconds = 600
	return cfg
}

func doctorAutoPromoteIssue(id string, pullRequest *connector.PullRequest) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#399"
	issue.Title = "Auto promote diagnostic"
	issue.State = "Human Review"
	issue.PullRequest = pullRequest
	return issue
}

type fakeDoctorAutoPromoteConnector struct {
	issues    []connector.Issue
	verifyErr error
}

func (c *fakeDoctorAutoPromoteConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return c.issues, nil
}

func (c *fakeDoctorAutoPromoteConnector) FetchIssuesByStatesLimit(context.Context, []string, int) ([]connector.Issue, error) {
	return c.issues, nil
}

func (c *fakeDoctorAutoPromoteConnector) VerifyStatusOptions(context.Context, []string) error {
	return c.verifyErr
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
