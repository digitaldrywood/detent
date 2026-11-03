package project

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type runnerSetupTestStore map[string]string

func (s runnerSetupTestStore) ProjectRunnerSetupHash(_ context.Context, runner, project string) (string, error) {
	return s[runner+"/"+project], nil
}

func (s runnerSetupTestStore) SetProjectRunnerSetupHash(_ context.Context, runner, project, hash string) error {
	s[runner+"/"+project] = hash
	return nil
}

func TestRunnerSetupMissingHelperAtSelectedRevision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture runs POSIX setup hooks")
	}
	for _, layout := range []string{"stale checkout", "non-checkout workdir"} {
		t.Run(layout, func(t *testing.T) {
			root := initWorkflowSourceRepo(t)
			runWorkflowSourceGit(t, root, "commit", "--allow-empty", "-m", "base setup")
			base := strings.TrimSpace(runWorkflowSourceGit(t, root, "rev-parse", "HEAD"))
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("WORKFLOW.md", "---\ntracker:\n  kind: hub_native\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n")
			write("setup.sh", "set -eu\nsh helper.sh\nprintf '%s\\n' \"$PWD\" >> \"$DETENT_SETUP_TEST_OUTPUT\"\nprintf 'scratch' > setup-output\n")
			write("helper.sh", "set -eu\ntest \"$(cat package.json)\" = '{\"version\":\"selected\"}'\nprintf 'selected\\n' >> \"$DETENT_SETUP_TEST_OUTPUT\"\n")
			write("package.json", "{\"version\":\"selected\"}")
			runWorkflowSourceGit(t, root, "add", ".")
			runWorkflowSourceGit(t, root, "commit", "-m", "selected setup")
			revision := strings.TrimSpace(runWorkflowSourceGit(t, root, "rev-parse", "HEAD"))
			write("README.md", "Unrelated source change.\n")
			runWorkflowSourceGit(t, root, "add", "README.md")
			runWorkflowSourceGit(t, root, "commit", "-m", "unchanged setup")
			reuseRevision := strings.TrimSpace(runWorkflowSourceGit(t, root, "rev-parse", "HEAD"))
			runWorkflowSourceGit(t, root, "checkout", "--detach", base)
			write("package.json", "dirty manifest must survive")
			workdir := root
			if layout == "non-checkout workdir" {
				workdir = filepath.Join(root, "workspaces")
				if err := os.Mkdir(workdir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before := runWorkflowSourceGit(t, root, "status", "--porcelain")
			output := filepath.Join(t.TempDir(), "installed")
			t.Setenv("DETENT_SETUP_TEST_OUTPUT", output)
			cfg := globalconfig.Project{ID: "example-project", Workdir: workdir, Workflow: "WORKFLOW.md", WorkflowRef: revision}
			localStore := runnerSetupTestStore{}
			setup := NewRunnerSetup("example-runner", localStore, nil)
			if err := setup.Prepare(t.Context(), cfg); err != nil {
				t.Fatalf("selected hook could not load its helper and manifest: %v", err)
			}
			for range 2 {
				cfg.WorkflowRef = reuseRevision
				setup = NewRunnerSetup("example-runner", localStore, nil)
				if err := setup.Prepare(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 || lines[0] != "selected" {
				t.Fatalf("successful setup was not reused: %q", data)
			}
			if _, err := os.Stat(lines[1]); !os.IsNotExist(err) {
				t.Fatalf("setup checkout was not removed: %v", err)
			}
			if after := runWorkflowSourceGit(t, root, "status", "--porcelain"); before != after {
				t.Fatalf("setup mutated source files: before %q, after %q", before, after)
			}
			if head := strings.TrimSpace(runWorkflowSourceGit(t, root, "rev-parse", "HEAD")); head != base {
				t.Fatalf("setup moved source HEAD: %s", head)
			}
		})
	}
}

func TestRunnerSetupFailureDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture runs POSIX setup hooks")
	}
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			root := t.TempDir()
			cfg := globalconfig.Project{ID: "example-project", Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md")}
			if err := os.WriteFile(cfg.Workflow, []byte("---\ntracker:\n  kind: hub_native\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EXAMPLE_SETUP_TOKEN", "short-private-value")
			script := "printf 'MODULE_NOT_FOUND helper.sh\\n%s\\nTOKEN=hidden-value\\nAuthorization: Bearer hidden-header\\n' \"$EXAMPLE_SETUP_TOKEN\"\nprintf 'partial-sensitive '\nprintf '%020000d' 0\nexit 7\n"
			if stream == "stderr" {
				script = "exec 1>&2\n" + script
			}
			if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			localStore := runnerSetupTestStore{}
			setup := NewRunnerSetup("example-runner", localStore, slog.New(slog.NewJSONHandler(&logs, nil)))
			err := setup.Prepare(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), "MODULE_NOT_FOUND helper.sh") || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "truncated") || len(err.Error()) > 2300 {
				t.Fatalf("missing bounded failure evidence: %v", err)
			}
			for _, diagnostic := range []string{err.Error(), logs.String()} {
				for _, secret := range []string{"short-private-value", "hidden-value", "hidden-header", "partial-sensitive"} {
					if strings.Contains(diagnostic, secret) {
						t.Fatalf("setup diagnostic exposed a credential: %q", secret)
					}
				}
			}
			if hash, _ := localStore.ProjectRunnerSetupHash(t.Context(), "example-runner", cfg.ID); hash != "" {
				t.Fatal("failed setup was cached")
			}
		})
	}
}
