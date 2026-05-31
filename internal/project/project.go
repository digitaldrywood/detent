package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/connector/factory"
	"github.com/digitaldrywood/symphony-go/internal/connector/memory"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/orchestrator"
	"github.com/digitaldrywood/symphony-go/internal/scheduler"
)

var (
	ErrAlreadyRunning      = errors.New("project already running")
	ErrMissingConnector    = errors.New("project connector is required")
	ErrMissingOrchestrator = errors.New("project orchestrator is required")
	ErrMissingProject      = errors.New("project is required")
	ErrMissingProjectID    = errors.New("project id is required")
	ErrNotRunning          = errors.New("project is not running")
	ErrProjectPaused       = errors.New("project is paused")
)

const (
	EventStarted EventKind = "project_started"
	EventStopped EventKind = "project_stopped"
)

type ProjectID string

type EventKind string

type Event struct {
	ProjectID ProjectID
	Kind      EventKind
	At        time.Time
	Error     string
}

type Config struct {
	Project  globalconfig.Project
	Workflow workflowconfig.Workflow
}

type ConnectorFactory func(workflowconfig.Config) (connector.Connector, error)

type OrchestratorFactory func(orchestrator.Config, orchestrator.Dependencies) (*orchestrator.Orchestrator, error)

type Dependencies struct {
	Connector           connector.Connector
	ConnectorFactory    ConnectorFactory
	OrchestratorFactory OrchestratorFactory
	Runner              orchestrator.Runner
	Scheduler           scheduler.Scheduler
	Events              *hub.Hub[Event]
	Logger              *slog.Logger
}

type Project struct {
	id           ProjectID
	cfg          globalconfig.Project
	workflow     workflowconfig.Workflow
	connector    connector.Connector
	orchestrator *orchestrator.Orchestrator
	scheduler    scheduler.Scheduler
	events       *hub.Hub[Event]
	logger       *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
}

func Load(cfg globalconfig.Project, deps Dependencies) (*Project, error) {
	workflow, err := workflowconfig.LoadWorkflow(cfg.Workflow)
	if err != nil {
		return nil, fmt.Errorf("load project workflow: %w", err)
	}

	return New(Config{Project: cfg, Workflow: workflow}, deps)
}

func New(cfg Config, deps Dependencies) (*Project, error) {
	id := normalizeProjectID(ProjectID(cfg.Project.ID))
	if id == "" {
		return nil, ErrMissingProjectID
	}

	workflow := normalizeWorkflow(cfg.Workflow)
	if err := workflow.Config.Validate(); err != nil {
		return nil, fmt.Errorf("validate project workflow: %w", err)
	}

	projectConnector, err := buildConnector(workflow.Config, deps)
	if err != nil {
		return nil, err
	}

	projectScheduler, err := buildScheduler(workflow.Config, deps)
	if err != nil {
		return nil, err
	}

	projectEvents := deps.Events
	if projectEvents == nil {
		projectEvents = hub.New[Event]()
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	orchestratorFactory := deps.OrchestratorFactory
	if orchestratorFactory == nil {
		orchestratorFactory = orchestrator.New
	}

	orch, err := orchestratorFactory(orchestrator.ConfigFromWorkflow(workflow.Config), orchestrator.Dependencies{
		Connector: projectConnector,
		Runner:    deps.Runner,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create project orchestrator: %w", err)
	}
	if orch == nil {
		return nil, ErrMissingOrchestrator
	}

	cfg.Project.ID = string(id)
	return &Project{
		id:           id,
		cfg:          cfg.Project,
		workflow:     workflow,
		connector:    projectConnector,
		orchestrator: orch,
		scheduler:    projectScheduler,
		events:       projectEvents,
		logger:       logger,
	}, nil
}

func (p *Project) ID() ProjectID {
	if p == nil {
		return ""
	}
	return p.id
}

func (p *Project) Config() globalconfig.Project {
	return p.cfg
}

func (p *Project) Workflow() workflowconfig.Workflow {
	return p.workflow
}

func (p *Project) Connector() connector.Connector {
	return p.connector
}

func (p *Project) Orchestrator() *orchestrator.Orchestrator {
	return p.orchestrator
}

func (p *Project) Scheduler() scheduler.Scheduler {
	return p.scheduler
}

func (p *Project) Events() *hub.Hub[Event] {
	return p.events
}

func (p *Project) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.done != nil
}

func (p *Project) Start(ctx context.Context) error {
	if p.cfg.Paused {
		return ErrProjectPaused
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.done != nil {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	p.cancel = cancel
	p.done = done
	p.mu.Unlock()

	p.publish(Event{
		ProjectID: p.id,
		Kind:      EventStarted,
		At:        time.Now(),
	})

	go p.run(runCtx, done)
	return nil
}

func (p *Project) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	if done == nil {
		p.mu.Unlock()
		return ErrNotRunning
	}
	cancel()
	p.mu.Unlock()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Project) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	done := p.done
	if done == nil {
		p.mu.Unlock()
		return ErrNotRunning
	}
	p.mu.Unlock()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Project) run(ctx context.Context, done chan<- error) {
	err := p.orchestrator.Run(ctx)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = nil
	}

	p.mu.Lock()
	if p.done == done {
		p.cancel = nil
		p.done = nil
	}
	p.mu.Unlock()

	p.publish(Event{
		ProjectID: p.id,
		Kind:      EventStopped,
		At:        time.Now(),
		Error:     errorString(err),
	})

	done <- err
	close(done)
}

