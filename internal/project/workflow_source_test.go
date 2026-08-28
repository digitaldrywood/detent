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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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

func TestLoadWorkflowRejectsRelativeWorkflowWhenWorkflowRefUnset(t *testing.T) {
	root := t.TempDir()
	daemonDir := filepath.Join(root, "daemon")
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeWorkflowSourceFile(t, filepath.Join(daemonDir, "WORKFLOW.md"), "daemon")
	writeWorkflowSourceFile(t, filepath.Join(projectDir, "WORKFLOW.md"), "project")
	t.Chdir(daemonDir)

	_, err := LoadWorkflow(globalconfig.Project{
		Workflow: "WORKFLOW.md",
		Workdir:  projectDir,
	})
	if !errors.Is(err, errRelativeWorkflowPath) {
		t.Fatalf("LoadWorkflow() error = %v, want %v", err, errRelativeWorkflowPath)
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

func TestLoadWorkflowUsesSplitDefinitionFromConfiguredGitRef(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "WORKFLOW.md"), []byte("from split ref\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: memory\npolling:\n  interval_ms: 90000\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(detent.yaml) error = %v", err)
	}
	runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md", "detent.yaml")
	runWorkflowSourceGit(t, repo, "commit", "-m", "split project definition")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "WORKFLOW.md"), []byte("working tree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(working tree) error = %v", err)
	}

	workflow, err := LoadWorkflow(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if strings.TrimSpace(workflow.Prompt) != "from split ref" {
		t.Fatalf("Prompt = %q, want split ref", workflow.Prompt)
	}
	if workflow.Config.Polling.IntervalMS != 90000 {
		t.Fatalf("Polling.IntervalMS = %d, want 90000", workflow.Config.Polling.IntervalMS)
	}
	if workflow.Definition.Layout != workflowconfig.ProjectDefinitionSplit {
		t.Fatalf("Layout = %q, want split", workflow.Definition.Layout)
	}
	if workflow.Definition.Revision == "" || workflow.Definition.Revision == workflow.SourceHash {
		t.Fatalf("Revision = %q, want git commit revision", workflow.Definition.Revision)
	}
}

func TestLoadWorkflowUsesAdmissionEffortGuidanceFromConfiguredGitRef(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	workflowPrompt := "## Admission Criteria\n\n- **Alignment** — ready.\n"
	config := `schema: 1
tracker:
  kind: memory
backlog_admission:
  enabled: true
  sources:
    states: [Backlog]
  target_state: Todo
  criteria_section: Admission Criteria
  require_effort: true
  effort_file: AGENTS.md
  effort_section: Issue Effort Selection
`
	agents := "## Issue Effort Selection\n\n- `medium` — small.\n- `high` — standard.\n"
	for fileName, content := range map[string]string{
		"WORKFLOW.md": workflowPrompt,
		"detent.yaml": config,
		"AGENTS.md":   agents,
	} {
		if err := os.WriteFile(filepath.Join(repo, fileName), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", fileName, err)
		}
	}
	runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md", "detent.yaml", "AGENTS.md")
	runWorkflowSourceGit(t, repo, "commit", "-m", "configure admission effort source")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("working tree guidance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}

	workflow, err := LoadWorkflow(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	rubric, err := workflowconfig.ResolveWorkflowAdmissionEffortRubric(workflow)
	if err != nil {
		t.Fatalf("ResolveWorkflowAdmissionEffortRubric() error = %v", err)
	}
	if len(rubric.Efforts) != 2 || rubric.Efforts[0] != "medium" || strings.Contains(workflow.AgentsPrompt, "working tree guidance") {
		t.Fatalf("rubric = %#v agents prompt = %q, want configured-ref AGENTS.md", rubric, workflow.AgentsPrompt)
	}
}

func TestLoadWorkflowUsesLocalOverlayWithConfiguredGitRef(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	workflowPath := filepath.Join(repo, "WORKFLOW.md")
	writeWorkflowSourceFile(t, workflowPath, "from ref")
	commitWorkflowSourceRepo(t, repo, "initial workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "WORKFLOW.local.md"), []byte("---\npolling:\n  interval_ms: 90000\n---\nlocal direction\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	workflow, err := LoadWorkflow(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	})
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if workflow.Config.Polling.IntervalMS != 90000 {
		t.Fatalf("Polling.IntervalMS = %d, want 90000", workflow.Config.Polling.IntervalMS)
	}
	if got := workflow.Prompt; !strings.Contains(got, "from ref") || !strings.Contains(got, "local direction") {
		t.Fatalf("Prompt = %q, want shared and local direction", got)
	}
	if workflow.Overlay.Path != filepath.Join(repo, "WORKFLOW.local.md") {
		t.Fatalf("Overlay.Path = %q, want working-tree overlay", workflow.Overlay.Path)
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

func TestGitRefWorkflowWatcherReloadsLocalOverlayLifecycle(t *testing.T) {
	t.Parallel()

	repo := initWorkflowSourceRepo(t)
	writeWorkflowSourceFile(t, filepath.Join(repo, "WORKFLOW.md"), "shared")
	commitWorkflowSourceRepo(t, repo, "initial workflow")
	updateWorkflowSourceRef(t, repo, "origin/main", "HEAD")

	watcher, err := newGitRefWorkflowWatcher(globalconfig.Project{
		Workflow:    "WORKFLOW.md",
		WorkflowRef: "origin/main",
		Workdir:     repo,
	}, time.Hour, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	if err != nil {
		t.Fatalf("newGitRefWorkflowWatcher() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	updates, err := watcher.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	localPath := filepath.Join(repo, "WORKFLOW.local.md")
	writeWorkflowSourceFile(t, localPath, "local")
	created := readWorkflowSourceUpdate(t, updates)
	if created.Err != nil {
		t.Fatalf("create update error = %v", created.Err)
	}
	if !strings.Contains(created.Workflow.Prompt, "shared") || !strings.Contains(created.Workflow.Prompt, "local") {
		t.Fatalf("create Prompt = %q, want shared and local", created.Workflow.Prompt)
	}

	if err := os.Remove(localPath); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	deleted := readWorkflowSourceUpdate(t, updates)
	if deleted.Err != nil {
		t.Fatalf("delete update error = %v", deleted.Err)
	}
	if got := strings.TrimSpace(deleted.Workflow.Prompt); got != "shared" {
		t.Fatalf("delete Prompt = %q, want shared", got)
	}
	if deleted.Workflow.Overlay.Path != "" {
		t.Fatalf("delete Overlay = %#v, want inactive", deleted.Workflow.Overlay)
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
	case <-time.After(30 * time.Second):
		t.Fatal("deadlocked waiting for workflow update")
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

	cmd := exec.CommandContext(t.Context(), name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s error = %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}
