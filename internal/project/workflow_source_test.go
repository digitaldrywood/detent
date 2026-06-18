package project

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
)

func TestLoadWorkflowUsesWorkingTreeWhenWorkflowRefUnset(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	writeWorkflowSourceFile(t, workflowPath, "working tree")

	workflow, err := LoadWorkflow(globalconfig.Project{Workflow: workflowPath})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}

	if got := strings.TrimSpace(workflow.Prompt); got != "working tree" {
		t.Fatalf("Prompt = %q, want working tree", got)
	}
}

func TestLoadWorkflowUsesConfiguredGitRef(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	writeWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "from ref")
	commitWorkflowSourceRepo(t, repo, "initial workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")
	writeWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "working tree")

	workflow, err := LoadWorkflow(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}

	if got := strings.TrimSpace(workflow.Prompt); got != "from ref" {
		t.Fatalf("Prompt = %q, want from ref", got)
	}
}

func TestLoadWorkflowUsesAbsolutePathUnderWorkdirWithConfiguredGitRef(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	workflowPath := filepath.Join(repo, "WORKFLOW.md")
	writeWorkflowSourceFile(t, workflowPath, "from ref")
	commitWorkflowSourceRepo(t, repo, "initial workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")
	writeWorkflowSourceFile(t, workflowPath, "working tree")

	workflow, err := LoadWorkflow(globalconfig.Project{
		Workflow:    workflowPath,
		WorkflowRef: "origin/main",
		Workdir:     repo,
	})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}

	if got := strings.TrimSpace(workflow.Prompt); got != "from ref" {
		t.Fatalf("Prompt = %q, want from ref", got)
	}
}

func TestLoadWorkflowRejectsRefPathOutsideWorkdir(t *testing.T) {
	t.Parallel()

	_, err := LoadWorkflow(globalconfig.Project{
		Workflow:    filepath.Join(t.TempDir(), "WORKFLOW.md"),
		WorkflowRef: "origin/main",
		Workdir:     t.TempDir(),
	})
	if !errors.Is(err, errUnsafeWorkflowPath) {
		t.Fatalf("LoadWorkflow() error = %v, want %v", err, errUnsafeWorkflowPath)
	}
}

func TestGitRefWorkflowWatcherReloadsWhenRefAdvances(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	writeWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "first")
	commitWorkflowSourceRepo(t, repo, "first workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")

	watcher, err := newGitRefWorkflowWatcher(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	}, 10*time.Millisecond, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	if err != nil {
		t.Fatalf("newGitRefWorkflowWatcher() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan configwatcher.Update, 1)

	lastRevision, lastErr := watcher.seed(ctx, updates)
	if lastErr != "" {
		t.Fatalf("seed() error = %s", lastErr)
	}
	if lastRevision == "" {
		t.Fatal("seed() revision = empty")
	}
	assertNoWorkflowSourceUpdate(t, updates)

	writeWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "second")
	commitWorkflowSourceRepo(t, repo, "second workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")

	lastRevision, lastErr = watcher.reload(ctx, updates, lastRevision, lastErr)
	if lastErr != "" {
		t.Fatalf("reload() error = %s", lastErr)
	}
	if lastRevision == "" {
		t.Fatal("reload() revision = empty")
	}
	update := readWorkflowSourceUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("workflow update error = %v", update.Err)
	}
	if got := strings.TrimSpace(update.Workflow.Prompt); got != "second" {
		t.Fatalf("Prompt = %q, want second", got)
	}
}

func assertNoWorkflowSourceUpdate(t *testing.T, updates <-chan configwatcher.Update) {
	t.Helper()

	select {
	case update := <-updates:
		t.Fatalf("unexpected workflow update: %#v", update)
	default:
	}
}

func readWorkflowSourceUpdate(t *testing.T, updates <-chan configwatcher.Update) configwatcher.Update {
	t.Helper()

	select {
	case update := <-updates:
		return update
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow update")
		return configwatcher.Update{}
	}
}

func initWorkflowSourceRepo(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	runWorkflowSourceCommand(t, "", "git", "init", root)
	runWorkflowSourceGit(t, root, "config", "user.email", "detent@example.com")
	runWorkflowSourceGit(t, root, "config", "user.name", "Detent Test")
	return root
}

func writeWorkflowSourceFile(t *testing.T, path string, prompt string) {
	t.Helper()

	content := "---\ntracker:\n  kind: memory\n---\n" + prompt + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func commitWorkflowSourceRepo(t *testing.T, repo string, message string) {
	t.Helper()

	runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md")
	runWorkflowSourceGit(t, repo, "commit", "-m", message)
}

func updateWorkflowSourceRef(t *testing.T, repo string, ref string, value string) {
	t.Helper()

	runWorkflowSourceGit(t, repo, "update-ref", "refs/remotes/"+ref, value)
}

func runWorkflowSourceGit(t *testing.T, repo string, args ...string) string {
	t.Helper()

	return runWorkflowSourceCommand(t, repo, "git", args...)
}

func runWorkflowSourceCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s error = %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}
