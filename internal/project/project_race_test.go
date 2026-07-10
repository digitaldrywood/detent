package project

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func TestHandleWorkflowUpdateDoesNotRaceWithPause(t *testing.T) {
	t.Parallel()

	reloadEntered := make(chan struct{})
	reloadRelease := make(chan struct{})
	var reloadEnteredOnce sync.Once
	var connectorFactoryCalls atomic.Int32

	got, err := New(Config{
		Project: globalconfig.Project{
			ID:     "detent",
			Weight: 1,
		},
		Workflow: workflowconfig.Workflow{
			Config: projectRaceWorkflowConfig(),
			Prompt: "initial",
		},
	}, Dependencies{
		ConnectorFactory: func(cfg workflowconfig.Config) (connector.Connector, error) {
			if connectorFactoryCalls.Add(1) > 1 {
				reloadEnteredOnce.Do(func() {
					close(reloadEntered)
				})
				<-reloadRelease
			}
			return defaultConnectorFactory(cfg)
		},
		Runner: projectRaceBlockingRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		reloaded := projectRaceWorkflowConfig()
		reloaded.Polling.IntervalMS = 60000
		got.handleWorkflowUpdate(context.Background(), configwatcher.Update{
			Workflow: workflowconfig.Workflow{
				Config: reloaded,
				Prompt: "reloaded",
			},
		})
	}()

	select {
	case <-reloadEntered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow reload")
	}

	if err := got.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}

	close(reloadRelease)
	select {
	case <-updateDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow reload to finish")
	}
}

func TestHandleWorkflowUpdateRejectsIntakeBeforeRuntimeMutation(t *testing.T) {
	t.Parallel()

	initial := projectRaceWorkflowConfig()
	projectConnector := memory.New(memory.Config{Stateful: true})
	got, err := New(Config{
		Project: globalconfig.Project{ID: "detent", Weight: 1},
		Workflow: workflowconfig.Workflow{
			Config: initial,
			Prompt: "initial",
		},
	}, Dependencies{
		Connector: projectConnector,
		Runner:    projectRaceBlockingRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := got.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	reloaded := projectRaceWorkflowConfig()
	reloaded.Tracker.Kind = workflowconfig.TrackerGitHub
	reloaded.Tracker.APIKey = "token"
	reloaded.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
	reloaded.Tracker.Repository = "example/repo"
	reloaded.Polling.IntervalMS = 60000
	reloaded.Intake.Sources = []intake.Source{{
		Name:   "alerts",
		Kind:   "pagerduty",
		Secret: "secret",
	}}
	got.handleWorkflowUpdate(context.Background(), configwatcher.Update{
		Workflow: workflowconfig.Workflow{
			Config: reloaded,
			Prompt: "reloaded",
		},
	})

	if got.Workflow().Prompt != "initial" {
		t.Fatalf("Workflow().Prompt = %q, want initial", got.Workflow().Prompt)
	}
	if got.Connector() != projectConnector {
		t.Fatal("Connector() changed after rejected intake reload")
	}
	state, err := got.orchestrator.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if state.PollInterval != time.Hour {
		t.Fatalf("State().PollInterval = %s, want %s", state.PollInterval, time.Hour)
	}
}

func TestBuildRetroIssueStoresRoutesProductRepository(t *testing.T) {
	t.Parallel()

	cfg := projectRaceWorkflowConfig()
	cfg.Tracker.Repository = "example/project"
	cfg.Retro.Enabled = true
	cfg.Retro.ProductRepository = "digitaldrywood/detent"
	cfg.Retro.Normalize()
	projectConnector := memory.New(memory.Config{Stateful: true})
	productConnector := memory.New(memory.Config{Stateful: true})
	var repositories []string

	projectIssues, productIssues, created, err := buildRetroIssueStores(cfg, projectConnector, func(candidate workflowconfig.Config) (connector.Connector, error) {
		repositories = append(repositories, candidate.Tracker.Repository)
		return productConnector, nil
	})
	if err != nil {
		t.Fatalf("buildRetroIssueStores() error = %v", err)
	}
	if projectIssues != projectConnector {
		t.Fatalf("project issue store = %T, want project connector", projectIssues)
	}
	if productIssues != productConnector || created != productConnector {
		t.Fatalf("product issue store/connector = %T/%T, want product connector", productIssues, created)
	}
	if len(repositories) != 1 || repositories[0] != "digitaldrywood/detent" {
		t.Fatalf("product repositories = %v, want digitaldrywood/detent", repositories)
	}
}

func projectRaceWorkflowConfig() workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = connector.BackendMemory.String()
	cfg.Polling.IntervalMS = int(time.Hour / time.Millisecond)
	return cfg
}

type projectRaceBlockingRunner struct{}

func (projectRaceBlockingRunner) Run(ctx context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}
