package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	"github.com/digitaldrywood/symphony/internal/hub"
	projectpkg "github.com/digitaldrywood/symphony/internal/project"
	runnerpkg "github.com/digitaldrywood/symphony/internal/runner"
	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/workspace"
)

var errProjectFactoryStub = errors.New("project factory stub")

func TestBuildRunnerReturnsRunner(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = t.TempDir()
	cfg.Workspace.SourceRoot = initRunnerSourceRepo(t)

	run, err := buildRunner(workflowconfig.Workflow{Config: cfg}, nil, nil)
	if err != nil {
		t.Fatalf("buildRunner() error = %v", err)
	}
	if run == nil {
		t.Fatal("buildRunner() = nil, want non-nil runner")
	}
	if _, ok := run.(*runnerpkg.Runner); !ok {
		t.Fatalf("buildRunner() = %T, want *runner.Runner", run)
	}
}

func TestBuildWorkspaceBackendUsesConfiguredSourceRoot(t *testing.T) {
	t.Parallel()

	sourceRoot := initRunnerSourceRepo(t)
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")

	cfg := workflowconfig.Default()
	cfg.Workspace.Root = workspaceRoot
	cfg.Workspace.SourceRoot = sourceRoot
	cfg.Workspace.AutoBranch = true

	backend, err := buildWorkspaceBackend(cfg, nil)
	if err != nil {
		t.Fatalf("buildWorkspaceBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), workspace.Issue{Identifier: "DD-110"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	canonicalWorkspaceRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if !strings.HasPrefix(info.Path, canonicalWorkspaceRoot+string(os.PathSeparator)) {
		t.Fatalf("workspace path = %q, want under %q", info.Path, canonicalWorkspaceRoot)
	}
	raw, err := os.ReadFile(filepath.Join(info.Path, "README.md"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(raw) != "source repo\n" {
		t.Fatalf("README.md = %q, want source repo", raw)
	}
}

func TestProjectDependenciesInjectsNonNilRunner(t *testing.T) {
	t.Parallel()

	var captured projectpkg.Dependencies
	base := projectpkg.Dependencies{Logger: nil}
	factory := withRunnerFactory(base, nil, func(d projectpkg.Dependencies) (*projectpkg.Project, error) {
		captured = d
		return nil, errProjectFactoryStub
	})

	workflowPath := writeWorkflowFile(t)
	_, err := factory(globalconfig.Project{
		ID:       "alpha",
		Workflow: workflowPath,
		Workdir:  filepath.Dir(workflowPath),
		Weight:   1,
	})
	if err != errProjectFactoryStub {
		t.Fatalf("ProjectFactory() error = %v, want stub", err)
	}
	if captured.Runner == nil {
		t.Fatal("project dependencies Runner = nil, want non-nil injected runner")
	}
	if _, ok := captured.Runner.(*runnerpkg.Runner); !ok {
		t.Fatalf("injected Runner = %T, want *runner.Runner", captured.Runner)
	}
}

func TestPublishSnapshotsPublishesToHub(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, startRefreshProject(t, "alpha"))

	snapshotHub := hub.New[telemetry.Snapshot]()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		publishSnapshots(ctx, registry, snapshotHub, 5*time.Millisecond, func() time.Time { return now })
	}()

	var (
		snapshot telemetry.Snapshot
		ok       bool
	)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if snapshot, ok = snapshotHub.Latest(); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if !ok {
		t.Fatal("publishSnapshots did not publish any snapshot")
	}
	if !snapshot.GeneratedAt.Equal(now) {
		t.Fatalf("snapshot.GeneratedAt = %v, want %v", snapshot.GeneratedAt, now)
	}
}

func writeWorkflowFile(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	content := "---\n" +
		"tracker:\n  kind: memory\n" +
		"codex:\n  command: codex app-server\n" +
		"workspace:\n  root: " + filepath.Join(dir, "workspaces") + "\n" +
		"  source_root: " + initRunnerSourceRepo(t) + "\n" +
		"---\n\nTest workflow prompt.\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow file: %v", err)
	}
	return path
}

func initRunnerSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runRunnerCommand(t, dir, "git", "init", "-b", "main")
	runRunnerCommand(t, dir, "git", "config", "user.name", "Test User")
	runRunnerCommand(t, dir, "git", "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runRunnerCommand(t, dir, "git", "add", "README.md")
	runRunnerCommand(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func runRunnerCommand(t *testing.T, dir string, name string, args ...string) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}
