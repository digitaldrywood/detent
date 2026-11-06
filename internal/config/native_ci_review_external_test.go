package config_test

import (
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
	"testing"
)

func TestNativeGitHubCIReviewPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		tracker string
		github  bool
		floor   int
	}{
		{name: "native GitHub CI is a landing gate", tracker: "hub_native", github: true},
		{name: "native local checks retain their review floor", tracker: "hub_native", floor: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := "---\ntracker:\n  kind: " + test.tracker + "\n  api_key: fixture-token\ngate:\n  run: true\n  required_status_checks: ['Full CI']\n  ci_trigger_label: run-full-ci\n  validator:\n    enabled: true\nreview:\n  human: true\ndeliverable:\n  github_pull_request: " + map[bool]string{true: "true", false: "false"}[test.github] + "\n---\nComplete the work.\n"
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte(source)})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil || descriptor.Gates.RequiredChecks != test.floor || !descriptor.Gates.Validator || !descriptor.Gates.HumanReview {
				t.Fatalf("CI projection changed native review gates: %+v, %v", descriptor, err)
			}
			rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
			if err := changerequest.ValidatePolicy(rules, descriptor); (err == nil) != (test.floor == 0) {
				t.Fatalf("GitHub CI prevented native publication or waived native checks: %v", err)
			}
			if test.floor == 0 {
				if descriptor.Gates.GitHubCIDigest == "" {
					t.Fatal("remote CI contract is absent from reviewed gate identity")
				}
				workflow.Config.Gate.RequiredStatusChecks = []string{"Other CI"}
				changed, err := config.ResolvePolicy(workflow)
				if err != nil || changed.Gates.GitHubCIDigest == descriptor.Gates.GitHubCIDigest || changed.Gates.RequiredChecks != 0 {
					t.Fatalf("same-count CI replacement lost approval fencing: %+v, %v", changed, err)
				}
			} else if descriptor.Gates.GitHubCIDigest != "" {
				t.Fatal("native GitHub CI projection escaped its delivery mode")
			}
		})
	}
}
