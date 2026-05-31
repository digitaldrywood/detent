package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	"github.com/digitaldrywood/symphony/internal/hub"
	projectpkg "github.com/digitaldrywood/symphony/internal/project"
	runnerpkg "github.com/digitaldrywood/symphony/internal/runner"
	"github.com/digitaldrywood/symphony/internal/telemetry"
)

var errProjectFactoryStub = errors.New("project factory stub")

func TestBuildRunnerReturnsRunner(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = t.TempDir()

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

func TestTokenTrendRecorderAppliesRollingWindow(t *testing.T) {
	t.Parallel()

	recorder := newTokenTrendRecorder(2)
	start := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	snapshots := []telemetry.Snapshot{
		{GeneratedAt: start, Tokens: telemetry.Tokens{Input: 10, Output: 1, Total: 11}},
		{GeneratedAt: start.Add(time.Minute), Tokens: telemetry.Tokens{Input: 20, Output: 2, Total: 22}},
		{GeneratedAt: start.Add(2 * time.Minute), Tokens: telemetry.Tokens{Input: 30, Output: 3, Total: 33}},
	}

	var got telemetry.Snapshot
	for _, snapshot := range snapshots {
		got = recorder.apply(snapshot)
	}

	if len(got.TokenTrend) != 2 {
		t.Fatalf("TokenTrend len = %d, want 2", len(got.TokenTrend))
	}
	if !got.TokenTrend[0].At.Equal(start.Add(time.Minute)) {
		t.Fatalf("TokenTrend[0].At = %v, want second sample", got.TokenTrend[0].At)
	}
	if got.TokenTrend[1].Input != 30 || got.TokenTrend[1].Output != 3 || got.TokenTrend[1].Total != 33 {
		t.Fatalf("TokenTrend[1] = %#v, want latest totals", got.TokenTrend[1])
	}
}

func TestTokenTrendRecorderKeepsEmptyStateWithoutUsage(t *testing.T) {
	t.Parallel()

	recorder := newTokenTrendRecorder(2)
	got := recorder.apply(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC),
		Tokens:      telemetry.Tokens{},
	})

	if len(got.TokenTrend) != 0 {
		t.Fatalf("TokenTrend len = %d, want 0", len(got.TokenTrend))
	}
}

func TestTokenTrendRecorderClearsStaleUsage(t *testing.T) {
	t.Parallel()

	recorder := newTokenTrendRecorder(2)
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	_ = recorder.apply(telemetry.Snapshot{
		GeneratedAt: now,
		Tokens:      telemetry.Tokens{Input: 10, Output: 1, Total: 11},
	})
	got := recorder.apply(telemetry.Snapshot{
		GeneratedAt: now.Add(time.Minute),
		Tokens:      telemetry.Tokens{},
	})

	if len(got.TokenTrend) != 0 {
		t.Fatalf("TokenTrend len = %d, want 0", len(got.TokenTrend))
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
		"---\n\nTest workflow prompt.\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write workflow file: %v", err)
	}
	return path
}
