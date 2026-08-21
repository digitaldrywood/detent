package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorConfiguredLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configure  func(*workflowconfig.Config)
		labels     []string
		wantStatus doctorStatus
		wantDetail []string
		wantHint   []string
		wantReads  int
	}{
		{
			name: "labels present",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Agent.AutoPromote.OptoutLabel = "requires-human-review"
				cfg.Agent.MaxSessionTokenOverrideLabel = "allow-large-session"
				cfg.Agent.LifetimeLimitOverrideLabel = "allow-lifetime-limit"
			},
			labels:     []string{"Requires-Human-Review", "allow-large-session", "allow-lifetime-limit"},
			wantStatus: doctorOK,
			wantDetail: []string{"verified 3 configured labels", "digitaldrywood/detent"},
			wantReads:  1,
		},
		{
			name: "opt-out label absent",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Agent.AutoPromote.OptoutLabel = "requires-human-review"
			},
			labels:     []string{"bug"},
			wantStatus: doctorFail,
			wantDetail: []string{`missing label "requires-human-review"`, "agent.auto_promote.optout_label"},
			wantHint:   []string{"gh label create", "requires-human-review", "--repo", "digitaldrywood/detent"},
			wantReads:  1,
		},
		{
			name: "config keys unset",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Agent.AutoPromote.OptoutLabel = ""
				cfg.Agent.MaxSessionTokenOverrideLabel = ""
				cfg.Agent.LifetimeLimitOverrideLabel = ""
			},
			wantStatus: doctorOK,
			wantDetail: []string{"no escape-hatch labels are configured"},
		},
		{
			name: "multiple labels missing",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Agent.AutoPromote.OptoutLabel = "requires-human-review"
				cfg.Agent.MaxSessionTokenOverrideLabel = "allow-large-session"
			},
			wantStatus: doctorFail,
			wantDetail: []string{
				`missing label "requires-human-review" referenced by agent.auto_promote.optout_label`,
				`missing label "allow-large-session" referenced by agent.max_session_token_override_label`,
				`missing label "allow-lifetime-limit" referenced by agent.lifetime_limit_override_label`,
			},
			wantHint:  []string{"gh label create", "requires-human-review", "allow-large-session", "allow-lifetime-limit"},
			wantReads: 1,
		},
		{
			name: "session override label absent warns",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Agent.AutoPromote.OptoutLabel = ""
				cfg.Agent.MaxSessionTokenOverrideLabel = "allow-large-session"
				cfg.Agent.LifetimeLimitOverrideLabel = ""
			},
			wantStatus: doctorWarn,
			wantDetail: []string{"agent.max_session_token_override_label"},
			wantHint:   []string{"gh label create", "allow-large-session"},
			wantReads:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerGitHub
			cfg.Tracker.Repository = "digitaldrywood/detent"
			tt.configure(&cfg)
			reads := 0
			check := checkDoctorConfiguredLabels(context.Background(), "detent", globalconfig.Project{}, cfg, doctorDeps{
				githubLabels: func(context.Context, workflowconfig.Config, string) ([]string, error) {
					reads++
					return tt.labels, nil
				},
			})

			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s: %#v", check.Status, tt.wantStatus, check)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			for _, want := range tt.wantHint {
				if !strings.Contains(check.Hint, want) {
					t.Fatalf("Hint = %q, want containing %q", check.Hint, want)
				}
			}
			if reads != tt.wantReads {
				t.Fatalf("repository label reads = %d, want %d", reads, tt.wantReads)
			}
		})
	}
}

func TestCheckDoctorConfiguredLabelsReportsInventoryFailure(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "digitaldrywood/detent"
	check := checkDoctorConfiguredLabels(context.Background(), "detent", globalconfig.Project{}, cfg, doctorDeps{
		githubLabels: func(context.Context, workflowconfig.Config, string) ([]string, error) {
			return nil, errors.New("permission denied")
		},
	})

	if check.Status != doctorFail || !strings.Contains(check.Detail, "permission denied") {
		t.Fatalf("check = %#v, want failed repository label inventory", check)
	}
}

func TestCheckDoctorConfiguredLabelsUsesTrackerRepository(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "digitaldrywood/detent"
	cfg.Tracker.WriteProbeIssue = "digitaldrywood/scratch#1"
	repositories := []string{}
	check := checkDoctorConfiguredLabels(
		context.Background(),
		"detent",
		globalconfig.Project{Workdir: "digitaldrywood/worktree#1"},
		cfg,
		doctorDeps{
			githubLabels: func(_ context.Context, _ workflowconfig.Config, repository string) ([]string, error) {
				repositories = append(repositories, repository)
				return []string{"requires-human-review", "allow-large-session", "allow-lifetime-limit"}, nil
			},
			gitRemoteURL: func(context.Context, string) (string, error) {
				return "https://github.com/digitaldrywood/checkout.git", nil
			},
		},
	)

	if check.Status != doctorOK {
		t.Fatalf("Status = %s, want %s: %#v", check.Status, doctorOK, check)
	}
	if len(repositories) != 1 || repositories[0] != cfg.Tracker.Repository {
		t.Fatalf("repository label reads = %v, want [%s]", repositories, cfg.Tracker.Repository)
	}
}

func TestDoctorConfiguredLabelFixTerminatesFlagParsing(t *testing.T) {
	t.Parallel()

	fix := doctorConfiguredLabelFix("digitaldrywood/detent", doctorConfiguredLabel{
		Name:        "-escape",
		Description: "Escape hatch.",
		Color:       "b60205",
	})
	want := "gh label create --repo 'digitaldrywood/detent' --color b60205 --description 'Escape hatch.' -- '-escape'"
	if fix != want {
		t.Fatalf("fix = %q, want %q", fix, want)
	}
}

func TestCheckDoctorProjectIncludesConfiguredLabelCheck(t *testing.T) {
	t.Parallel()

	cfg := validDoctorWorkflow("/repo")
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.APIKey = "token"
	cfg.Tracker.ProjectSlug = "PVT_1"
	cfg.Tracker.Repository = "digitaldrywood/detent"
	deps := successfulDoctorDeps()
	deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
		return workflowconfig.Workflow{Config: cfg}, nil
	}
	deps.githubLabels = func(context.Context, workflowconfig.Config, string) ([]string, error) {
		return []string{"bug"}, nil
	}

	checks := checkDoctorProject(context.Background(), globalconfig.Project{ID: "detent", Workflow: "WORKFLOW.md"}, deps, RuntimeSecret{}, false)
	for _, check := range checks {
		if check.Name == "Project detent configured labels" {
			if check.Status != doctorFail || !strings.Contains(check.Detail, "agent.auto_promote.optout_label") {
				t.Fatalf("configured label check = %#v, want missing opt-out failure", check)
			}
			return
		}
	}
	t.Fatalf("checks = %#v, want configured label check", checks)
}