func (p *Project) publish(event Event) {
	if err := p.events.Publish(event); err != nil {
		p.logger.Warn("publish project event failed",
			"project_id", event.ProjectID,
			"event", event.Kind,
			"error", err,
		)
	}
}

func buildConnector(cfg workflowconfig.Config, deps Dependencies) (connector.Connector, error) {
	if deps.Connector != nil {
		return deps.Connector, nil
	}

	connectorFactory := deps.ConnectorFactory
	if connectorFactory == nil {
		connectorFactory = defaultConnectorFactory
	}

	projectConnector, err := connectorFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("create project connector: %w", err)
	}
	if projectConnector == nil {
		return nil, ErrMissingConnector
	}

	return projectConnector, nil
}

func buildScheduler(cfg workflowconfig.Config, deps Dependencies) (scheduler.Scheduler, error) {
	if deps.Scheduler != nil {
		return deps.Scheduler, nil
	}

	projectScheduler, err := scheduler.NewFromConfig(scheduler.Config{
		Capacity:        cfg.Agent.MaxConcurrentAgents,
		CapacityByState: cfg.Agent.MaxConcurrentAgentsByState,
		CapacityPerHost: maxConcurrentAgentsPerHost(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("create project scheduler: %w", err)
	}

	return projectScheduler, nil
}

func defaultConnectorFactory(cfg workflowconfig.Config) (connector.Connector, error) {
	return factory.NewFromConfig(factory.Config{
		Kind:                    cfg.Tracker.Kind,
		Memory:                  memory.Config{Issues: cfg.Tracker.Issues},
		Endpoint:                cfg.Tracker.Endpoint,
		APIKey:                  cfg.Tracker.APIKey,
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		ProjectSlug:             cfg.Tracker.ProjectSlug,
	})
}

func normalizeWorkflow(workflow workflowconfig.Workflow) workflowconfig.Workflow {
	if !emptyWorkflowConfig(workflow.Config) {
		return workflow
	}

	workflow.Config = workflowconfig.Default()
	workflow.Config.Tracker.Kind = workflowconfig.TrackerMemory
	return workflow
}

func emptyWorkflowConfig(cfg workflowconfig.Config) bool {
	return cfg.Tracker.Kind == "" &&
		cfg.Polling.IntervalMS == 0 &&
		cfg.Agent.MaxConcurrentAgents == 0 &&
		cfg.Codex.Command == ""
}

func maxConcurrentAgentsPerHost(cfg workflowconfig.Config) int {
	if cfg.Worker.MaxConcurrentAgentsPerHost == nil {
		return 0
	}
	return *cfg.Worker.MaxConcurrentAgentsPerHost
}

func normalizeProjectID(id ProjectID) ProjectID {
	return ProjectID(strings.TrimSpace(string(id)))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
