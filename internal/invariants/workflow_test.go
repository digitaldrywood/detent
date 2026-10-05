package invariants

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
		return errors.New("INV-5 CI must trigger only on a schedule and manual dispatch")
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		if _, ok := workflow.On[event]; !ok {
			return fmt.Errorf("INV-5 %s trigger missing", event)
		}
	}
	var schedule []struct {
		Cron string `yaml:"cron"`
	}
	scheduleNode := workflow.On["schedule"]
	if err := scheduleNode.Decode(&schedule); err != nil || len(schedule) != 1 || schedule[0].Cron != "17 * * * *" {
		return errors.New("INV-5 CI must run hourly at minute 17")
	}
	for _, required := range []string{"preflight", "invariants", "lint", "verify-fast", "verify-race", "test-cover", "security", "browser-visual-shard", "portability-verify", "windows-core", "installer-smoke", "goreleaser-snapshot", "finalize"} {
		if _, ok := workflow.Jobs[required]; !ok {
			return fmt.Errorf("INV-5 required CI job %s missing", required)
		}
	}
	for name, job := range workflow.Jobs {
		condition := strings.TrimSpace(job.If)
		switch name {
		case "preflight":
			if condition != "" {
				return errors.New("INV-5 preflight must always run")
			}
		case "finalize":
			if condition != "always() && (needs.preflight.outputs.should_run == 'true' || needs.preflight.result == 'failure')" {
				return errors.New("INV-5 finalizer must inspect every attempted full run")
			}
		default:
			if condition != "needs.preflight.outputs.should_run == 'true'" {
				return fmt.Errorf("INV-5 %s must run when preflight finds new work", name)
			}
		}
	}
	if !strings.Contains(string(data), "git ls-remote origin refs/heads/develop") || strings.Count(string(data), "ref: ${{ needs.preflight.outputs.develop_sha }}") < 12 {
		return errors.New("INV-5 CI must validate the pinned development SHA")
	}
	if !strings.Contains(string(data), "scripts/scheduled-ci-finish.sh") {
		return errors.New("INV-5 CI must publish the validated tag or report failures")
	}
	return nil
}

func TestRepositoryWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkWorkflow(data); err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string `yaml:"id"`
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["preflight"].Steps {
		if step.ID == "scope" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("preflight scope script missing")
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		t.Run(event+" with emergency provenance tag", func(t *testing.T) {
			dir := t.TempDir()
			git := `#!/usr/bin/env bash
case "$1" in
  ls-remote) printf '%s\trefs/heads/develop\n' 0123456789012345678901234567890123456789 ;;
  tag) printf 'v0.1.0\n' ;;
  for-each-ref) printf '<!-- detent-release-provenance:{"checks":[]} -->\n' ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o755); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(dir, "output")
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_EVENT_NAME="+event, "GITHUB_OUTPUT="+outputPath)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("preflight: %v: %s", err, output)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != "develop_sha=0123456789012345678901234567890123456789\nshould_run=true\n" {
				t.Fatalf("preflight output = %q, want pinned SHA and validation enabled", output)
			}
		})
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
		data, err := os.ReadFile(filepath.Join(workflows, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			On   map[string]yaml.Node `yaml:"on"`
			Jobs map[string]struct {
				Steps []struct {
					Uses string `yaml:"uses"`
					Run  string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"pull_request", "pull_request_target", "merge_group"} {
			if _, ok := workflow.On[event]; ok {
				if entry.Name() == "cla.yml" && event == "pull_request_target" && len(workflow.Jobs) == 1 {
					job, ok := workflow.Jobs["cla"]
					if ok && len(job.Steps) == 1 && job.Steps[0].Uses == "contributor-assistant/github-action@v2.6.1" && job.Steps[0].Run == "" {
						continue
					}
				}
				t.Errorf("INV-5 %s must not trigger on %s", entry.Name(), event)
			}
		}
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
		{"pull request trigger", "  workflow_dispatch:\n", "  pull_request:\n  workflow_dispatch:\n"},
		{"schedule dropped", "  schedule:\n    - cron: '17 * * * *'\n", ""},
		{"slow schedule", "cron: '17 * * * *'", "cron: '17 0 * * *'"},
		{"manual dispatch dropped", "  workflow_dispatch:\n", "  unused_event:\n"},
		{"missing invariant job", "  invariants:\n", "  renamed:\n"},
		{"filtered real job", "  lint:\n    needs: preflight\n    if: needs.preflight.outputs.should_run == 'true'", "  lint:\n    needs: preflight\n    if: false"},
		{"unbound development branch", "git ls-remote origin refs/heads/develop", "git ls-remote origin refs/heads/main"},
		{"malformed", "name: CI", "name: ["},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := strings.ReplaceAll(string(data), "\r\n", "\n")
			changed := strings.Replace(original, tt.old, tt.replacement, 1)
			if changed == original {
				t.Fatal("fixture replacement did not match")
			}
			if err := checkWorkflow([]byte(changed)); err == nil {
				t.Fatal("forbidden workflow passed")
			}
		})
	}
}
