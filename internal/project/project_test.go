package project_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/orchestrator"
	"github.com/digitaldrywood/symphony-go/internal/project"
	"github.com/digitaldrywood/symphony-go/internal/scheduler"
)

func TestNewBuildsProjectLifecycleDependencies(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event]()
	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 3})
	created := make(chan orchestrator.Config, 1)

	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:       "symphony",
			Workflow: "workflow.md",
			Workdir:  "/workspace/symphony",
			Weight:   2,
			Priority: 10,
		},
		Workflow: workflowconfig.Workflow{
			Config: workflowConfigWithMemoryIssue("issue-1"),
			Prompt: "Run issue",
		},
	}, project.Dependencies{
		Scheduler: sched,
		Events:    events,
		Runner:    orchestrator.FakeRunner{},
		OrchestratorFactory: func(cfg orchestrator.Config, deps orchestrator.Dependencies) (*orchestrator.Orchestrator, error) {
			created <- cfg
			return orchestrator.New(cfg, deps)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if got.ID() != project.ProjectID("symphony") {
		t.Fatalf("ID() = %q, want symphony", got.ID())
	}
	if got.Config().Workdir != "/workspace/symphony" {
		t.Fatalf("Config().Workdir = %q, want /workspace/symphony", got.Config().Workdir)
	}
	if got.Connector().Name() != connector.BackendMemory.String() {
		t.Fatalf("Connector().Name() = %q, want memory", got.Connector().Name())
	}
	if got.Scheduler() != sched {
		t.Fatalf("Scheduler() = %p, want %p", got.Scheduler(), sched)
	}
	if got.Events() != events {
		t.Fatalf("Events() = %p, want %p", got.Events(), events)
	}
	if got.Orchestrator() == nil {
		t.Fatal("Orchestrator() = nil, want configured orchestrator")
	}

	select {
	case cfg := <-created:
		if cfg.MaxConcurrentAgents != 4 {
			t.Fatalf("orchestrator MaxConcurrentAgents = %d, want 4", cfg.MaxConcurrentAgents)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for orchestrator factory")
	}
}

func TestProjectStartStopPublishesLifecycleEvents(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(2))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "symphony",
			Weight: 1,
		},
		Workflow: workflowconfig.Workflow{
			Config: workflowConfigWithMemoryIssue("issue-1"),
		},
	}, project.Dependencies{
		Events: events,
		Runner: blockingRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := got.Start(context.Background()); !errors.Is(err, project.ErrAlreadyRunning) {
		t.Fatalf("Start() second error = %v, want %v", err, project.ErrAlreadyRunning)
	}

	started := receiveEvent(t, sub.C())
	if started.ProjectID != got.ID() || started.Kind != project.EventStarted {
		t.Fatalf("started event = %#v, want project started", started)
	}

	if err := got.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	stopped := receiveEvent(t, sub.C())
	if stopped.ProjectID != got.ID() || stopped.Kind != project.EventStopped {
		t.Fatalf("stopped event = %#v, want project stopped", stopped)
	}
	if err := got.Stop(context.Background()); !errors.Is(err, project.ErrNotRunning) {
		t.Fatalf("Stop() second error = %v, want %v", err, project.ErrNotRunning)
	}
	if err := got.Start(context.Background()); !errors.Is(err, project.ErrProjectStopped) {
		t.Fatalf("Start() after Stop error = %v, want %v", err, project.ErrProjectStopped)
	}
}

func TestNewRejectsInvalidProjectConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  project.Config
		deps project.Dependencies
		want error
	}{
		{
			name: "missing id",
			cfg:  project.Config{},
			want: project.ErrMissingProjectID,
		},
		{
			name: "missing connector",
			cfg: project.Config{
				Project:  globalconfig.Project{ID: "memory"},
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			},
			deps: project.Dependencies{
				ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					return nil, nil
				},
			},
			want: project.ErrMissingConnector,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := project.New(tt.cfg, tt.deps)
			if got != nil {
				t.Fatalf("New() project = %#v, want nil", got)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("New() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestProjectWaitersReceiveRunError(t *testing.T) {
	t.Parallel()

	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "symphony",
			Weight: 1,
		},
		Workflow: workflowconfig.Workflow{
			Config: workflowConfigWithMemoryIssue("issue-1"),
		},
	}, project.Dependencies{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := got.Start(runCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- got.Wait(context.Background())
		}()
	}

	for range 2 {
		if err := <-errs; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait() error = %v, want %v", err, context.DeadlineExceeded)
		}
	}
}

