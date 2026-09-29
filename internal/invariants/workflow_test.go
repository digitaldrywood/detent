package invariants

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const nonPRCondition = "github.event_name != 'pull_request'"
const integrationCondition = "github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/main')"

func checkWorkflow(data []byte) error {
	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	if len(workflow.On) != 4 {
		return errors.New("INV-5 CI must trigger only on main push, pull requests into main, merge groups and workflow_dispatch")
	}
	for _, event := range []string{"push", "pull_request", "merge_group", "workflow_dispatch"} {
		if _, ok := workflow.On[event]; !ok {
			return fmt.Errorf("INV-5 %s trigger missing", event)
		}
	}
	var push struct {
		Branches []string `yaml:"branches"`
		Tags     []string `yaml:"tags"`
	}
	pushNode := workflow.On["push"]
	if err := pushNode.Decode(&push); err != nil {
		return fmt.Errorf("INV-5 decode push trigger: %w", err)
	}
	if !slices.Equal(push.Branches, []string{"main"}) || len(push.Tags) != 0 {
		return errors.New("INV-5 CI push must run only on main; tag checks invalidate release provenance")
	}
	var pullRequest struct {
		Branches []string `yaml:"branches"`
	}
	pullRequestNode := workflow.On["pull_request"]
	if err := pullRequestNode.Decode(&pullRequest); err != nil {
		return fmt.Errorf("INV-5 decode pull_request trigger: %w", err)
	}
	if !slices.Equal(pullRequest.Branches, []string{"main"}) {
		return errors.New("INV-5 CI pull requests must target only main; develop pull requests use the local gate")
	}
	var mergeGroup struct {
		Branches []string `yaml:"branches"`
	}
	mergeGroupNode := workflow.On["merge_group"]
	if err := mergeGroupNode.Decode(&mergeGroup); err != nil {
		return fmt.Errorf("INV-5 decode merge_group trigger: %w", err)
	}
	if !slices.Equal(mergeGroup.Branches, []string{"main"}) {
		return errors.New("INV-5 CI merge groups must target only main")
	}
	for _, required := range []string{"invariants", "lint", "verify", "verify-fast", "verify-race", "test-cover", "security", "browser-visual", "browser-visual-shard", "portability-verify", "windows-core", "installer-smoke", "goreleaser-snapshot"} {
		if _, ok := workflow.Jobs[required]; !ok {
			return fmt.Errorf("required CI job %s missing", required)
		}
	}
	for name, job := range workflow.Jobs {
		condition := strings.TrimSpace(job.If)
		switch name {
		case "portability-verify", "windows-core", "installer-smoke", "goreleaser-snapshot":
			if condition != integrationCondition {
				return fmt.Errorf("INV-5 %s must run only on main push or explicit manual dispatch", name)
			}
		case "report-integration-failures":
			if condition != "failure() && github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch')" {
				return errors.New("INV-5 integration failure reporting must stay on main")
			}
		case "verify", "browser-visual":
			if condition != "always() && "+nonPRCondition {
				return fmt.Errorf("INV-5 %s must aggregate results outside pull requests", name)
			}
		default:
			if condition != nonPRCondition {
				return fmt.Errorf("INV-5 %s must report skipped on pull requests and run in the merge group", name)
			}
		}
	}
	return nil
}

func TestRepositoryWorkflow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkWorkflow(data); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPullRequestActionsOnlyForMain(t *testing.T) {
	workflows := filepath.Join(repositoryRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(workflows)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yml" && filepath.Ext(entry.Name()) != ".yaml") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(workflows, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				On map[string]yaml.Node `yaml:"on"`
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			if _, ok := workflow.On["pull_request_target"]; ok {
				t.Fatalf("INV-5 %s must not trigger on pull_request_target", entry.Name())
			}
			if entry.Name() == "ci.yml" {
				return
			}
			for _, event := range []string{"pull_request", "merge_group"} {
				if _, ok := workflow.On[event]; ok {
					t.Fatalf("INV-5 %s must not trigger on %s; only ci.yml gates pull requests into main", entry.Name(), event)
				}
			}
		})
	}
}

func TestPortabilityStressIsManualOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "portability-stress.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On map[string]yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 {
		t.Fatalf("INV-5 portability stress triggers = %v, want only workflow_dispatch", workflow.On)
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("INV-5 portability stress must allow manual dispatch")
	}
}

func TestWorkflowViolations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, old, replacement string }{
		{"duplicate tag checks", "branches: [main]", "branches: [main]\n    tags: ['v*']"},
		{"unfiltered push", "branches: [main]", "branches: []"},
		{"develop push", "branches: [main]", "branches: [main, develop]"},
		{"main push dropped", "branches: [main]", "branches: [develop]"},
		{"feature branch push", "branches: [main]", "branches: [main, 'feature/*']"},
		{"develop pull requests", "    branches: [main]\n    types:", "    branches: [main, develop]\n    types:"},
		{"pull requests into any branch", "    branches: [main]\n    types:", "    types:"},
		{"merge group dropped", "  merge_group:\n    branches: [main]\n    types: [checks_requested]\n", ""},
		{"merge group on develop", "  merge_group:\n    branches: [main]\n", "  merge_group:\n    branches: [main, develop]\n"},
		{"merge group on any branch", "  merge_group:\n    branches: [main]\n", "  merge_group:\n"},
		{"nightly trigger", "  workflow_dispatch:", "  schedule:\n    - cron: '0 0 * * *'\n  workflow_dispatch:"},
		{"manual dispatch dropped", "  workflow_dispatch:", "  unused_event:"},
		{"integration develop push", "if: " + integrationCondition, "if: github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/develop')"},
		{"failure reports on develop", "if: failure() && github.ref == 'refs/heads/main'", "if: failure() && (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/develop')"},
		{"filtered real job", "  lint:\n    if: " + nonPRCondition + "\n", "  lint:\n    if: false\n"},
		{"real job runs on pull requests", "  lint:\n    if: " + nonPRCondition + "\n", "  lint:\n"},
		{"placeholder", "jobs:", "jobs:\n  placeholder:\n    if: github.event_name == 'pull_request'\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo skipped"},
		{"integration on PR", "if: " + integrationCondition, "if: " + nonPRCondition},
		{"missing invariant job", "  invariants:", "  renamed:"},
		{"aggregate filtered", "if: always() && " + nonPRCondition, "if: false"},
		{"malformed", "name: CI", "name: ["},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(string(data), tt.old, tt.replacement, 1)
			if changed == string(data) {
				t.Fatal("fixture replacement did not match")
			}
			if err := checkWorkflow([]byte(changed)); err == nil {
				t.Fatal("forbidden workflow passed")
			}
		})
	}
}
