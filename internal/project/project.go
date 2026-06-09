package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
)

var (
	ErrAlreadyRunning      = errors.New("project already running")
	ErrMissingConnector    = errors.New("project connector is required")
	ErrMissingOrchestrator = errors.New("project orchestrator is required")
	ErrMissingProject      = errors.New("project is required")
	ErrMissingProjectID    = errors.New("project id is required")
	ErrNotRunning          = errors.New("project is not running")
	ErrProjectPaused       = errors.New("project is paused")
	ErrProjectStopped      = errors.New("project is stopped")
)

const (
	EventStarted  EventKind = "project_started"
	EventPaused   EventKind = "project_paused"
	EventStopped  EventKind = "project_stopped"
	EventUnpaused EventKind = "project_unpaused"
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

type WorkflowWatcher interface {
	Watch(context.Context) (<-chan configwatcher.Update, error)
}

type WorkflowWatcherFactory func(string) (WorkflowWatcher, error)

type startOptions struct {
	provision     bool
	publishEvents bool
}

type Dependencies struct {
	Connector              connector.Connector
	ConnectorFactory       ConnectorFactory
	OrchestratorFactory    OrchestratorFactory
	WorkflowWatcherFactory WorkflowWatcherFactory
	Runner                 orchestrator.Runner
	Scheduler              scheduler.Scheduler
	Events                 *hub.Hub[Event]
	Logger                 *slog.Logger
	GitHubToken            string
}

type Project struct {
	id               ProjectID
	cfg              globalconfig.Project
	workflow         workflowconfig.Workflow
	githubToken      string
	connector        connector.Connector
	connectorFactory ConnectorFactory
	orchestrator     *orchestrator.Orchestrator
	orchFactory      OrchestratorFactory
	orchConfig       orchestrator.Config
	orchDeps         orchestrator.Dependencies
	runner           orchestrator.Runner
	scheduler        scheduler.Scheduler
	schedulerFactory schedulerFactory
	events           *hub.Hub[Event]
	logger           *slog.Logger
	watcher          WorkflowWatcherFactory

	mu              sync.Mutex
	cancel          context.CancelFunc
	done            chan struct{}
	runErr          error
	started         bool
	lifecycleEvents bool
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
	workflow.Config = workflowConfigWithProjectIdentity(cfg.Project, workflow.Config)
	workflow.Config = workflowConfigWithGitHubToken(workflow.Config, deps.GitHubToken)
	if err := workflow.Config.Validate(); err != nil {
		return nil, fmt.Errorf("validate project workflow: %w", err)
	}

	connectorFactory := resolveConnectorFactory(deps)
	projectConnector, err := buildConnector(workflow.Config, connectorFactory)
	if err != nil {
		return nil, err
	}

	schedulerFactory := resolveSchedulerFactory(deps)
	projectScheduler, err := buildScheduler(workflow.Config, schedulerFactory)
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

	orchConfig := projectOrchestratorConfig(cfg.Project, workflow.Config)
	orchDeps := orchestrator.Dependencies{
		Connector: projectConnector,
		Runner:    deps.Runner,
		Logger:    logger,
	}
	orch, err := orchestratorFactory(orchConfig, orchDeps)
	if err != nil {
		return nil, fmt.Errorf("create project orchestrator: %w", err)
	}
	if orch == nil {
		return nil, ErrMissingOrchestrator
	}

	watcherFactory := deps.WorkflowWatcherFactory
	if watcherFactory == nil {
		watcherFactory = defaultWorkflowWatcherFactory
	}

	cfg.Project.ID = string(id)
	return &Project{
		id:               id,
		cfg:              cfg.Project,
		workflow:         workflow,
		githubToken:      strings.TrimSpace(deps.GitHubToken),
		connector:        projectConnector,
		connectorFactory: connectorFactory,
		orchestrator:     orch,
		orchFactory:      orchestratorFactory,
		orchConfig:       orchConfig,
		orchDeps:         orchDeps,
		runner:           deps.Runner,
		scheduler:        projectScheduler,
		schedulerFactory: schedulerFactory,
		events:           projectEvents,
		logger:           logger,
		watcher:          watcherFactory,
	}, nil
}

func (p *Project) ID() ProjectID {
	if p == nil {
		return ""
	}
	return p.id
}

func (p *Project) Config() globalconfig.Project {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cfg
}

func (p *Project) Workflow() workflowconfig.Workflow {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.workflow
}

func (p *Project) Connector() connector.Connector {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.connector
}

func (p *Project) Orchestrator() *orchestrator.Orchestrator {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.orchestrator
}

func (p *Project) Scheduler() scheduler.Scheduler {
	p.mu.Lock()
	defer p.mu.Unlock()

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

func (p *Project) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cfg.Paused
}

func (p *Project) Start(ctx context.Context) error {
	return p.start(ctx, startOptions{provision: true, publishEvents: true})
}

func (p *Project) start(ctx context.Context, opts startOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.cfg.Paused {
		p.mu.Unlock()
		return ErrProjectPaused
	}
	if p.done != nil {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}
	if p.started {
		p.mu.Unlock()
		return ErrProjectStopped
	}
	p.mu.Unlock()

	if opts.provision {
		if err := p.provision(ctx); err != nil {
			return err
		}
	}

	p.mu.Lock()
	if p.cfg.Paused {
		p.mu.Unlock()
		return ErrProjectPaused
	}
	if p.done != nil {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}
	if p.started {
		p.mu.Unlock()
		return ErrProjectStopped
	}
	if p.orchestrator == nil {
		orch, err := p.orchFactory(p.orchConfig, p.orchDeps)
		if err != nil {
			p.mu.Unlock()
			return fmt.Errorf("create project orchestrator: %w", err)
		}
		if orch == nil {
			p.mu.Unlock()
			return ErrMissingOrchestrator
		}
		p.orchestrator = orch
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	orch := p.orchestrator
	p.cancel = cancel
	p.done = done
	p.runErr = nil
	p.started = true
	p.lifecycleEvents = opts.publishEvents
	p.mu.Unlock()

	if opts.publishEvents {
		p.publishStarted()
	}

	go p.run(runCtx, done, orch)
	return nil
}

func (p *Project) Pause(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.cfg.Paused {
		p.mu.Unlock()
		return nil
	}
	cancel := p.cancel
	done := p.done
	wasRunning := done != nil
	if !wasRunning {
		p.cfg.Paused = true
	}
	p.mu.Unlock()

	if wasRunning {
		cancel()
		if err := p.waitDone(ctx, done); err != nil {
			return err
		}
	}

	p.mu.Lock()
	if wasRunning && p.done == nil {
		p.cfg.Paused = true
		p.started = false
		p.orchestrator = nil
	}
	p.mu.Unlock()

	p.publish(Event{
		ProjectID: p.id,
		Kind:      EventPaused,
		At:        time.Now(),
	})
	return nil
}

func (p *Project) Unpause(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if !p.cfg.Paused {
		p.mu.Unlock()
		return nil
	}
	p.cfg.Paused = false
	running := p.done != nil
	p.mu.Unlock()

	if !running {
		if err := p.Start(ctx); err != nil {
			p.mu.Lock()
			if p.done == nil {
				p.cfg.Paused = true
			}
			p.mu.Unlock()
			return err
		}
	}
	p.publish(Event{
		ProjectID: p.id,
		Kind:      EventUnpaused,
		At:        time.Now(),
	})
	return nil
}

func (p *Project) provision(ctx context.Context) error {
	p.mu.Lock()
	autoProvision := p.workflow.Config.Tracker.AutoProvision
	projectConnector := p.connector
	p.mu.Unlock()

	if !autoProvision {
		return nil
	}
	provisioner, ok := projectConnector.(connector.Provisioner)
	if !ok {
		return nil
	}
	if err := provisioner.Provision(ctx); err != nil {
		return fmt.Errorf("provision project connector: %w", err)
	}
	return nil
}

func (p *Project) Stop(ctx context.Context) error {
	return p.stop(ctx, true)
}

func (p *Project) stop(ctx context.Context, publishEvents bool) error {
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
	p.lifecycleEvents = publishEvents
	cancel()
	p.mu.Unlock()

	return p.waitDone(ctx, done)
}

func (p *Project) restart(ctx context.Context, opts startOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.cfg.Paused {
		p.mu.Unlock()
		return ErrProjectPaused
	}
	if p.done != nil {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}
	p.started = false
	p.orchestrator = nil
	p.mu.Unlock()

	return p.start(ctx, opts)
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

	return p.waitDone(ctx, done)
}

func (p *Project) waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.runErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Project) run(ctx context.Context, done chan struct{}, orch *orchestrator.Orchestrator) {
	watcherCtx, stopWatcher := context.WithCancel(ctx)
	watcherDone := p.startWorkflowWatcher(watcherCtx)

	err := orch.Run(ctx)
	stopWatcher()
	if watcherDone != nil {
		<-watcherDone
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = nil
	}

	p.mu.Lock()
	publishEvents := p.lifecycleEvents
	if p.done == done {
		p.cancel = nil
		p.done = nil
		p.runErr = err
	}
	p.mu.Unlock()

	if publishEvents {
		p.publish(Event{
			ProjectID: p.id,
			Kind:      EventStopped,
			At:        time.Now(),
			Error:     errorString(err),
		})
	}

	close(done)
}

func (p *Project) startWorkflowWatcher(ctx context.Context) <-chan struct{} {
	path := strings.TrimSpace(p.cfg.Workflow)
	if path == "" {
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		watcher, err := p.watcher(path)
		if err != nil {
			p.logger.Warn("create workflow watcher failed", "project_id", p.id, "path", path, "error", err)
			return
		}

		updates, err := watcher.Watch(ctx)
		if err != nil {
			p.logger.Warn("watch workflow failed", "project_id", p.id, "path", path, "error", err)
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				p.handleWorkflowUpdate(ctx, update)
			}
		}
	}()
	return done
}

