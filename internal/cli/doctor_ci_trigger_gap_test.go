package cli

import (
	"context"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorRequiredStatusTriggerGap(t *testing.T) {
	for _, tt := range []struct{ name, on, condition, label, want string }{
		{"ruleset label only", "pull_request: {types: [labeled]}", "github.event.label.name == 'run-full-ci'", "", "gate.ci_trigger_label: run-full-ci"},
		{"target scalar", "pull_request_target", "", "", "cannot trigger"},
		{"target mapping", "pull_request_target:", "", "", "cannot trigger"},
		{"target sequence", "[pull_request_target, workflow_dispatch]", "", "", "cannot trigger"},
		{"target labeled configured", "pull_request_target: {types: [labeled]}", "", "run-full-ci", "cannot trigger"},
		{"target with head producer", "[pull_request_target, pull_request]", "", "", ""},
		{"manual", "workflow_dispatch:", "", "", "cannot trigger"},
		{"contains label", "pull_request: {types: [labeled]}", "contains(github.event.pull_request.labels.*.name, 'run-full-ci')", "", "gate.ci_trigger_label: run-full-ci"},
		{"unconditional labeled configured", "pull_request: {types: [labeled]}", "", "run-full-ci", ""},
		{"opened without synchronize", "pull_request: {types: [opened]}", "", "", "cannot trigger"},
		{"filtered push", "push: {branches: [main]}", "", "", "cannot trigger"},
		{"explicit automatic", "pull_request: {types: [opened, synchronize]}", "", "", ""},
		{"sequence automatic", "[pull_request, workflow_dispatch]", "", "", ""},
		{"conditional automatic", "pull_request:", "github.event.label.name == 'run-full-ci'", "", "cannot trigger"},
		{"always aggregator", "pull_request:", "always()", "", ""},
		{"expression always aggregator", "pull_request:", "${{ always() }}", "", ""},
		{"automatic", "pull_request:", "", "", ""},
		{"configured label", "pull_request: {types: [labeled]}", "github.event.label.name == 'run-full-ci'", "run-full-ci", ""},
		{"wrong label", "pull_request: {types: [labeled]}", "github.event.label.name == 'run-full-ci'", "wrong", "gate.ci_trigger_label: run-full-ci"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind, cfg.Tracker.Repository = "github", "owner/repo"
			cfg.Gate.CITriggerLabel = tt.label
			deps := doctorDeps{
				githubBranchPolicy: func(context.Context, workflowconfig.Config, string) (ghconnector.BranchMergePolicy, error) {
					return ghconnector.BranchMergePolicy{Branch: "main", RequiredStatusChecks: []string{"Full CI"}}, nil
				},
				githubWorkflows: func(context.Context, workflowconfig.Config, string) (map[string]string, error) {
					return map[string]string{"ci.yml": "on:\n  " + tt.on + "\nconcurrency: ci-${{ github.event.pull_request.head.sha }}\njobs:\n  full:\n    name: Full CI\n    if: " + tt.condition + "\n    runs-on: ubuntu-latest\n    steps: []\n"}, nil
				},
			}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if tt.want != "" {
				if got.Status != doctorWarn || !strings.Contains(got.Detail, "Full CI") || !strings.Contains(got.Detail, tt.want) {
					t.Fatalf("got %+v", got)
				}
			} else if got.Status != doctorOK {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorLegacyStatusPublisher(t *testing.T) {
	for _, condition := range []string{"github.event.label.name == 'run-full-ci'", "${{ github.event.label.name == 'run-full-ci' }}"} {
		t.Run(condition, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Gate.RequiredStatusChecks = []string{"Full CI"}
			deps := doctorDeps{githubWorkflows: func(context.Context, workflowconfig.Config, string) (map[string]string, error) {
				return map[string]string{"ci.yml": "on: {pull_request: {types: [labeled]}}\nconcurrency: ci-${{ github.event.pull_request.head.sha }}\njobs:\n  publish:\n    steps:\n      - if: " + condition + "\n        run: |\n          gh api statuses -f context='Full CI'\n"}, nil
			}}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if got.Status != doctorWarn || !strings.Contains(got.Detail, "gate.ci_trigger_label: run-full-ci") {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
