//go:build !windows

package project

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestRunnerSetupLifecycle(t *testing.T) {

	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, ref := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout", true: "workflow ref"}[ref], func(t *testing.T) {
			root := t.TempDir()
			if ref {
				root = initWorkflowSourceRepo(t)
			}
			cfg := globalconfig.Project{ID: "project-one", Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md")}
			if err := os.WriteFile(cfg.Workflow, []byte("---\ntracker:\n  kind: hub_native\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if ref {
				cfg.WorkflowRef = "HEAD"
			}
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if ref {
					runWorkflowSourceGit(t, root, "add", "WORKFLOW.md", "setup.sh")
					runWorkflowSourceGit(t, root, "commit", "-m", "setup fixture")
				}
			}
			script := "printf 'run\\n' >> trace\nmkdir -p tools\nprintf '#!/bin/sh\\nprintf cli-ready' > tools/setup-cli\nchmod +x tools/setup-cli\n"
			write(script)
			if ref {
				if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte("exit 9\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			first := NewRunnerSetup("runner-one", nil, logger)
			second := NewRunnerSetup("runner-two", nil, logger)
			assertRuns := func(want int) {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(root, "trace"))
				if err != nil || strings.Count(string(content), "\n") != want {
					t.Fatalf("setup runs = %q, error = %v; want %d", content, err, want)
				}
			}
			var callers sync.WaitGroup
			for range 8 {
				callers.Go(func() {
					if err := first.Prepare(t.Context(), cfg); err != nil {
						t.Error(err)
					}
				})
			}
			callers.Wait()
			assertRuns(8)
			for _, runner := range []*RunnerSetup{first, second} {
				if err := runner.Prepare(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				output, err := exec.CommandContext(t.Context(), filepath.Join(root, "tools", "setup-cli")).Output()
				if err != nil || string(output) != "cli-ready" {
					t.Fatalf("next turn CLI = %q, error = %v", output, err)
				}
			}
			assertRuns(10)
			script += "printf 'changed' > version\n"
			write(script)
			for _, runner := range []*RunnerSetup{first, second} {
				for range 2 {
					if err := runner.Prepare(t.Context(), cfg); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertRuns(14)
			first = NewRunnerSetup("runner-one", nil, logger)
			if err := first.Prepare(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			assertRuns(15)
			write("printf 'failure\\n' >> trace\nexit 7\n")
			for range 2 {
				if err := first.Prepare(t.Context(), cfg); err == nil {
					t.Fatal("failing setup succeeded")
				}
			}
			assertRuns(17)
			if !strings.Contains(logs.String(), `"attribution":"instance"`) || !strings.Contains(logs.String(), `"project_id":"project-one"`) {
				t.Fatalf("missing instance setup report: %s", &logs)
			}
			write(script)
			if err := first.Prepare(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			assertRuns(18)
			if ref {
				if _, err := os.Stat(filepath.Join(root, ".git", "detent-runner-setup")); !os.IsNotExist(err) {
					t.Fatalf("stateless setup created a reusable local receipt: %v", err)
				}
				for range 2 {
					if err := first.Prepare(t.Context(), cfg); err != nil {
						t.Fatal(err)
					}
				}
				assertRuns(20)
				return
			}
			moved := t.TempDir()
			for _, name := range []string{"WORKFLOW.md", "setup.sh"} {
				content, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(moved, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			relocated := cfg
			relocated.Workdir, relocated.Workflow = moved, filepath.Join(moved, "WORKFLOW.md")
			for range 2 {
				if err := first.Prepare(t.Context(), relocated); err != nil {
					t.Fatal(err)
				}
			}
			if content, err := os.ReadFile(filepath.Join(moved, "trace")); err != nil || string(content) != "run\nrun\n" {
				t.Fatalf("relocated checkout setup runs = %q, error = %v; want one per preparation", content, err)
			}
		})
	}
}

func TestRunnerSetupWithoutLocalStoreRunsEachPreparation(t *testing.T) {

	root := t.TempDir()
	cfg := globalconfig.Project{ID: "example", Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md")}
	if err := os.WriteFile(cfg.Workflow, []byte("---\ntracker:\n  kind: hub_native\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte("printf 'run\\n' >> trace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setup := NewRunnerSetup("runner-example", nil, nil)
	for range 2 {
		if err := setup.Prepare(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	trace, err := os.ReadFile(filepath.Join(root, "trace"))
	if err != nil || string(trace) != "run\nrun\n" {
		t.Fatalf("setup runs = %q, error = %v", trace, err)
	}
}
