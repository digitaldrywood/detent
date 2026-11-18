package cli

import (
	"context"
	"errors"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestDoctorTokenAuditCITriggerShape(t *testing.T) {
	for _, tt := range []struct {
		name, on, concurrency string
		want                  doctorStatus
	}{
		{"label no concurrency", "pull_request:\n    types: [labeled]", "", doctorWarn},
		{"label branch concurrency", "pull_request:\n    types: [labeled]", "concurrency: '${{ github.head_ref }}'", doctorWarn},
		{"label head concurrency", "pull_request:\n    types: [labeled]", "concurrency:\n  group: 'ci-${{ github.event.pull_request.head.sha }}'", doctorWarn},
		{"default PR events", "pull_request", "", doctorOK},
		{"push", "push", "", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			cfg.Gate.RequiredStatusChecks = []string{"Verify"}
			deps := doctorDeps{githubWorkflows: func(_ context.Context, _ workflowconfig.Config, repo string) (map[string]string, error) {
				if repo != "owner/repo" {
					t.Fatal(repo)
				}
				return map[string]string{"ci.yml": "on:\n  " + tt.on + "\n" + tt.concurrency + "\njobs:\n  verify:\n    name: Verify\n    runs-on: ubuntu-latest\n    steps: []\n"}, nil
			}}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditCIEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		readErr      bool
		want         doctorStatus
	}{
		{"job concurrency", "on:\n  pull_request:\n    types: [labeled]\njobs:\n  Verify:\n    concurrency: 'ci-${{ github.event.pull_request.head.sha }}'\n", false, doctorWarn},
		{"malformed", "on: [", false, doctorWarn},
		{"unmatched", "on: push\njobs:\n  other:\n    steps: []\n", false, doctorWarn},
		{"read denied", "", true, doctorWarn},
		{"empty", "", false, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Gate.RequiredStatusChecks = []string{"Verify"}
			deps := doctorDeps{githubWorkflows: func(context.Context, workflowconfig.Config, string) (map[string]string, error) {
				if tt.readErr {
					return nil, errors.New("denied")
				}
				return map[string]string{"ci.yml": tt.source}, nil
			}}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
