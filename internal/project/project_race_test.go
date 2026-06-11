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
