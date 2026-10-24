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

func checkWorkflow(data []byte) error {
	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If    string    `yaml:"if"`
			Needs yaml.Node `yaml:"needs"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	for event := range workflow.On {
		if event != "schedule" && event != "workflow_dispatch" && event != "pull_request" {
			return fmt.Errorf("INV-5 CI must not trigger on %s", event)
		}
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		if _, ok := workflow.On[event]; !ok {
			return fmt.Errorf("INV-5 %s trigger missing", event)
		}
	}
	if _, pr := workflow.On["pull_request"]; pr {
		if _, ok := workflow.Jobs["verify-fast"]; !ok {
			return errors.New("INV-5 PR validation must reuse verify-fast")
		}
		for name, job := range workflow.Jobs {
			if name == "verify-fast" {
				continue
			}
			const excludePR = "github.event_name != 'pull_request'"
			if job.If != excludePR && !strings.HasPrefix(job.If, excludePR+" && ") {
				return fmt.Errorf("INV-5 %s must exclude PR events", name)
			}
		}
	}
	finalize, ok := workflow.Jobs["finalize"]
	if !ok {
		return errors.New("INV-5 CI finalize job missing")
	}
	var needs []string
	if finalize.Needs.Kind == yaml.ScalarNode {
		needs = []string{finalize.Needs.Value}
	} else if err := finalize.Needs.Decode(&needs); err != nil {
		return err
	}
	for name := range workflow.Jobs {
		if name != "finalize" && !slices.Contains(needs, name) {
			return fmt.Errorf("INV-5 finalizer must need %s", name)
		}
	}
	for _, step := range finalize.Steps {
		if strings.Contains(step.Run, "scripts/scheduled-ci-finish.sh") {
			return nil
		}
	}
	return errors.New("INV-5 finalizer must run scripts/scheduled-ci-finish.sh")
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
				if entry.Name() == "ci.yml" && event == "pull_request" {
					if err := checkWorkflow(data); err != nil {
						t.Error(err)
					}
					continue
				}
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

func TestWorkflowViolations(t *testing.T) {
	const jobs = "jobs:\n  test:\n    runs-on: ubuntu-latest\n  finalize:\n    needs: [test]\n    steps:\n      - run: bash scripts/scheduled-ci-finish.sh\n"
	const prJobs = "jobs:\n  verify-fast:\n    runs-on: ubuntu-latest\n  finalize:\n    if: github.event_name != 'pull_request'\n    needs: [verify-fast]\n    steps:\n      - run: bash scripts/scheduled-ci-finish.sh\n"
	for _, tt := range []struct {
		name, workflow string
		wantErr        bool
	}{
		{name: "valid", workflow: "on:\n  schedule:\n  workflow_dispatch:\n" + jobs},
		{name: "push trigger", workflow: "on:\n  schedule:\n  workflow_dispatch:\n  push:\n" + jobs, wantErr: true},
		{name: "bounded pull request guard", workflow: "on:\n  schedule:\n  workflow_dispatch:\n  pull_request:\n" + prJobs},
		{name: "pull request leaks full suite", workflow: "on:\n  schedule:\n  workflow_dispatch:\n  pull_request:\n" + strings.Replace(prJobs, "    if: github.event_name != 'pull_request'\n", "", 1), wantErr: true},
		{name: "pull request trigger without guard", workflow: "on:\n  schedule:\n  workflow_dispatch:\n  pull_request:\n" + jobs, wantErr: true},
		{name: "schedule missing", workflow: "on:\n  workflow_dispatch:\n" + jobs, wantErr: true},
		{name: "dispatch missing", workflow: "on:\n  schedule:\n" + jobs, wantErr: true},
		{name: "finalizer missing", workflow: "on:\n  schedule:\n  workflow_dispatch:\njobs:\n  test:\n    runs-on: ubuntu-latest\n", wantErr: true},
		{name: "job outside finalizer", workflow: "on:\n  schedule:\n  workflow_dispatch:\n" + jobs + "  extra:\n    runs-on: ubuntu-latest\n", wantErr: true},
		{name: "finalizer skips script", workflow: "on:\n  schedule:\n  workflow_dispatch:\n" + strings.Replace(jobs, "bash scripts/scheduled-ci-finish.sh", "true", 1), wantErr: true},
		{name: "malformed", workflow: "on: [", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkWorkflow([]byte(tt.workflow)); (err != nil) != tt.wantErr {
				t.Fatalf("checkWorkflow() error = %v, want error %t", err, tt.wantErr)
			}
		})
	}
}