func (p *Project) handleWorkflowUpdate(ctx context.Context, update configwatcher.Update) {
	if update.Err != nil {
		p.logger.Warn("workflow reload failed",
			"project_id", p.id,
			"path", update.Path,
			"error", update.Err,
		)
		return
	}

	p.mu.Lock()
	projectConfig := p.cfg
	githubToken := p.githubToken
	connectorFactory := p.connectorFactory
	schedulerFactory := p.schedulerFactory
	runner := p.runner
	projectOrchestrator := p.orchestrator
	p.mu.Unlock()
	if projectOrchestrator == nil {
		return
	}

	workflow := normalizeWorkflow(update.Workflow)
	workflow.Config = workflowConfigWithProjectIdentity(projectConfig, workflow.Config)
	workflow.Config = workflowConfigWithGitHubToken(workflow.Config, githubToken)
	if err := workflow.Config.Validate(); err != nil {
		p.logger.Warn("workflow reload validation failed",
			"project_id", p.id,
			"path", update.Path,
			"error", err,
		)
		return
	}

	projectConnector, err := buildConnector(workflow.Config, connectorFactory)
	if err != nil {
		p.logger.Warn("workflow reload connector failed",
			"project_id", p.id,
			"path", update.Path,
			"error", err,
		)
		return
	}

	projectScheduler, err := buildScheduler(workflow.Config, schedulerFactory)
	if err != nil {
		p.logger.Warn("workflow reload scheduler failed",
			"project_id", p.id,
			"path", update.Path,
			"error", err,
		)
		return
	}

	if updater, ok := runner.(workflowUpdater); ok {
		updater.UpdateWorkflow(workflow)
	}

	runtimeConfig := projectOrchestratorConfig(projectConfig, workflow.Config)
	if err := projectOrchestrator.UpdateRuntime(ctx, orchestrator.RuntimeUpdate{
		Config:    runtimeConfig,
		Connector: projectConnector,
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		p.logger.Warn("apply workflow reload failed",
			"project_id", p.id,
			"path", update.Path,
			"error", err,
		)
		return
	}

	p.mu.Lock()
	p.workflow = workflow
	p.connector = projectConnector
	p.scheduler = projectScheduler
	p.orchConfig = runtimeConfig
	p.orchDeps.Connector = projectConnector
	p.mu.Unlock()

	p.logger.Info("workflow reloaded", "project_id", p.id, "path", update.Path)
}

func projectOrchestratorConfig(project globalconfig.Project, workflow workflowconfig.Config) orchestrator.Config {
	workflow = workflowConfigWithProjectIdentity(project, workflow)
	cfg := orchestrator.ConfigFromWorkflow(workflow)
	cfg.Authorization = combineAuthorizationSelectors(cfg.Authorization, project.Authorization)
	return cfg
}

func workflowConfigWithProjectIdentity(
	project globalconfig.Project,
	workflow workflowconfig.Config,
) workflowconfig.Config {
	if !project.Identity.Configured() {
		return workflow
	}
	identity := project.Identity
	identity.Normalize()
	workflow.Identity = identity
	return workflow
}

func workflowConfigWithGitHubToken(workflow workflowconfig.Config, token string) workflowconfig.Config {
	token = strings.TrimSpace(token)
	if token != "" && workflow.Tracker.Kind == workflowconfig.TrackerGitHub {
		workflow.Tracker.APIKey = token
	}
	return workflow
}

func combineAuthorizationSelectors(selectors ...selector.Selector) selector.Selector {
	configured := make([]selector.Selector, 0, len(selectors))
	for _, candidate := range selectors {
		if candidate.Configured() {
			configured = append(configured, candidate)
		}
	}

	switch len(configured) {
	case 0:
		return selector.Selector{}
	case 1:
		return configured[0]
	default:
		return selector.Selector{And: configured}
	}
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

func (p *Project) publishStarted() {
	p.mu.Lock()
	p.lifecycleEvents = true
	id := p.id
	p.mu.Unlock()

	p.publish(Event{
		ProjectID: id,
		Kind:      EventStarted,
		At:        time.Now(),
	})
}

func (p *Project) publishStopped(err error) {
	p.mu.Lock()
	p.lifecycleEvents = true
	id := p.id
	p.mu.Unlock()

	p.publish(Event{
		ProjectID: id,
		Kind:      EventStopped,
		At:        time.Now(),
		Error:     errorString(err),
	})
}

type workflowUpdater interface {
	UpdateWorkflow(workflowconfig.Workflow)
}

type schedulerFactory func(workflowconfig.Config) (scheduler.Scheduler, error)

func resolveConnectorFactory(deps Dependencies) ConnectorFactory {
	if deps.Connector != nil {
		return func(workflowconfig.Config) (connector.Connector, error) {
			return deps.Connector, nil
		}
	}
	if deps.ConnectorFactory != nil {
		return deps.ConnectorFactory
	}
	return defaultConnectorFactory
}

func buildConnector(cfg workflowconfig.Config, connectorFactory ConnectorFactory) (connector.Connector, error) {
	projectConnector, err := connectorFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("create project connector: %w", err)
	}
	if projectConnector == nil {
		return nil, ErrMissingConnector
	}

	return projectConnector, nil
}

func resolveSchedulerFactory(deps Dependencies) schedulerFactory {
	if deps.Scheduler != nil {
		return func(workflowconfig.Config) (scheduler.Scheduler, error) {
			return deps.Scheduler, nil
		}
	}
	return defaultSchedulerFactory
}

func buildScheduler(cfg workflowconfig.Config, schedulerFactory schedulerFactory) (scheduler.Scheduler, error) {
	projectScheduler, err := schedulerFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("create project scheduler: %w", err)
	}
	if projectScheduler == nil {
		return nil, fmt.Errorf("create project scheduler: %w", scheduler.ErrUnsupportedBackend)
	}
	return projectScheduler, nil
}

