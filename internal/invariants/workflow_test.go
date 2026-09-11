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
const placeholderCondition = "github.event_name == 'pull_request'"
const integrationCondition = "github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/main')"

func checkWorkflow(data []byte) error {
	var workflow struct {
		On struct {
			PullRequest struct {
				Types []string `yaml:"types"`
			} `yaml:"pull_request"`
			MergeGroup struct {
				Types []string `yaml:"types"`
			} `yaml:"merge_group"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	if !slices.Contains(workflow.On.MergeGroup.Types, "checks_requested") {
		return errors.New("INV-4 merge_group checks_requested missing")
	}
	events := workflow.On.PullRequest.Types
	for _, event := range []string{"opened", "synchronize", "reopened", "ready_for_review"} {
		if !slices.Contains(events, event) {
			return fmt.Errorf("INV-5 missing PR activity %s", event)
		}
	}
	for _, event := range events {
		if !slices.Contains([]string{"opened", "synchronize", "reopened", "ready_for_review"}, event) {
			return fmt.Errorf("INV-5 extra PR activity %s", event)
		}
	}
	for _, required := range []string{"pr-required-placeholders", "invariants", "lint", "verify", "verify-fast", "verify-race", "test-cover", "security", "browser-visual", "portability-verify", "windows-core", "installer-smoke", "goreleaser-snapshot"} {
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
				return errors.New("INV-5 integration failure reporting must exclude PRs")
			}
		case "verify":
			if condition != "always() && "+nonPRCondition {
				return errors.New("INV-5 Verify must aggregate results and never run on pull_request events")
			}
		case "pr-required-placeholders":
			if condition != placeholderCondition {
				return errors.New("INV-5 placeholder checks must run only on pull_request events")
			}
		default:
			if condition != nonPRCondition {
				return fmt.Errorf("INV-5 %s must not run on pull_request events; real CI runs in the merge queue and on main", name)
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

func TestWorkflowViolations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, old, replacement string }{
		{"real job on PR", "if: " + nonPRCondition, "if: true"},
		{"pr bypass", "if: " + nonPRCondition, "if: " + nonPRCondition + " || true"},
		{"placeholder everywhere", "if: " + placeholderCondition, "if: true"},
		{"missing merge group", "merge_group:", "unused_event:"},
		{"label reruns", "types: [opened,", "types: [labeled, opened,"},
		{"integration on PR", "if: " + integrationCondition, "if: " + nonPRCondition},
		{"missing invariant job", "  invariants:", "  renamed:"},
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
