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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var errProjectFactoryStub = errors.New("project factory stub")

func TestBuildRunnerReturnsRunner(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = t.TempDir()

	run, err := buildRunner(workflowconfig.Workflow{Config: cfg}, "alpha", "", nil, nil)
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

func TestBuildRunnerUsesTopLevelPricingPath(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = t.TempDir()
	cfg.Budget.PricingPath = filepath.Join(t.TempDir(), "missing-models.yaml")

	_, err := buildRunner(workflowconfig.Workflow{Config: cfg}, "alpha", "", nil, nil)
	if err == nil {
		t.Fatal("buildRunner() error = nil, want pricing load error")
	}
	if !strings.Contains(err.Error(), "load pricing") {
		t.Fatalf("buildRunner() error = %v, want load pricing error", err)
	}
}

func TestBuildCodexCommandUsesConfiguredShell(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Codex.Command = "codex app-server --experimental"
	cfg.Codex.Shell = "bash"

	cmd := buildCodexCommand(context.Background(), cfg)
	if got := strings.Join(cmd.Args, "\x00"); got != "bash\x00-c\x00codex app-server --experimental" {
		t.Fatalf("Args = %#v, want bash -c configured command", cmd.Args)
	}
}

func TestBuildWorkspaceBackendUsesProjectWorkdirAsSourceRoot(t *testing.T) {
	t.Parallel()

	source := initRunnerSourceRepo(t)
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Workspace.Root = filepath.Join(t.TempDir(), "workspaces")
	cfg.Workspace.SourceRoot = ""

	backend, err := buildWorkspaceBackend(cfg, source, nil)
	if err != nil {
		t.Fatalf("buildWorkspaceBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), workspace.Issue{Identifier: "DD-129"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if got := strings.TrimSpace(runRunnerGit(t, info.Path, "branch", "--show-current")); got != "detent/dd-129" {
		t.Fatalf("worktree branch = %q, want detent/dd-129", got)
	}
	if got := readRunnerFile(t, filepath.Join(info.Path, "README.md")); got != "source repo\n" {
		t.Fatalf("README.md = %q, want source repo", got)
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
	if !errors.Is(err, errProjectFactoryStub) {
		t.Fatalf("ProjectFactory() error = %v, want stub", err)
	}
	if captured.Runner == nil {
		t.Fatal("project dependencies Runner = nil, want non-nil injected runner")
	}
	if _, ok := captured.Runner.(*runnerpkg.Runner); !ok {
		t.Fatalf("injected Runner = %T, want *runner.Runner", captured.Runner)
	}
}

func TestProjectDependenciesUseRuntimeGitHubTokenSource(t *testing.T) {
	t.Parallel()

	var captured projectpkg.Dependencies
	token := "first-token"
	factory := withRunnerFactory(projectpkg.Dependencies{}, nil, func(d projectpkg.Dependencies) (*projectpkg.Project, error) {
		captured = d
		return nil, errProjectFactoryStub
	}, func() string {
		return token
	})

	workflowPath := writeWorkflowFile(t)
	_, err := factory(globalconfig.Project{
		ID:       "alpha",
		Workflow: workflowPath,
		Workdir:  filepath.Dir(workflowPath),
		Weight:   1,
	})
	if !errors.Is(err, errProjectFactoryStub) {
		t.Fatalf("ProjectFactory() error = %v, want %v", err, errProjectFactoryStub)
	}
	if captured.GitHubToken != "first-token" {
		t.Fatalf("GitHubToken = %q, want first-token", captured.GitHubToken)
	}

	token = "second-token"
	_, err = factory(globalconfig.Project{
		ID:       "bravo",
		Workflow: workflowPath,
		Workdir:  filepath.Dir(workflowPath),
		Weight:   1,
	})
	if !errors.Is(err, errProjectFactoryStub) {
		t.Fatalf("ProjectFactory() error = %v, want %v", err, errProjectFactoryStub)
	}
	if captured.GitHubToken != "second-token" {
		t.Fatalf("GitHubToken = %q, want second-token", captured.GitHubToken)
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
		publishSnapshots(ctx, registry, snapshotHub, nil, "http://localhost:4101", 5*time.Millisecond, func() time.Time { return now })
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
	if snapshot.Project.DisplayName != "alpha" {
		t.Fatalf("snapshot.Project.DisplayName = %q, want alpha", snapshot.Project.DisplayName)
	}
	if len(snapshot.Projects) != 1 {
		t.Fatalf("snapshot.Projects len = %d, want 1", len(snapshot.Projects))
	}
	if snapshot.Projects[0].Project.ID != "alpha" || snapshot.Projects[0].Project.DisplayName != "alpha" {
		t.Fatalf("snapshot.Projects[0].Project = %#v, want alpha metadata", snapshot.Projects[0].Project)
	}
	if snapshot.DashboardURL != "http://localhost:4101" {
		t.Fatalf("snapshot.DashboardURL = %q, want dashboard URL", snapshot.DashboardURL)
	}
	if snapshot.Refresh.NextRefreshAt == nil {
		t.Fatalf("snapshot.Refresh.NextRefreshAt = nil, want next refresh")
	}
}

func TestRepublishSnapshotsOnProjectEventsPublishesLatestSnapshot(t *testing.T) {
	t.Parallel()

	events := hub.New[projectpkg.Event]()
	snapshotHub := hub.New[telemetry.Snapshot](hub.WithBuffer(2))
	now := time.Date(2026, 6, 20, 15, 30, 0, 0, time.UTC)
	latest := telemetry.Snapshot{
		GeneratedAt: now,
		Counts:      telemetry.Counts{Running: 1},
	}
	if err := snapshotHub.Publish(latest); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := snapshotHub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer sub.Close()
	receiveSnapshot(t, sub.C())

	done := make(chan struct{})
	go func() {
		defer close(done)
		republishSnapshotsOnProjectEvents(ctx, events, snapshotHub, nil)
	}()

	if err := events.Publish(projectpkg.Event{
		ProjectID: "alpha",
		Kind:      projectpkg.EventWorkflowReloaded,
		At:        now,
	}); err != nil {
		t.Fatalf("events.Publish() error = %v", err)
	}

	republished := receiveSnapshot(t, sub.C())
	if !republished.GeneratedAt.Equal(now) || republished.Counts.Running != 1 {
		t.Fatalf("republished snapshot = %#v, want latest snapshot", republished)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for project event republisher to stop")
	}
}

func TestPublishSnapshotOncePreservesPipeline(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := now.Add(-7 * time.Minute)
	pipelineIssue := connector.Issue{
		ID:         "i-212",
		Identifier: "digitaldrywood/detent#212",
		Title:      "Add PR pipeline lanes",
		State:      "Human Review",
		UpdatedAt:  &updatedAt,
		PullRequest: &connector.PullRequest{
			Number:           218,
			URL:              "https://github.com/digitaldrywood/detent/pull/218",
			State:            "OPEN",
			CIStatus:         "pending",
			CodexReviewState: "P1",
		},
	}
	project := newRefreshProjectWithConnector(t, "alpha", memory.New(memory.Config{
		Issues: []connector.Issue{pipelineIssue},
	}))
	if err := project.Start(context.Background()); err != nil {
		t.Fatalf("Project.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := project.Stop(ctx); err != nil && !errors.Is(err, projectpkg.ErrNotRunning) {
			t.Fatalf("Project.Stop() error = %v", err)
		}
	})
	mustSetProject(t, registry, project)

	snapshotHub := hub.New[telemetry.Snapshot]()
	if err := publishSnapshotOnce(context.Background(), registry, snapshotHub, now, nil, nil, "http://localhost:4101"); err != nil {
		t.Fatalf("publishSnapshotOnce() error = %v", err)
	}

	snapshot, ok := snapshotHub.Latest()
	if !ok {
		t.Fatal("snapshotHub.Latest() ok = false, want published snapshot")
	}
	if len(snapshot.Pipeline) != 1 {
		t.Fatalf("Pipeline len = %d, want 1", len(snapshot.Pipeline))
	}
	got := snapshot.Pipeline[0]
	if got.ID != "i-212" || got.State != "Human Review" || got.Title != "Add PR pipeline lanes" {
		t.Fatalf("Pipeline[0] = %#v, want issue #212 in Human Review", got)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("Pipeline[0].UpdatedAt = %v, want %v", got.UpdatedAt, updatedAt)
	}
	if got.PullRequest == nil || got.PullRequest.Number != 218 || got.PullRequest.CIStatus != "pending" || got.PullRequest.CodexReviewState != "P1" {
		t.Fatalf("Pipeline[0].PullRequest = %#v, want PR #218 pending with P1 review", got.PullRequest)
	}
}

func TestMergeSnapshotMergesInstanceScope(t *testing.T) {
	t.Parallel()

	base := telemetry.Snapshot{
		Instance: telemetry.Instance{
			Name:               "release-captain",
			GitHubLogin:        "detent-bot",
			AuthorizationScope: "All issues",
		},
	}
	got := mergeSnapshot(telemetry.Snapshot{}, base)
	if got.Instance != base.Instance {
		t.Fatalf("first merge Instance = %#v, want %#v", got.Instance, base.Instance)
	}

	got = mergeSnapshot(got, telemetry.Snapshot{
		Instance: telemetry.Instance{
			Name:                    "release-captain",
			GitHubLogin:             "detent-bot",
			AuthorizationScope:      "labels include release",
			AuthorizationConfigured: true,
		},
	})
	want := telemetry.Instance{
		Name:                    "release-captain",
		GitHubLogin:             "detent-bot",
		AuthorizationScope:      "Multiple authorization scopes",
		AuthorizationConfigured: true,
	}
	if got.Instance != want {
		t.Fatalf("merged Instance = %#v, want %#v", got.Instance, want)
	}
}

func TestMergeSnapshotStampsProjectIDOnIssueRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC)
	stageAt := now.Add(-time.Minute)
	completedAt := now.Add(-30 * time.Second)
	got := mergeSnapshot(telemetry.Snapshot{}, telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
		BoardIssues: []telemetry.Issue{
			{ID: "board", Identifier: "digitaldrywood/detent#6"},
		},
		Pipeline: []telemetry.Issue{
			{ID: "pipeline", Identifier: "digitaldrywood/detent#1", StageUpdatedAt: &stageAt},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running", Identifier: "digitaldrywood/detent#2"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "queued", Identifier: "digitaldrywood/detent#3"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "blocked", Identifier: "digitaldrywood/detent#4"}},
		},
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "completed", Identifier: "digitaldrywood/detent#5"}, CompletedAt: completedAt},
		},
	})

	tests := []struct {
		name string
		got  string
	}{
		{name: "pipeline", got: got.Pipeline[0].ProjectID},
		{name: "board", got: got.BoardIssues[0].ProjectID},
		{name: "running", got: got.Running[0].ProjectID},
		{name: "queued", got: got.Queue[0].ProjectID},
		{name: "blocked", got: got.Blocked[0].ProjectID},
		{name: "completed", got: got.Completed[0].ProjectID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != "detent" {
				t.Fatalf("ProjectID = %q, want detent", tt.got)
			}
		})
	}
}

