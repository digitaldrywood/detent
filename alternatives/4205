package invariants

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

var finalizerCommand = regexp.MustCompile(`\bbash scripts/release-finalize\.sh\b`)

var validationCommand = regexp.MustCompile(`\bgo test\b|\bmake (test|check|lint|vet|security|visual-e2e)\b|\bnpm (run )?test\b|\bplaywright test\b|\bgolangci-lint\b`)

type workflowFile struct {
	On   map[string]yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func checkWorkflow(name string, data []byte) error {
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	for event := range workflow.On {
		switch event {
		case "pull_request", "merge_group", "schedule":
			return fmt.Errorf("INV-5 %s must not trigger on %s", name, event)
		case "pull_request_target":
			if name != "cla.yml" || len(workflow.Jobs) != 1 {
				return fmt.Errorf("INV-5 %s must not trigger on %s", name, event)
			}
			for _, job := range workflow.Jobs {
				if len(job.Steps) != 1 || job.Steps[0].Uses != "contributor-assistant/github-action@v2.6.1" || job.Steps[0].Run != "" {
					return fmt.Errorf("INV-5 %s may only run the CLA assistant on %s", name, event)
				}
			}
		}
	}
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if validationCommand.MatchString(step.Run) {
				return fmt.Errorf("INV-5 %s job %s runs validation in GitHub Actions", name, jobName)
			}
		}
	}
	if name == "release-finalize.yml" {
		if _, ok := workflow.On["workflow_dispatch"]; !ok || len(workflow.On) != 1 {
			return fmt.Errorf("INV-5 %s must trigger only on workflow_dispatch", name)
		}
		for _, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if finalizerCommand.MatchString(step.Run) {
					return nil
				}
			}
		}
		return fmt.Errorf("INV-5 %s must run scripts/release-finalize.sh", name)
	}
	return nil
}

func TestRepositoryWorkflow(t *testing.T) {
	workflows := filepath.Join(repositoryRoot(t), ".github", "workflows")
	for _, removed := range []string{"ci.yml", "windows-tests.yml", "portability-stress.yml"} {
		if _, err := os.Stat(filepath.Join(workflows, removed)); err == nil {
			t.Errorf("INV-5 GitHub Actions validation workflow %s must not exist", removed)
		}
	}
	if _, err := os.Stat(filepath.Join(workflows, "release-finalize.yml")); err != nil {
		t.Errorf("INV-5 release-finalize.yml missing: %v", err)
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
		if err := checkWorkflow(entry.Name(), data); err != nil {
			t.Error(err)
		}
	}
}

func TestWorkflowViolations(t *testing.T) {
	const cla = "on:\n  pull_request_target:\njobs:\n  cla:\n    steps:\n      - uses: contributor-assistant/github-action@v2.6.1\n"
	const finalize = "on:\n  workflow_dispatch:\njobs:\n  finalize:\n    steps:\n      - run: bash scripts/release-finalize.sh\n"
	for _, tt := range []struct {
		name, file, workflow string
		wantErr              bool
	}{
		{name: "dispatch-only release flow", file: "release-cut.yml", workflow: "on:\n  workflow_dispatch:\njobs:\n  cut:\n    steps:\n      - run: bash scripts/release-cut.sh\n"},
		{name: "cla exception", file: "cla.yml", workflow: cla},
		{name: "finalizer", file: "release-finalize.yml", workflow: finalize},
		{name: "pull request validation", file: "ci.yml", workflow: "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - run: go build ./...\n", wantErr: true},
		{name: "scheduled suite", file: "nightly.yml", workflow: "on:\n  schedule:\njobs:\n  test:\n    steps:\n      - run: true\n", wantErr: true},
		{name: "merge queue", file: "queue.yml", workflow: "on:\n  merge_group:\njobs: {}\n", wantErr: true},
		{name: "go tests in actions", file: "release.yml", workflow: "on:\n  workflow_dispatch:\njobs:\n  test:\n    steps:\n      - run: go test ./...\n", wantErr: true},
		{name: "lint in actions", file: "release.yml", workflow: "on:\n  workflow_dispatch:\njobs:\n  lint:\n    steps:\n      - run: make lint\n", wantErr: true},
		{name: "pull request target outside cla", file: "other.yml", workflow: cla, wantErr: true},
		{name: "cla running code", file: "cla.yml", workflow: cla + "      - run: make build\n", wantErr: true},
		{name: "finalizer on push", file: "release-finalize.yml", workflow: "on:\n  workflow_dispatch:\n  push:\n" + finalize[len("on:\n  workflow_dispatch:\n"):], wantErr: true},
		{name: "finalizer skips script", file: "release-finalize.yml", workflow: "on:\n  workflow_dispatch:\njobs:\n  finalize:\n    steps:\n      - run: true\n", wantErr: true},
		{name: "malformed", file: "x.yml", workflow: "on: [", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkWorkflow(tt.file, []byte(tt.workflow)); (err != nil) != tt.wantErr {
				t.Fatalf("checkWorkflow() error = %v, want error %t", err, tt.wantErr)
			}
		})
	}
}
