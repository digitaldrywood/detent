package invariants

import (
	"encoding/json"
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
		On          map[string]yaml.Node `yaml:"on"`
		Permissions map[string]string    `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			Permissions map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	if len(workflow.On) != 2 {
		return errors.New("INV-5 CI must trigger only on a schedule and manual dispatch")
	}
	if _, ok := workflow.Permissions["actions"]; ok {
		return errors.New("INV-5 Actions access must be scoped to preflight")
	}
	if permissions := workflow.Jobs["preflight"].Permissions; len(permissions) != 2 || permissions["actions"] != "read" || permissions["contents"] != "read" {
		return errors.New("INV-5 preflight must have read-only Actions and contents access")
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
	var dispatch struct {
		Inputs map[string]struct {
			Type    string `yaml:"type"`
			Default any    `yaml:"default"`
		} `yaml:"inputs"`
	}
	dispatchNode := workflow.On["workflow_dispatch"]
	if err := dispatchNode.Decode(&dispatch); err != nil {
		return err
	}
	force, ok := dispatch.Inputs["force"]
	if !ok || force.Type != "boolean" || force.Default != false {
		return errors.New("INV-5 workflow_dispatch must expose force defaulting to false")
	}
	if _, ok := dispatch.Inputs["fail_job"]; !ok {
		return errors.New("INV-5 workflow_dispatch must preserve the fail_job input")
	}
	for _, required := range []string{"preflight", "invariants", "lint", "verify-fast", "generated", "app", "verify-race", "test-cover", "security", "browser-visual-shard", "installer-smoke", "goreleaser-snapshot", "finalize"} {
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
	if !strings.Contains(string(data), "git ls-remote origin refs/heads/develop") || strings.Count(string(data), "ref: ${{ needs.preflight.outputs.develop_sha }}") < 11 {
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
				ID   string            `yaml:"id"`
				Name string            `yaml:"name"`
				If   string            `yaml:"if"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	var scopeEnv map[string]string
	for _, step := range workflow.Jobs["preflight"].Steps {
		if step.ID == "scope" {
			script = step.Run
			scopeEnv = step.Env
		}
	}
	if script == "" {
		t.Fatal("preflight scope script missing")
	}
	if scopeEnv["FORCE"] != "${{ inputs.force || false }}" || scopeEnv["FAIL_JOB"] != "${{ inputs.fail_job || 'none' }}" {
		t.Fatalf("preflight input wiring = %v", scopeEnv)
	}
	var failureProbeIf string
	for _, step := range workflow.Jobs["verify-fast"].Steps {
		if step.Name == "Force acceptance probe failure" {
			failureProbeIf = step.If
		}
	}
	if failureProbeIf != "github.event_name == 'workflow_dispatch' && inputs.fail_job == 'verify-fast'" {
		t.Fatalf("fail_job probe condition = %q", failureProbeIf)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the preflight fixture")
	}
	type priorRun struct {
		ID         int    `json:"id"`
		Event      string `json:"event"`
		Conclusion string `json:"conclusion"`
	}
	for _, tt := range []struct {
		name, event, currentRunID string
		force                     bool
		failJob                   string
		priorRuns                 []priorRun
		wantRun                   bool
		wantGitHubQuery           bool
		wantPriorRunID            string
	}{
		{name: "dispatch on validated SHA skips", event: "workflow_dispatch", currentRunID: "900", priorRuns: []priorRun{{ID: 123, Event: "schedule", Conclusion: "success"}}, wantGitHubQuery: true, wantPriorRunID: "123"},
		{name: "dispatch with force runs", event: "workflow_dispatch", currentRunID: "900", force: true, priorRuns: []priorRun{{ID: 123, Event: "schedule", Conclusion: "success"}}, wantRun: true},
		{name: "cancelled prior run does not skip", event: "workflow_dispatch", currentRunID: "900", priorRuns: []priorRun{{ID: 123, Event: "schedule", Conclusion: "cancelled"}}, wantRun: true, wantGitHubQuery: true},
		{name: "new SHA runs", event: "schedule", currentRunID: "900", wantRun: true, wantGitHubQuery: true},
		{name: "fail_job implies force", event: "workflow_dispatch", currentRunID: "900", failJob: "verify-fast", priorRuns: []priorRun{{ID: 123, Event: "schedule", Conclusion: "success"}}, wantRun: true},
		{name: "current run is excluded", event: "workflow_dispatch", currentRunID: "900", priorRuns: []priorRun{{ID: 900, Event: "workflow_dispatch", Conclusion: "success"}}, wantRun: true, wantGitHubQuery: true},
		{name: "schedule deduplicates against dispatch run", event: "schedule", currentRunID: "900", priorRuns: []priorRun{{ID: 123, Event: "workflow_dispatch", Conclusion: "failure"}}, wantGitHubQuery: true, wantPriorRunID: "123"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			git := `#!/usr/bin/env bash
case "$1" in
  ls-remote) printf '%s\trefs/heads/develop\n' 0123456789012345678901234567890123456789 ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o755); err != nil {
				t.Fatal(err)
			}
			gh := `#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *'actions/workflows/ci.yml/runs?'*) cat "$FIXTURE_RUNS_FILE" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			priorRuns := tt.priorRuns
			if priorRuns == nil {
				priorRuns = []priorRun{}
			}
			fixture, err := json.Marshal([]struct {
				WorkflowRuns []priorRun `json:"workflow_runs"`
			}{{WorkflowRuns: priorRuns}})
			if err != nil {
				t.Fatal(err)
			}
			fixturePath := filepath.Join(dir, "runs.json")
			if err := os.WriteFile(fixturePath, fixture, 0o600); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(dir, "output")
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			logPath := filepath.Join(dir, "calls")
			force := map[bool]string{true: "true", false: "false"}[tt.force]
			failJob := tt.failJob
			if failJob == "" {
				failJob = "none"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_EVENT_NAME="+tt.event, "GITHUB_OUTPUT="+outputPath, "GITHUB_REPOSITORY=digitaldrywood/detent", "GITHUB_RUN_ID="+tt.currentRunID, "FORCE="+force, "FAIL_JOB="+failJob, "FIXTURE_RUNS_FILE="+fixturePath, "FIXTURE_LOG="+logPath)
			preflightLog, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("preflight: %v: %s", err, preflightLog)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			wantShouldRun := map[bool]string{true: "true", false: "false"}[tt.wantRun]
			if string(output) != "develop_sha=0123456789012345678901234567890123456789\nshould_run="+wantShouldRun+"\n" {
				t.Fatalf("preflight output = %q, want pinned SHA and should_run=%s; log: %s", output, wantShouldRun, preflightLog)
			}
			logs, err := os.ReadFile(logPath)
			if err != nil && tt.wantGitHubQuery {
				t.Fatal(err)
			}
			if tt.wantGitHubQuery != (err == nil) {
				t.Fatalf("GitHub prior-run query presence = %t, want %t", err == nil, tt.wantGitHubQuery)
			}
			if tt.wantGitHubQuery {
				query := string(logs)
				for _, required := range []string{"status=completed", "head_sha=0123456789012345678901234567890123456789"} {
					if !strings.Contains(query, required) {
						t.Fatalf("prior-run query omitted %q: %s", required, query)
					}
				}
				if strings.Contains(query, "event=") {
					t.Fatalf("prior-run query filtered on event: %s", query)
				}
			}
			if !strings.Contains(string(preflightLog), "Develop SHA: 0123456789012345678901234567890123456789") || !strings.Contains(string(preflightLog), "current_run_id="+tt.currentRunID) {
				t.Fatalf("preflight log omitted compared SHA or decision: %s", preflightLog)
			}
			if tt.wantPriorRunID != "" && !strings.Contains(string(preflightLog), "prior_run_id="+tt.wantPriorRunID) {
				t.Fatalf("preflight log omitted qualifying prior run id %s: %s", tt.wantPriorRunID, preflightLog)
			}
			wantDecision := map[bool]string{true: "run", false: "skip"}[tt.wantRun]
			if !strings.Contains(string(preflightLog), "Preflight decision: "+wantDecision) {
				t.Fatalf("preflight log omitted decision %s: %s", wantDecision, preflightLog)
			}
			t.Logf("preflight log:\n%s", preflightLog)
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
		{"actions read permission dropped", "  actions: read\n", ""},
		{"finalizer treats a preflight skip as a run", "if: always() && (needs.preflight.outputs.should_run == 'true' || needs.preflight.result == 'failure')", "if: always()"},
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
