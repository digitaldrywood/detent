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
	if len(workflow.On) != 2 {
		return errors.New("INV-5 CI must trigger only on main push and workflow_dispatch")
	}
	if _, ok := workflow.On["push"]; !ok {
		return errors.New("INV-5 main push trigger missing")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		return errors.New("INV-5 workflow_dispatch trigger missing")
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
			if condition != "always()" {
				return fmt.Errorf("INV-5 %s must aggregate results on main push and manual dispatch", name)
			}
		default:
			if condition != "" {
				return fmt.Errorf("INV-5 %s must run on main push and manual dispatch", name)
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

func TestRepositoryHasNoPullRequestActions(t *testing.T) {
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
			for _, event := range []string{"pull_request", "pull_request_target", "merge_group"} {
				if _, ok := workflow.On[event]; ok {
					t.Fatalf("INV-5 %s must not trigger on %s", entry.Name(), event)
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
		{"pull request trigger", "  workflow_dispatch:", "  pull_request:\n  workflow_dispatch:"},
		{"merge group trigger", "  workflow_dispatch:", "  merge_group:\n  workflow_dispatch:"},
		{"nightly trigger", "  workflow_dispatch:", "  schedule:\n    - cron: '0 0 * * *'\n  workflow_dispatch:"},
		{"manual dispatch dropped", "  workflow_dispatch:", "  unused_event:"},
		{"integration develop push", "if: " + integrationCondition, "if: github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/develop')"},
		{"failure reports on develop", "if: failure() && github.ref == 'refs/heads/main'", "if: failure() && (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/develop')"},
		{"filtered real job", "  lint:\n", "  lint:\n    if: false\n"},
		{"placeholder", "jobs:", "jobs:\n  placeholder:\n    if: github.event_name == 'pull_request'\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo skipped"},
		{"integration on PR", "if: " + integrationCondition, "if: " + nonPRCondition},
		{"missing invariant job", "  invariants:", "  renamed:"},
		{"aggregate filtered", "  browser-visual:\n", "  browser-visual:\n    if: false\n"},
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