func defaultSchedulerFactory(cfg workflowconfig.Config) (scheduler.Scheduler, error) {
	projectScheduler, err := scheduler.NewFromConfig(scheduler.Config{
		Capacity:        cfg.Agent.MaxConcurrentAgents,
		CapacityByState: cfg.Agent.MaxConcurrentAgentsByState,
		CapacityPerHost: maxConcurrentAgentsPerHost(cfg),
	})
	if err != nil {
		return nil, err
	}

	return projectScheduler, nil
}

func defaultConnectorFactory(cfg workflowconfig.Config) (connector.Connector, error) {
	return factory.NewFromConfig(factory.Config{
		Kind:                    cfg.Tracker.Kind,
		Memory:                  memory.Config{Issues: cfg.Tracker.Issues},
		Endpoint:                cfg.Tracker.Endpoint,
		APIKey:                  cfg.Tracker.APIKey,
		HTTPMaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:   cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		ProjectSlug:             cfg.Tracker.ProjectSlug,
		ActiveStates:            cfg.Tracker.ActiveStates,
		ObservedStates:          cfg.Tracker.ObservedStates,
		TerminalStates:          cfg.Tracker.TerminalStates,
		StateMap:                trackerStateMap(cfg.Tracker.StateMap),
		PriorityMap:             trackerPriorityMap(cfg.Tracker.PriorityMap),
	})
}

func trackerStateMap(value workflowconfig.StringOrMap) map[string]string {
	if !value.IsMap {
		return nil
	}

	out := make(map[string]string, len(value.Map))
	for state, mapped := range value.Map {
		mappedState, ok := mapped.(string)
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		mappedState = strings.TrimSpace(mappedState)
		if state != "" && mappedState != "" {
			out[state] = mappedState
		}
	}
	return out
}

func trackerPriorityMap(value workflowconfig.StringOrMap) map[string]*int {
	if !value.IsMap {
		return nil
	}

	out := make(map[string]*int, len(value.Map))
	for name, rank := range value.Map {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch rank := rank.(type) {
		case nil:
			out[name] = nil
		case int:
			rankValue := rank
			out[name] = &rankValue
		}
	}
	return out
}

func defaultWorkflowWatcherFactory(path string) (WorkflowWatcher, error) {
	return configwatcher.New(path)
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
