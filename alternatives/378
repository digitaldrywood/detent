package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorAdmissionMaximumGap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schedule string
		want     time.Duration
	}{
		{name: "quarter hourly", schedule: "*/15 * * * *", want: 15 * time.Minute},
		{name: "hourly", schedule: "0 * * * *", want: time.Hour},
		{name: "weekdays", schedule: "0 6 * * 1-5", want: 72 * time.Hour},
		{name: "configured timezone", schedule: "CRON_TZ=America/New_York 0 6 * * 1-5", want: 73 * time.Hour},
		{name: "invalid", schedule: "not cron", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorAdmissionMaximumGap(tt.schedule); got != tt.want {
				t.Fatalf("doctorAdmissionMaximumGap(%q) = %s, want %s", tt.schedule, got, tt.want)
			}
		})
	}
}

func TestDoctorAdmissionWarnsForStatesOnlyPublicExposure(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "digitaldrywood/detent"
	cfg.BacklogAdmission.Enabled = true
	cfg.BacklogAdmission.CriteriaSection = "Admission criteria"
	cfg.BacklogAdmission.Sources.States = []string{"Backlog"}
	check := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config: cfg,
		SharedPrompt: `## Admission criteria

- **Risk** — requires a bounded recovery path.
`,
	}, doctorDeps{
		githubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
			return ghconnector.RepositoryInfo{Visibility: "public"}, nil
		},
	})
	if check.BacklogAdmission == nil ||
		check.BacklogAdmission.RepositoryVisibility != "public" ||
		!strings.Contains(strings.Join(check.BacklogAdmission.Warnings, "; "), "untrusted issue authors") {
		t.Fatalf("check = %#v", check)
	}
}

func TestDoctorAdmissionResolvesCriteriaSection(t *testing.T) {
	cfg := workflowconfig.Default()
	cfg.BacklogAdmission.Enabled = true
	cfg.BacklogAdmission.CriteriaSection = "Admission criteria"

	unresolved := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config:       cfg,
		SharedPrompt: "# Workflow\n",
	}, doctorDeps{})
	if unresolved.Status != doctorWarn || !strings.Contains(unresolved.Detail, "was not found") {
		t.Fatalf("unresolved check = %#v", unresolved)
	}

	resolved := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config: cfg,
		SharedPrompt: `# Workflow

## Admission criteria

- **Risk** — requires a bounded recovery path.
`,
	}, doctorDeps{})
	if resolved.Status != doctorOK || !strings.Contains(resolved.Detail, "1 project-defined dimensions") {
		t.Fatalf("resolved check = %#v", resolved)
	}
	if resolved.BacklogAdmission == nil || len(resolved.BacklogAdmission.Dimensions) != 1 {
		t.Fatalf("BacklogAdmission = %#v", resolved.BacklogAdmission)
	}
}