func TestMergeSnapshotMergesDrainingShutdown(t *testing.T) {
	t.Parallel()

	firstRequestedAt := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	secondRequestedAt := firstRequestedAt.Add(2 * time.Minute)
	got := mergeSnapshot(telemetry.Snapshot{}, telemetry.Snapshot{
		Shutdown: telemetry.Shutdown{
			Status:            "draining",
			Draining:          true,
			SessionsRemaining: 2,
			RequestedAt:       &secondRequestedAt,
		},
	})
	got = mergeSnapshot(got, telemetry.Snapshot{
		Shutdown: telemetry.Shutdown{
			Status:            "draining",
			Draining:          true,
			SessionsRemaining: 1,
			RequestedAt:       &firstRequestedAt,
		},
	})

	if got.Shutdown.Status != "draining" || !got.Shutdown.Draining {
		t.Fatalf("Shutdown = %#v, want draining", got.Shutdown)
	}
	if got.Shutdown.SessionsRemaining != 3 {
		t.Fatalf("Shutdown.SessionsRemaining = %d, want 3", got.Shutdown.SessionsRemaining)
	}
	if got.Shutdown.RequestedAt == nil || !got.Shutdown.RequestedAt.Equal(firstRequestedAt) {
		t.Fatalf("Shutdown.RequestedAt = %v, want %v", got.Shutdown.RequestedAt, firstRequestedAt)
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

func TestTokenTrendRecorderCalculatesRollingThroughput(t *testing.T) {
	t.Parallel()

	recorder := newTokenTrendRecorder(10)
	start := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	snapshots := []telemetry.Snapshot{
		{GeneratedAt: start, Tokens: telemetry.Tokens{Input: 90, Output: 10, Total: 100}},
		{GeneratedAt: start.Add(10 * time.Second), Tokens: telemetry.Tokens{Input: 225, Output: 25, Total: 250}},
		{GeneratedAt: start.Add(70 * time.Second), Tokens: telemetry.Tokens{Input: 279, Output: 31, Total: 310}},
	}

	var got telemetry.Snapshot
	for _, snapshot := range snapshots {
		got = recorder.apply(snapshot)
	}

	if got.Throughput.TokensPerSecond != 1 {
		t.Fatalf("Throughput.TokensPerSecond = %v, want 1", got.Throughput.TokensPerSecond)
	}
	if got.Throughput.WindowSeconds != 60 {
		t.Fatalf("Throughput.WindowSeconds = %d, want 60", got.Throughput.WindowSeconds)
	}
	if got.Throughput.Tokens != 60 {
		t.Fatalf("Throughput.Tokens = %d, want 60", got.Throughput.Tokens)
	}
}

func TestTokenTrendRecorderResetsThroughputWhenTotalsDecrease(t *testing.T) {
	t.Parallel()

	recorder := newTokenTrendRecorder(10)
	start := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	_ = recorder.apply(telemetry.Snapshot{
		GeneratedAt: start,
		Tokens:      telemetry.Tokens{Input: 90, Output: 10, Total: 100},
	})
	got := recorder.apply(telemetry.Snapshot{
		GeneratedAt: start.Add(10 * time.Second),
		Tokens:      telemetry.Tokens{Input: 40, Output: 10, Total: 50},
	})

	if len(got.TokenTrend) != 1 {
		t.Fatalf("TokenTrend len = %d, want 1 after reset", len(got.TokenTrend))
	}
	if got.Throughput.TokensPerSecond != 0 || got.Throughput.Tokens != 0 {
		t.Fatalf("Throughput = %#v, want reset zero throughput", got.Throughput)
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

func initRunnerSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runRunnerCommand(t, dir, "git", "init", "-b", "main")
	runRunnerGit(t, dir, "config", "core.autocrlf", "false")
	runRunnerGit(t, dir, "config", "user.name", "Test User")
	runRunnerGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runRunnerGit(t, dir, "add", "README.md")
	runRunnerGit(t, dir, "commit", "-m", "initial")

	return dir
}

func runRunnerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runRunnerCommand(t, dir, "git", args...)
}

func runRunnerCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func readRunnerFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func newRefreshProjectWithConnector(t *testing.T, id string, projectConnector connector.Connector) *projectpkg.Project {
	t.Helper()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	project, err := projectpkg.New(projectpkg.Config{
		Project: globalconfig.Project{
			ID:      id,
			Workdir: t.TempDir(),
			Weight:  1,
		},
		Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
	}, projectpkg.Dependencies{Connector: projectConnector})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}
	return project
}

func receiveSnapshot(t *testing.T, ch <-chan telemetry.Snapshot) telemetry.Snapshot {
	t.Helper()

	select {
	case snapshot := <-ch:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for snapshot")
	}

	return telemetry.Snapshot{}
}