func TestProjectStartRejectsPausedProject(t *testing.T) {
	t.Parallel()

	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "paused",
			Paused: true,
			Weight: 1,
		},
	}, project.Dependencies{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := got.Start(context.Background()); !errors.Is(err, project.ErrProjectPaused) {
		t.Fatalf("Start() error = %v, want %v", err, project.ErrProjectPaused)
	}
}

func TestProjectPauseUnpauseRestartsProject(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "symphony",
			Weight: 1,
		},
	}, project.Dependencies{
		Events: events,
		Runner: blockingRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if event := receiveEvent(t, sub.C()); event.Kind != project.EventStarted {
		t.Fatalf("first event = %#v, want started", event)
	}

	if err := got.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	stopped := receiveEvent(t, sub.C())
	paused := receiveEvent(t, sub.C())
	if stopped.Kind != project.EventStopped || paused.Kind != project.EventPaused {
		t.Fatalf("pause events = %#v %#v, want stopped then paused", stopped, paused)
	}
	if !got.Paused() {
		t.Fatal("Paused() = false, want true")
	}

	if err := got.Unpause(context.Background()); err != nil {
		t.Fatalf("Unpause() error = %v", err)
	}
	unpaused := receiveEvent(t, sub.C())
	restarted := receiveEvent(t, sub.C())
	if unpaused.Kind != project.EventStarted || restarted.Kind != project.EventUnpaused {
		t.Fatalf("unpause events = %#v %#v, want started then unpaused", unpaused, restarted)
	}
	if got.Paused() {
		t.Fatal("Paused() = true, want false")
	}
}

func TestProjectPauseDoesNotMarkPausedWhenShutdownTimesOut(t *testing.T) {
	t.Parallel()

	blocker := newPauseBlockingConnector()
	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "symphony",
			Weight: 1,
		},
	}, project.Dependencies{
		Connector: blocker,
		Runner:    blockingRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	blocker.waitEntered(t)

	pauseCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := got.Pause(pauseCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pause() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got.Paused() {
		t.Fatal("Paused() = true after failed Pause, want false")
	}

	blocker.release()
	if err := got.Wait(context.Background()); err != nil && !errors.Is(err, project.ErrNotRunning) {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

func TestProjectUnpauseKeepsProjectPausedWhenRestartFails(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	calls := 0
	got, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:     "symphony",
			Weight: 1,
		},
	}, project.Dependencies{
		Events: events,
		Runner: blockingRunner{},
		OrchestratorFactory: func(cfg orchestrator.Config, deps orchestrator.Dependencies) (*orchestrator.Orchestrator, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("recreate failed")
			}
			return orchestrator.New(cfg, deps)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveEvent(t, sub.C())
	if err := got.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	receiveEvent(t, sub.C())
	receiveEvent(t, sub.C())

	if err := got.Unpause(context.Background()); err == nil || err.Error() != "create project orchestrator: recreate failed" {
		t.Fatalf("Unpause() error = %v, want recreate failure", err)
	}
	if !got.Paused() {
		t.Fatal("Paused() = false after failed Unpause, want true")
	}
}

func workflowConfigWithMemoryIssue(id string) workflowconfig.Config {
	cfg := workflowConfig("memory")
	cfg.Agent.MaxConcurrentAgents = 4
	cfg.Polling.IntervalMS = 10
	cfg.Tracker.Issues = []connector.Issue{{
		ID:         id,
		Identifier: id,
		State:      "Done",
	}}
	return cfg
}

func workflowConfig(kind string) workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = kind
	return cfg
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

type pauseBlockingConnector struct {
	entered  chan struct{}
	releasec chan struct{}
	once     sync.Once
}

func newPauseBlockingConnector() *pauseBlockingConnector {
	return &pauseBlockingConnector{
		entered:  make(chan struct{}),
		releasec: make(chan struct{}),
	}
}

func (c *pauseBlockingConnector) Name() string {
	return "pause-blocking"
}

func (c *pauseBlockingConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	c.once.Do(func() {
		close(c.entered)
	})
	<-c.releasec
	return nil, ctx.Err()
}

func (c *pauseBlockingConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *pauseBlockingConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *pauseBlockingConnector) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *pauseBlockingConnector) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *pauseBlockingConnector) waitEntered(t *testing.T) {
	t.Helper()

	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for connector fetch")
	}
}

func (c *pauseBlockingConnector) release() {
	close(c.releasec)
}

func receiveEvent(t *testing.T, ch <-chan project.Event) project.Event {
	t.Helper()

	select {
	case event := <-ch:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for project event")
	}

	return project.Event{}
}
