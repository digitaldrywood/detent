package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	"github.com/digitaldrywood/detent/internal/activity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

var (
	ErrAlreadyRunning      = errors.New("project already running")
	ErrConnectorCreation   = errors.New("project connector creation failed")
	ErrProjectDefinition   = errors.New("project definition failed")
	ErrMissingConnector    = errors.New("project connector is required")
	ErrMissingOrchestrator = errors.New("project orchestrator is required")
	ErrMissingProject      = errors.New("project is required")
	ErrMissingProjectID    = errors.New("project id is required")
	ErrNotRunning          = errors.New("project is not running")
	ErrProjectPaused       = errors.New("project is paused")
	ErrProjectStopped      = errors.New("project is stopped")
)

const (
	EventStarted          EventKind = "project_started"
	EventPaused           EventKind = "project_paused"
	EventStopped          EventKind = "project_stopped"
	EventUnpaused         EventKind = "project_unpaused"
	EventWorkflowReloaded EventKind = "project_workflow_reloaded"

	workflowWatcherInitialBackoff = 100 * time.Millisecond
	workflowWatcherMaxBackoff     = 5 * time.Second
)

type ID string

type EventKind string

type Event struct {
	ProjectID ID
	Kind      EventKind
	At        time.Time
	Error     string
}

type RuntimeError struct {
	Message     string
	At          time.Time
	NextRetryAt time.Time
	Terminal    bool
	Transient   bool
}

type WorkflowSourceStatus struct {
	Path             string
	Hash             string
	Revision         string
	Layout           workflowconfig.ProjectDefinitionLayout
	ModifiedAt       time.Time
	LoadedAt         time.Time
	LastWatchEventAt time.Time
	LastReconcileAt  time.Time
	WatcherArmed     bool
	LastReloadError  string
	ReloadFailedAt   time.Time
}

type Config struct {
	Project  globalconfig.Project
	Workflow workflowconfig.Workflow
}

type projectDefinitionError struct {
	err error
}

func (e projectDefinitionError) Error() string {
	return e.err.Error()
}

func (e projectDefinitionError) Unwrap() error {
	return e.err
}

func (e projectDefinitionError) Is(target error) bool {
	return target == ErrProjectDefinition
}

type ConnectorFactory func(workflowconfig.Config) (connector.Connector, error)

type OrchestratorFactory func(orchestrator.Config, orchestrator.Dependencies) (*orchestrator.Orchestrator, error)

type WorkflowWatcher interface {
	Watch(context.Context) (<-chan configwatcher.Update, error)
}

type WorkflowWatcherFactory func(string) (WorkflowWatcher, error)

type startOptions struct {
	draining      bool
	provision     bool
	publishEvents bool
}

type Dependencies struct {
	HostBuildCache            *toolcache.Managed
	HostCacheReport           func() *toolcache.Report
	TrimHostCache             func(context.Context, toolcache.Policy, time.Time) error
	Connector                 connector.Connector
	Scheduling                orchestrator.SchedulingSource
	ConnectorFactory          ConnectorFactory
	OrchestratorFactory       OrchestratorFactory
	WorkflowWatcherFactory    WorkflowWatcherFactory
	WorkflowReconcileInterval time.Duration
	Runner                    orchestrator.Runner
	Scheduler                 scheduler.Scheduler
	GlobalDispatchGate        scheduler.ProjectDispatchGate
	DispatchPacer             runpkg.DispatchPacer
	WorkflowMetrics           orchestrator.WorkflowMetricsRecorder
	Efficiency                efficiency.Recorder
	LifecycleExporter         efficiency.LifecycleExporter
	WorkAttempts              store.WorkAttemptStore
	AgentResume               store.AgentResumeStore
	ValidatorMemo             store.ValidatorMemoStore
	StalenessWarnings         store.StalenessWarningStore
	Activity                  *activity.Broker
	Events                    *hub.Hub[Event]
	Logger                    *slog.Logger
	GitHubToken               string
	RefreshGitHubToken        func(context.Context) (string, error)
	ScheduleStore             coordination.Store
	ScheduleOwner             string
}

type Project struct {
	hostCacheReport           func() *toolcache.Report
	id                        ID
	cfg                       globalconfig.Project
	workflow                  workflowconfig.Workflow
	workflowActiveHours       activehours.Config
	githubToken               string
	connector                 connector.Connector
	connectorFactory          ConnectorFactory
	orchestrator              *orchestrator.Orchestrator
	orchFactory               OrchestratorFactory
	orchConfig                orchestrator.Config
	orchDeps                  orchestrator.Dependencies
	policyScheduling          orchestrator.SchedulingSource
	runner                    orchestrator.Runner
	scheduler                 scheduler.Scheduler
	schedulerFactory          schedulerFactory
	scheduleOwner             *scheduleowner.Manager
	issueCoordinator          *scheduleowner.IssueCoordinator
	scheduleConfig            scheduleowner.Config
	scheduleFault             *scheduleFaultState
	events                    *hub.Hub[Event]
	logger                    *slog.Logger
	watcher                   WorkflowWatcherFactory
	workflowReconcileInterval time.Duration

	mu              sync.Mutex
	configMu        sync.Mutex
	cancel          context.CancelFunc
	done            chan struct{}
	runErr          error
	runtimeErr      RuntimeError
	started         bool
	closed          bool
	lifecycleEvents bool
	workflowSource  WorkflowSourceStatus
}

type scheduleFaultState struct {
	mu         sync.Mutex
	runtimeErr RuntimeError
}

func (s *scheduleFaultState) record(err error, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.runtimeErr = RuntimeError{}
		return
	}
	s.runtimeErr = RuntimeError{Message: err.Error(), At: at.UTC()}
}

func (s *scheduleFaultState) current() RuntimeError {
	if s == nil {
		return RuntimeError{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimeErr
}

func Load(cfg globalconfig.Project, deps Dependencies) (*Project, error) {
	workflow, err := loadWorkflowForScheduling(context.Background(), cfg, deps.Scheduling)
	if err != nil {
		return nil, fmt.Errorf("load project workflow: %w", err)
	}

	return New(Config{Project: cfg, Workflow: workflow}, deps)
}

func New(cfg Config, deps Dependencies) (*Project, error) {
	return NewContext(context.Background(), cfg, deps)
}

func NewContext(ctx context.Context, cfg Config, deps Dependencies) (*Project, error) {
	id := normalizeProjectID(ID(cfg.Project.ID))
	if id == "" {
		return nil, ErrMissingProjectID
	}

	workflow := normalizeWorkflow(cfg.Workflow)
	workflow.Config = applyProjectAgentDefaults(workflow.Config, cfg.Project)
	workflow.Config = WithMappedNativeTracker(workflow.Config, deps.Scheduling, id)
	if err := configureProjectPolicy(ctx, cfg.Project, &workflow, deps.Scheduling); err != nil {
		return nil, projectDefinitionError{err: err}
	}
	workflowActiveHours := workflow.Config.ActiveHours.Normalize()
	workflow.Config = workflowConfigWithProjectIdentity(cfg.Project, workflow.Config)
	workflow.Config = workflowConfigWithGitHubToken(workflow.Config, deps.GitHubToken)
	if err := workflow.Config.Validate(); err != nil {
		return nil, projectDefinitionError{err: fmt.Errorf("validate project workflow: %w", err)}
	}
	if err := workflowconfig.ValidateWorkflowAdmission(workflow); err != nil {
		return nil, projectDefinitionError{err: fmt.Errorf("validate project workflow: %w", err)}
	}

	connectorFactory := resolveConnectorFactory(deps)
	if workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		source, ok := deps.Scheduling.(interface {
			ConnectorForProject(string) (connector.Connector, bool)
		})
		if !ok {
			return nil, errors.New("native Hub tracker requires configured Hub scheduling")
		}
		native, ok := source.ConnectorForProject(string(id))
		if !ok {
			return nil, errors.New("native Hub tracker requires a client.native_projects mapping")
		}
		connectorFactory = func(workflowconfig.Config) (connector.Connector, error) { return native, nil }
	}
	projectConnector, err := buildConnector(ctx, workflow.Config, connectorFactory, cfg.Project.Paused)
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
	for _, warning := range workflow.Config.ValidationWarnings() {
		if strings.HasPrefix(warning, "tracker.") {
			logger.Warn("workflow configuration warning", "project_id", id, "warning", warning)
		}
	}
	if reclaimed, resolvedRoot, err := toolcache.RemoveLegacyContext(ctx, workflow.Config.Workspace.Root); err != nil {
		logger.Warn("remove legacy worker caches", "workspace_root", resolvedRoot, "error", err)
	} else if reclaimed > 0 {
		logger.Info("removed legacy worker caches", "bytes_reclaimed", reclaimed, "workspace_root", resolvedRoot)
	}
	scheduleFault := &scheduleFaultState{}
	projectScheduleOwner, issueCoordinator, scheduleConfig, err := buildScheduleOwnership(workflow.Config, deps, logger, func(err error) {
		scheduleFault.record(err, time.Now().UTC())
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create project schedule ownership: %w", err), closeConnector(projectConnector))
	}
	retainScheduleOwner := false
	defer func() {
		if !retainScheduleOwner {
			if closeErr := closeScheduleOwner(projectScheduleOwner); closeErr != nil {
				logger.Warn("close unused schedule ownership failed", "project_id", id, "error", closeErr)
			}
		}
	}()
	var orch *orchestrator.Orchestrator
	var project *Project
	releaseBuild, err := buildReleaseCoordinator(workflow.Config, projectConnector)
	if err != nil {
		return nil, err
	}
	releaseCoordinator := releaseBuild.coordinator
	orchestratorFactory := deps.OrchestratorFactory
	if orchestratorFactory == nil {
		orchestratorFactory = orchestrator.New
	}
	lifecycleExporter := deps.LifecycleExporter
	if lifecycleExporter == nil {
		lifecycleExporter, err = efficiency.NewLifecycleExporter(efficiency.ExporterConfig{
			Endpoint:    workflow.Config.Observability.OTLP.Endpoint,
			Headers:     workflow.Config.Observability.OTLP.Headers,
			ServiceName: workflow.Config.Observability.OTLP.ServiceName,
			Timeout:     time.Duration(workflow.Config.Observability.OTLP.TimeoutMS) * time.Millisecond,
		})
		if err != nil {
			return nil, fmt.Errorf("create lifecycle exporter: %w", err)
		}
	}

	orchConfig := projectOrchestratorConfig(cfg.Project, workflow)
	orchDeps := orchestrator.Dependencies{
		TrimHostCache:      deps.TrimHostCache,
		LaneCoordination:   projectScheduleOwner.CoordinationStore(),
		Connector:          projectConnector,
		Scheduling:         projectSchedulingSource(deps.Scheduling, workflow.Config),
		Runner:             deps.Runner,
		GlobalDispatchGate: deps.GlobalDispatchGate,
		DispatchPacer:      deps.DispatchPacer,
		WorkflowMetrics:    deps.WorkflowMetrics,
		Efficiency:         deps.Efficiency,
		LifecycleExporter:  lifecycleExporter,
		WorkAttempts:       deps.WorkAttempts,
		AgentResume:        deps.AgentResume,
		ValidatorMemo:      deps.ValidatorMemo,
		StalenessWarnings:  deps.StalenessWarnings,
		Activity:           deps.Activity,
		Release:            releaseCoordinator,
		Logger:             logger,
	}
	orch, err = orchestratorFactory(orchConfig, orchDeps)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create project orchestrator: %w", err))
	}
	if orch == nil {
		return nil, errors.Join(ErrMissingOrchestrator)
	}
	if workflow.Config.Policy.ID != "" {
		if updater, ok := deps.Runner.(workflowUpdater); ok {
			updater.UpdateWorkflow(workflow)
		}
	}

	watcherProject := cfg.Project
	watcherProject.ID = string(id)
	workflowPath := workflowSourceDisplayPath(cfg.Project)
	workflowModifiedAt := workflowFileModifiedAt(cfg.Project)

	cfg.Project.ID = string(id)
	project = &Project{
		hostCacheReport:           deps.HostCacheReport,
		id:                        id,
		cfg:                       cfg.Project,
		workflow:                  workflow,
		workflowActiveHours:       workflowActiveHours,
		githubToken:               strings.TrimSpace(deps.GitHubToken),
		connector:                 projectConnector,
		connectorFactory:          connectorFactory,
		orchestrator:              orch,
		orchFactory:               orchestratorFactory,
		orchConfig:                orchConfig,
		orchDeps:                  orchDeps,
		policyScheduling:          deps.Scheduling,
		runner:                    deps.Runner,
		scheduler:                 projectScheduler,
		schedulerFactory:          schedulerFactory,
		scheduleOwner:             projectScheduleOwner,
		issueCoordinator:          issueCoordinator,
		scheduleConfig:            scheduleConfig,
		scheduleFault:             scheduleFault,
		events:                    projectEvents,
		logger:                    logger,
		workflowReconcileInterval: deps.WorkflowReconcileInterval,
		workflowSource: WorkflowSourceStatus{
			Path:       workflowPath,
			Hash:       workflow.SourceHash,
			Revision:   workflow.Definition.Revision,
			Layout:     workflow.Definition.Layout,
			ModifiedAt: workflowModifiedAt,
			LoadedAt:   time.Now().UTC(),
		},
	}
	project.watcher = resolveWorkflowWatcherFactory(ctx, deps, watcherProject, deps.GitHubToken, logger, project.Config)
	retainScheduleOwner = true
	return project, nil
}

func (p *Project) ID() ID {
	if p == nil {
		return ""
	}
	return p.id
}

func (p *Project) RunnerProblems() []runnerauth.Problem {
	p.mu.Lock()
	value := p.runner
	p.mu.Unlock()
	if reporter, ok := value.(interface{ Problems() []runnerauth.Problem }); ok {
		return reporter.Problems()
	}
	return nil
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

func (p *Project) ActiveHoursStatus(now time.Time) (activehours.Status, error) {
	if p == nil {
		return activehours.Status{}, nil
	}
	p.mu.Lock()
	candidate := p.orchConfig.Project
	p.mu.Unlock()
	return candidate.ActiveHoursStatus(now)
}

func (p *Project) DispatchCandidate() scheduler.ProjectCandidate {
	if p == nil {
		return scheduler.ProjectCandidate{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.orchConfig.Project
}

func (p *Project) updateLiveConfig(ctx context.Context, cfg globalconfig.Project) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.configMu.Lock()
	defer p.configMu.Unlock()

	p.mu.Lock()
	workflow := p.workflow
	workflow.Config = applyProjectAgentDefaults(workflow.Config, cfg)
	if workflow.Config.Policy.Configuration != nil {
		resolved, err := workflowconfig.ApplyNativePolicy(workflow, workflow.Config.Policy)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		workflow = resolved
	}
	if err := workflow.Config.Validate(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("validate inherited agent configuration: %w", err)
	}
	workflow.Config.ActiveHours = EffectiveActiveHours(cfg, p.workflowActiveHours)
	workflow.Config.Agent.RateWindowPacing = effectiveRateWindowPacing(cfg, workflow.Config)
	if workflow.Config.Policy.ID != "" && workflow.Config.Policy.Configuration == nil && (!reflect.DeepEqual(workflow.Config.ActiveHours, p.workflow.Config.ActiveHours) || !reflect.DeepEqual(workflow.Config.Agents, p.workflow.Config.Agents) || workflow.Config.Budget.PricingPath != p.workflow.Config.Budget.PricingPath) {
		p.mu.Unlock()
		return errors.New("policy_mismatch: host execution overrides changed; approve the effective descriptor and restart Detent before applying them")
	}
	runtimeConfig := projectOrchestratorConfig(cfg, workflow)
	projectOrchestrator := p.orchestrator
	running := p.done != nil
	projectRunner := p.runner
	p.mu.Unlock()

	applyRunner, err := prepareRunnerWorkflow(projectRunner, workflow)
	if err != nil {
		return fmt.Errorf("prepare inherited agent configuration: %w", err)
	}
	if projectOrchestrator != nil && running {
		if err := projectOrchestrator.UpdateConfig(ctx, runtimeConfig); err != nil {
			return fmt.Errorf("update project live config: %w", err)
		}
	}
	applyRunner()

	p.mu.Lock()
	p.cfg = cfg
	p.workflow = workflow
	p.orchConfig = runtimeConfig
	p.mu.Unlock()
	return nil
}

func (p *Project) WorkflowSourceStatus() WorkflowSourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.workflowSource
}

func (p *Project) EnforcedBudget() (workflowconfig.Budget, bool) {
	p.mu.Lock()
	runner := p.runner
	p.mu.Unlock()

	reporter, ok := runner.(interface {
		EnforcedBudget() (workflowconfig.Budget, bool)
	})
	if !ok {
		return workflowconfig.Budget{}, false
	}
	return reporter.EnforcedBudget()
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

func (p *Project) RuntimeError() RuntimeError {
	if p == nil {
		return RuntimeError{}
	}

	p.mu.Lock()
	runtimeErr := p.runtimeErr
	scheduleFault := p.scheduleFault
	p.mu.Unlock()
	if runtimeErr.Message != "" {
		return runtimeErr
	}
	return scheduleFault.current()
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
	if p.started || p.closed {
		p.mu.Unlock()
		return ErrProjectStopped
	}
	p.mu.Unlock()

	if opts.provision {
		if err := p.provision(ctx); err != nil {
			p.recordRuntimeError(err)
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
	if p.started || p.closed {
		p.mu.Unlock()
		return ErrProjectStopped
	}
	if p.orchestrator == nil {
		orch, err := p.orchFactory(p.orchConfig, p.orchDeps)
		if err != nil {
			p.mu.Unlock()
			err := fmt.Errorf("create project orchestrator: %w", err)
			p.recordRuntimeError(err)
			return err
		}
		if orch == nil {
			p.mu.Unlock()
			p.recordRuntimeError(ErrMissingOrchestrator)
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
	p.runtimeErr = RuntimeError{}
	p.started = true
	p.lifecycleEvents = opts.publishEvents
	p.mu.Unlock()

	if opts.publishEvents {
		p.publishStarted()
	}

	if opts.draining {
		orch.BeginDrain()
	}
	go p.run(runCtx, done, orch)
	if opts.draining {
		return orch.Drain(ctx)
	}
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
	return p.unpause(ctx, false)
}

func (p *Project) unpause(ctx context.Context, draining bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if !p.cfg.Paused {
		p.mu.Unlock()
		return nil
	}
	fileBackedWorkflow := p.workflowSource.Hash != ""
	p.mu.Unlock()

	if fileBackedWorkflow {
		if err := p.reconcileWorkflow(ctx); err != nil {
			return fmt.Errorf("reload workflow before unpause: %w", err)
		}
	}

	p.mu.Lock()
	if !p.cfg.Paused {
		p.mu.Unlock()
		return nil
	}
	p.cfg.Paused = false
	running := p.done != nil
	if !running {
		p.orchConfig = projectOrchestratorConfig(p.cfg, p.workflow)
		p.orchestrator = nil
	}
	p.mu.Unlock()

	if !running {
		if err := p.start(ctx, startOptions{provision: true, publishEvents: true, draining: draining}); err != nil {
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

func (p *Project) Close() error {
	return p.close(context.Background(), true)
}

func (p *Project) close(ctx context.Context, publishEvents bool) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	running := p.done != nil
	p.mu.Unlock()

	if running {
		if err := p.stop(ctx, publishEvents); err != nil && !errors.Is(err, ErrNotRunning) {
			return err
		}
	}
	if err := p.closeConnector(); err != nil {
		return err
	}

	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *Project) closeConnector() error {
	p.mu.Lock()
	projectConnector := p.connector
	scheduleOwner := p.scheduleOwner
	p.mu.Unlock()

	return errors.Join(closeConnector(projectConnector), closeScheduleOwner(scheduleOwner))
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
	if p.closed {
		p.mu.Unlock()
		return ErrProjectStopped
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
	started := logProjectShutdownBoundaryBegin(p.logger, "project_wait_done", "component", "project", "project_id", p.id)
	if err := ctx.Err(); err != nil {
		logProjectShutdownBoundaryEnd(p.logger, "project_wait_done", started, err, "component", "project", "project_id", p.id)
		return err
	}
	select {
	case <-done:
		if err := ctx.Err(); err != nil {
			logProjectShutdownBoundaryEnd(p.logger, "project_wait_done", started, err, "component", "project", "project_id", p.id)
			return err
		}
		p.mu.Lock()
		defer p.mu.Unlock()

		logProjectShutdownBoundaryEnd(p.logger, "project_wait_done", started, p.runErr, "component", "project", "project_id", p.id)
		return p.runErr
	case <-ctx.Done():
		err := ctx.Err()
		logProjectShutdownBoundaryEnd(p.logger, "project_wait_done", started, err, "component", "project", "project_id", p.id)
		return err
	}
}

func (p *Project) run(ctx context.Context, done chan struct{}, orch *orchestrator.Orchestrator) {
	watcherCtx, stopWatcher := context.WithCancel(ctx)
	watcherDone := p.startWorkflowWatcher(watcherCtx)

	runStarted := logProjectShutdownBoundaryBegin(p.logger, "orchestrator_run", "component", "orchestrator", "project_id", p.id)
	err := orch.Run(ctx)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		logProjectShutdownBoundaryEndResult(p.logger, "orchestrator_run", runStarted, "canceled", nil, "component", "orchestrator", "project_id", p.id)
	} else {
		logProjectShutdownBoundaryEnd(p.logger, "orchestrator_run", runStarted, err, "component", "orchestrator", "project_id", p.id)
	}
	watcherStarted := logProjectShutdownBoundaryBegin(p.logger, "workflow_watcher_stop", "component", "workflow_watcher", "project_id", p.id)
	stopWatcher()
	if watcherDone != nil {
		<-watcherDone
		logProjectShutdownBoundaryEnd(p.logger, "workflow_watcher_stop", watcherStarted, nil, "component", "workflow_watcher", "project_id", p.id)
	} else {
		logProjectShutdownBoundaryEndResult(p.logger, "workflow_watcher_stop", watcherStarted, "skipped", nil, "component", "workflow_watcher", "project_id", p.id)
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
		if err != nil {
			p.runtimeErr = RuntimeError{Message: err.Error(), At: time.Now().UTC(), Terminal: true}
		}
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

func logProjectShutdownBoundaryBegin(logger *slog.Logger, operation string, attrs ...any) time.Time {
	if logger == nil {
		logger = slog.Default()
	}
	started := time.Now()
	args := projectShutdownLogArgs(operation, append([]any{"phase", "begin"}, attrs...)...)
	logger.Debug("shutdown boundary begin", args...)
	return started
}

func logProjectShutdownBoundaryEnd(logger *slog.Logger, operation string, started time.Time, err error, attrs ...any) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	logProjectShutdownBoundaryEndResult(logger, operation, started, result, err, attrs...)
}

func logProjectShutdownBoundaryEndResult(logger *slog.Logger, operation string, started time.Time, result string, err error, attrs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	if result == "" {
		result = "ok"
	}
	args := projectShutdownLogArgs(operation,
		"phase", "end",
		"duration", time.Since(started),
		"result", result,
	)
	if err != nil {
		args = append(args, "error", err)
	}
	args = append(args, attrs...)
	logger.Debug("shutdown boundary end", args...)
}

func projectShutdownLogArgs(operation string, attrs ...any) []any {
	args := make([]any, 0, 2+len(attrs))
	args = append(args, "operation", operation)
	args = append(args, attrs...)
	return args
}

func (p *Project) startWorkflowWatcher(ctx context.Context) <-chan struct{} {
	path := strings.TrimSpace(p.cfg.Workflow)
	if path == "" {
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		reconcileTicker := time.NewTicker(p.workflowReconcileCadence())
		defer reconcileTicker.Stop()
		retryTimer := time.NewTimer(workflowWatcherInitialBackoff)
		if !retryTimer.Stop() {
			<-retryTimer.C
		}
		defer retryTimer.Stop()

		var updates <-chan configwatcher.Update
		var stopWatch context.CancelFunc
		var retryC <-chan time.Time
		backoff := workflowWatcherInitialBackoff
		arm := func() error {
			watcher, err := p.watcher(path)
			if err != nil {
				return fmt.Errorf("create workflow watcher: %w", err)
			}
			watchCtx, cancel := context.WithCancel(ctx)
			watchUpdates, err := watcher.Watch(watchCtx)
			if err != nil {
				cancel()
				return fmt.Errorf("watch workflow: %w", err)
			}
			updates = watchUpdates
			stopWatch = cancel
			p.setWorkflowWatcherArmed(true)
			return nil
		}
		scheduleRetry := func(err error) {
			p.setWorkflowWatcherArmed(false)
			p.logger.Warn("workflow watcher stopped; re-establishing",
				"project_id", p.id,
				"path", path,
				"backoff", backoff,
				"error", err,
			)
			resetProjectTimer(retryTimer, backoff)
			retryC = retryTimer.C
			backoff = min(backoff*2, workflowWatcherMaxBackoff)
		}

		if err := arm(); err != nil {
			scheduleRetry(err)
		}

		for {
			select {
			case <-ctx.Done():
				if stopWatch != nil {
					stopWatch()
				}
				p.setWorkflowWatcherArmed(false)
				return
			case <-reconcileTicker.C:
				if err := p.reconcileWorkflow(ctx); err != nil {
					continue
				}
			case <-retryC:
				retryC = nil
				if err := arm(); err != nil {
					scheduleRetry(err)
				}
			case update, ok := <-updates:
				if !ok {
					if ctx.Err() != nil {
						p.setWorkflowWatcherArmed(false)
						return
					}
					updates = nil
					if stopWatch != nil {
						stopWatch()
						stopWatch = nil
					}
					scheduleRetry(errors.New("workflow watcher update channel closed"))
					continue
				}
				p.recordWorkflowWatchEvent(update.At)
				backoff = workflowWatcherInitialBackoff
				if update.WatcherErr {
					if ctx.Err() != nil {
						p.setWorkflowWatcherArmed(false)
						return
					}
					updates = nil
					if stopWatch != nil {
						stopWatch()
						stopWatch = nil
					}
					scheduleRetry(update.Err)
					continue
				}
				if err := p.handleWorkflowUpdate(ctx, update); err != nil {
					continue
				}
			}
		}
	}()
	return done
}

func resetProjectTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func (p *Project) workflowReconcileCadence() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.workflowReconcileInterval > 0 {
		return p.workflowReconcileInterval
	}
	interval := time.Duration(p.workflow.Config.Polling.IntervalMS) * time.Millisecond
	if interval <= 0 {
		return time.Duration(workflowconfig.DefaultPollingIntervalMS) * time.Millisecond
	}
	return interval
}

func (p *Project) setWorkflowWatcherArmed(armed bool) {
	p.mu.Lock()
	p.workflowSource.WatcherArmed = armed
	p.mu.Unlock()
}

func (p *Project) recordWorkflowWatchEvent(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	p.mu.Lock()
	p.workflowSource.LastWatchEventAt = at.UTC()
	p.mu.Unlock()
}

func (p *Project) reconcileWorkflow(ctx context.Context) error {
	p.mu.Lock()
	projectConfig := p.cfg
	loadedHash := p.workflowSource.Hash
	scheduling := p.policyScheduling
	loadedSelection := p.workflow.Config.Agents.ModelSelection
	loadedPolicy := p.workflow.Config.Policy
	p.mu.Unlock()

	workflow, err := loadWorkflowForScheduling(ctx, projectConfig, p.policyScheduling)
	now := time.Now().UTC()
	p.mu.Lock()
	p.workflowSource.LastReconcileAt = now
	p.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			return p.workflowReloadError("workflow reconcile failed", workflowSourceDisplayPath(projectConfig), err)
		}
		return fmt.Errorf("load workflow: %w", err)
	}
	workflow.Config = WithMappedNativeTracker(workflow.Config, scheduling, normalizeProjectID(ID(projectConfig.ID)))
	if workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		if err := configureProjectPolicy(ctx, projectConfig, &workflow, scheduling); err != nil {
			return p.workflowReloadError(policyReloadFailure(err), workflowSourceDisplayPath(projectConfig), err)
		}
	}
	policyChanged := workflow.Config.Tracker.Kind == workflowconfig.TrackerHubNative && workflow.Config.Policy.ID != loadedPolicy.ID
	if (workflow.SourceHash == "" || workflow.SourceHash == loadedHash) && !policyChanged && reflect.DeepEqual(loadedSelection, workflow.Config.Agents.ModelSelection) {
		p.clearWorkflowReloadError()
		return nil
	}

	path := workflowSourceDisplayPath(projectConfig)
	p.logger.Warn("workflow reconcile detected stale config", "project_id", p.id, "path", path)
	return p.handleWorkflowUpdate(ctx, configwatcher.Update{Path: path, Workflow: workflow, At: now})
}

func (p *Project) handleWorkflowUpdate(ctx context.Context, update configwatcher.Update) error {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	return p.applyWorkflowUpdate(ctx, update, false)
}

func (p *Project) applyWorkflowUpdate(ctx context.Context, update configwatcher.Update, managed bool) error {

	if update.Err != nil {
		return p.workflowReloadError("workflow reload failed", update.Path, update.Err)
	}

	p.mu.Lock()
	projectConfig := p.cfg
	githubToken := p.githubToken
	connectorFactory := p.connectorFactory
	schedulerFactory := p.schedulerFactory
	runner := p.runner
	projectOrchestrator := p.orchestrator
	projectRunning := p.done != nil
	scheduleConfig := p.scheduleConfig
	scheduling := p.policyScheduling
	p.mu.Unlock()
	workflow := normalizeWorkflow(update.Workflow)
	workflow.Config = applyProjectAgentDefaults(workflow.Config, projectConfig)
	workflow.Config = WithMappedNativeTracker(workflow.Config, scheduling, normalizeProjectID(ID(projectConfig.ID)))
	if err := configureProjectPolicy(ctx, projectConfig, &workflow, scheduling); err != nil {
		return p.workflowReloadError(policyReloadFailure(err), update.Path, err)
	}
	if managed && update.Workflow.Config.Policy.ID != "" && (workflow.Config.Policy.ID != update.Workflow.Config.Policy.ID || workflow.Config.Policy.SourceRevision != update.Workflow.Config.Policy.SourceRevision) {
		return p.workflowReloadError("repository policy reload rejected", update.Path, errors.New("the approved policy changed during application; read the current policy before retrying"))
	}
	workflowActiveHours := workflow.Config.ActiveHours.Normalize()
	workflow.Config = workflowConfigWithProjectIdentity(projectConfig, workflow.Config)
	workflow.Config = workflowConfigWithGitHubToken(workflow.Config, githubToken)
	if err := workflow.Config.Validate(); err != nil {
		return p.workflowReloadError("workflow reload validation failed", update.Path, err)
	}
	if err := workflowconfig.ValidateWorkflowAdmission(workflow); err != nil {
		return p.workflowReloadError("workflow reload validation failed", update.Path, err)
	}
	if workflow.Config.ScheduleOwnership != scheduleConfig {
		return p.workflowReloadError("workflow reload validation failed", update.Path, errors.New("schedule_ownership changes require a Detent restart"))
	}
	projectConnector, err := buildConnector(ctx, workflow.Config, connectorFactory, projectConfig.Paused)
	if err != nil {
		return p.workflowReloadError("workflow reload connector failed", update.Path, err)
	}

	projectScheduler, err := buildScheduler(workflow.Config, schedulerFactory)
	if err != nil {
		return p.workflowReloadError("workflow reload scheduler failed", update.Path, err)
	}
	releaseBuild, err := buildReleaseCoordinator(workflow.Config, projectConnector)
	if err != nil {
		return p.workflowReloadError("workflow reload release coordinator failed", update.Path, err)
	}
	releaseCoordinator := releaseBuild.coordinator

	applyRunner, err := prepareRunnerWorkflow(runner, workflow)
	if err != nil {
		return p.workflowReloadError("prepare agent configuration rejected", update.Path, err)
	}
	runtimeConfig := projectOrchestratorConfig(projectConfig, workflow)
	if projectOrchestrator != nil && projectRunning {
		if err := projectOrchestrator.UpdateRuntime(ctx, orchestrator.RuntimeUpdate{
			Config:         runtimeConfig,
			Connector:      projectConnector,
			Release:        releaseCoordinator,
			ReplaceRelease: true,
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return p.workflowReloadError("apply workflow reload failed", update.Path, err)
		}
	}
	applyRunner()

	p.mu.Lock()
	p.workflow = workflow
	p.workflowActiveHours = workflowActiveHours
	p.workflowSource.Hash = workflow.SourceHash
	p.workflowSource.Revision = workflow.Definition.Revision
	p.workflowSource.Layout = workflow.Definition.Layout
	p.workflowSource.ModifiedAt = workflowFileModifiedAt(projectConfig)
	if update.At.IsZero() {
		p.workflowSource.LoadedAt = time.Now().UTC()
	} else {
		p.workflowSource.LoadedAt = update.At.UTC()
	}
	p.workflowSource.LastReloadError = ""
	p.workflowSource.ReloadFailedAt = time.Time{}
	p.connector = projectConnector
	p.scheduler = projectScheduler
	p.orchConfig = runtimeConfig
	p.orchDeps.Connector = projectConnector
	p.orchDeps.Release = releaseCoordinator
	publishEvents := p.lifecycleEvents
	id := p.id
	p.mu.Unlock()
	for _, warning := range workflow.Config.ValidationWarnings() {
		if strings.HasPrefix(warning, "tracker.") {
			p.logger.Warn("workflow configuration warning", "project_id", id, "warning", warning)
		}
	}
	p.logger.Info("workflow reloaded", "project_id", p.id, "path", update.Path)
	if publishEvents {
		p.publish(Event{
			ProjectID: id,
			Kind:      EventWorkflowReloaded,
			At:        time.Now(),
		})
	}
	return nil
}

const ProviderStartupFailed = "agent provider startup failed"

func IsProviderStartupFailure(message string) bool {
	for line := range strings.SplitSeq(message, "\n") {
		if strings.HasPrefix(line, ProviderStartupFailed+": ") {
			return true
		}
	}
	return false
}

func policyReloadFailure(err error) string {
	if errors.Is(err, providercapacity.ErrCatalogUnavailable) {
		return ProviderStartupFailed
	}
	return "repository policy reload rejected"
}

func (p *Project) workflowReloadError(message, path string, err error) error {
	p.logger.Warn(message, "project_id", p.id, "path", path, "error", err)
	reloadErr := fmt.Errorf("%s: %w", message, err)
	p.mu.Lock()
	p.workflowSource.LastReloadError = reloadErr.Error()
	p.workflowSource.ReloadFailedAt = time.Now().UTC()
	p.mu.Unlock()
	return reloadErr
}

func (p *Project) clearWorkflowReloadError() {
	p.mu.Lock()
	p.workflowSource.LastReloadError = ""
	p.workflowSource.ReloadFailedAt = time.Time{}
	p.mu.Unlock()
}

type releaseCoordinatorBuild struct {
	coordinator releasepkg.Coordinator
}

func buildReleaseCoordinator(cfg workflowconfig.Config, projectConnector connector.Connector) (releaseCoordinatorBuild, error) {
	if !cfg.Release.Enabled {
		return releaseCoordinatorBuild{}, nil
	}
	releaseBackend, ok := projectConnector.(releasepkg.Backend)
	if !ok {
		return releaseCoordinatorBuild{}, errors.New("create release coordinator: connector does not support releases")
	}
	return releaseCoordinatorBuild{coordinator: releasepkg.New(releasepkg.Config{
		Enabled:            cfg.Release.Enabled,
		MinMergedIssues:    cfg.Release.MinMergedIssues,
		MaxAge:             time.Duration(cfg.Release.MaxAgeHours) * time.Hour,
		RequireGreenCI:     cfg.Release.RequireGreenCI,
		VersionBump:        cfg.Release.VersionBump,
		RerunFlakyOnce:     cfg.Release.RerunFlakyOnce,
		FlakyCheckNames:    append([]string(nil), cfg.Release.FlakyCheckNames...),
		RequiredCheckNames: append([]string(nil), cfg.Release.RequiredCheckNames...),
	}, releaseBackend)}, nil
}

// WithMappedNativeTracker makes a project that client.native_projects maps to
// a Hub project use the Hub instead of the repository's GitHub tracker, so a
// runner host can reuse a committed detent.yaml without a local override. Any
// other tracker kind is an explicit local choice and is kept.
func WithMappedNativeTracker(workflow workflowconfig.Config, scheduling orchestrator.SchedulingSource, id ID) workflowconfig.Config {
	source, ok := scheduling.(interface {
		ConnectorForProject(string) (connector.Connector, bool)
	})
	if !ok {
		return workflow
	}
	_, mapped := source.ConnectorForProject(string(id))
	return MapNativeTracker(workflow, mapped)
}

func projectSchedulingSource(source orchestrator.SchedulingSource, workflow workflowconfig.Config) orchestrator.SchedulingSource {
	if workflow.Tracker.Kind == workflowconfig.TrackerHubNative {
		return source
	}
	if workflow.Tracker.Kind != workflowconfig.TrackerGitHub || strings.TrimSpace(workflow.Tracker.Repository) == "" {
		return nil
	}
	return source
}

func projectOrchestratorConfig(project globalconfig.Project, definition workflowconfig.Workflow) orchestrator.Config {
	workflow := workflowConfigWithProjectIdentity(project, definition.Config)
	cfg := orchestrator.ConfigFromWorkflow(workflow)
	cfg.AutoPromote.ValidatorWorkflow = definition.SharedPrompt
	cfg.LocalIntakeDisabled = !project.LocalIntakeOn()
	cfg.HostCache = project.GlobalCache.Normalized()
	memory := project.EffectiveMemory()
	cfg.MemoryPressureSomeAvg60Max = memory.PressureSomeAvg60Threshold
	cfg.MemoryPressurePollInterval = time.Duration(memory.PollIntervalMS) * time.Millisecond
	cfg.IOPressureFullAvg10Max = project.GlobalIO.PressureFullAvg10Threshold
	cfg.IOPressureDegradedMaxAgents = project.GlobalIO.DegradedMaxConcurrentAgents
	cfg.IOPressurePollInterval = time.Duration(project.GlobalIO.PollIntervalMS) * time.Millisecond
	cfg.CPUPressureSomeAvg10Max = project.GlobalCPU.PressureSomeAvg10Threshold
	cfg.CPUPressureDegradedMaxAgents = project.GlobalCPU.DegradedMaxConcurrentAgents
	cfg.CPUPressurePollInterval = time.Duration(project.GlobalCPU.PollIntervalMS) * time.Millisecond
	overrideUntil := activehours.ParsePersistedOverride(project.ActiveHoursOverrideUntil)
	cfg.Project = scheduler.ProjectCandidate{
		ID:                       project.ID,
		Rank:                     project.Priority,
		Pool:                     project.Pool,
		Paused:                   project.Paused,
		ActiveHours:              workflow.ActiveHours,
		ActiveHoursOverrideUntil: overrideUntil,
	}
	cfg.SchedulingRepository = workflow.Tracker.Repository
	cfg.Authorization = combineAuthorizationSelectors(cfg.Authorization, project.Authorization)
	return cfg
}

func workflowConfigWithProjectIdentity(
	project globalconfig.Project,
	workflow workflowconfig.Config,
) workflowconfig.Config {
	workflow.Agent.RateWindowPacing = effectiveRateWindowPacing(project, workflow)
	workflow.ActiveHours = EffectiveActiveHours(project, workflow.ActiveHours)
	workflow = workflowConfigWithProjectPaths(project, workflow)
	if workflow.Agent.Knowledge.Enabled {
		workflow.Agent.Knowledge = workflowconfig.KnowledgeWithSources(
			project.GlobalKnowledge,
			project.Knowledge,
			workflow.Agent.Knowledge,
		)
	} else {
		workflow.Agent.Knowledge.Normalize()
	}
	if !project.Identity.Configured() {
		return workflow
	}
	identity := project.Identity
	identity.Normalize()
	workflow.Identity = identity
	return workflow
}

func effectiveRateWindowPacing(project globalconfig.Project, workflow workflowconfig.Config) workflowconfig.RateWindowPacing {
	if workflow.RateWindowPacingConfigured() {
		return workflow.Agent.RateWindowPacing.Normalized()
	}
	return project.GlobalRateWindowPacing.Normalized()
}

func buildScheduleOwnership(
	cfg workflowconfig.Config,
	deps Dependencies,
	logger *slog.Logger,
	state func(error),
) (*scheduleowner.Manager, *scheduleowner.IssueCoordinator, scheduleowner.Config, error) {
	if cfg.Tracker.Kind == workflowconfig.TrackerHubNative && cfg.ScheduleOwnership.Enabled && strings.TrimSpace(cfg.ScheduleOwnership.Repository) == "" {
		return nil, nil, scheduleowner.Config{}, errors.New("native schedule ownership requires an explicit schedule_ownership.repository to opt into GitHub coordination")
	}
	ownership := cfg.NormalizedScheduleOwnership()
	if !ownership.Enabled {
		return nil, nil, ownership, nil
	}
	coordinationStore := deps.ScheduleStore
	var coordinationCloser io.Closer
	if coordinationStore == nil {
		httpClient := ghconnector.NewPooledHTTPClient(ghconnector.HTTPTransportConfig{
			MaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
			MaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
			IdleConnTimeout:     time.Duration(cfg.Tracker.HTTPIdleConnTimeoutMS) * time.Millisecond,
		})
		coordinationToken := strings.TrimSpace(deps.GitHubToken)
		if coordinationToken == "" {
			coordinationToken = strings.TrimSpace(cfg.Tracker.APIKey)
		}
		var tokenSource ghconnector.TokenSource = ghconnector.NewTokenResolver(ghconnector.TokenResolverConfig{
			Endpoint:                ownership.Endpoint,
			APIKey:                  coordinationToken,
			GitHubAppID:             cfg.Tracker.GitHubAppID,
			GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
			HTTPClient:              httpClient,
		})
		if deps.RefreshGitHubToken != nil && coordinationToken != "" {
			tokenSource = ghconnector.NewRefreshableTokenSource(coordinationToken, deps.RefreshGitHubToken)
		}
		client, err := ghconnector.NewClient(ghconnector.ClientConfig{
			Endpoint:    ownership.Endpoint,
			Project:     ownership.Key,
			TokenSource: tokenSource,
			HTTPClient:  httpClient,
			Logger:      logger,
		})
		if err != nil {
			return nil, nil, ownership, errors.Join(err, httpClient.Close())
		}
		githubStore, storeErr := coordination.NewGitHubRefStore(coordination.GitHubRefConfig{
			Repository: ownership.Repository,
			Branch:     ownership.Branch,
			Client:     client,
		})
		if storeErr != nil {
			return nil, nil, ownership, errors.Join(storeErr, client.Close())
		}
		coordinationStore = githubStore
		coordinationCloser = githubStore
	}
	owner := strings.TrimSpace(deps.ScheduleOwner)
	if owner == "" {
		hostname, hostnameErr := os.Hostname()
		if hostnameErr != nil {
			if coordinationCloser != nil {
				return nil, nil, ownership, errors.Join(hostnameErr, coordinationCloser.Close())
			}
			return nil, nil, ownership, hostnameErr
		}
		owner = hostname
		owner = strings.TrimSpace(owner)
	}
	manager, err := scheduleowner.New(ownership, owner, coordinationStore, scheduleowner.Dependencies{Closer: coordinationCloser, Logger: logger, State: state})
	if err != nil {
		if coordinationCloser != nil {
			return nil, nil, ownership, errors.Join(err, coordinationCloser.Close())
		}
		return nil, nil, ownership, err
	}
	coordinator, err := scheduleowner.NewIssueCoordinator(ownership, coordinationStore, scheduleowner.Dependencies{})
	if err != nil {
		return nil, nil, ownership, errors.Join(err, manager.Close())
	}
	return manager, coordinator, ownership, nil
}

func EffectiveActiveHours(project globalconfig.Project, workflow activehours.Config) activehours.Config {
	if project.ActiveHours != nil {
		return project.ActiveHours.Normalize()
	}
	if project.GlobalActiveHours != nil {
		return project.GlobalActiveHours.Normalize()
	}
	return workflow.Normalize()
}

func workflowConfigWithProjectPaths(project globalconfig.Project, workflow workflowconfig.Config) workflowconfig.Config {
	workdir := strings.TrimSpace(project.Workdir)
	if workdir == "" {
		return workflow
	}

	workflow.Workspace.Root = projectRelativePath(workdir, workflow.Workspace.Root)
	workflow.Workspace.SourceRoot = projectRelativePath(workdir, workflow.Workspace.SourceRoot)
	if workflow.Workspace.Kind == workflowconfig.WorkspaceLocalGit && workflow.Workspace.SourceRoot == "" {
		workflow.Workspace.SourceRoot = workdir
	}
	if workflow.Workspace.Kind == workflowconfig.WorkspaceFilesystem {
		workflow.Workspace.OutputRoot = projectRelativePath(workdir, workflow.Workspace.OutputRoot)
	}
	if workflow.Deliverable.Kind == workflowconfig.DeliverableArtifact {
		workflow.Deliverable.OutputRoot = projectRelativePath(workdir, workflow.Deliverable.OutputRoot)
	}
	workflow.Agent.Knowledge = knowledgeWithProjectRelativePaths(workdir, workflow.Agent.Knowledge)
	return workflow
}

func knowledgeWithProjectRelativePaths(base string, cfg workflowconfig.Knowledge) workflowconfig.Knowledge {
	for index := range cfg.Sources {
		cfg.Sources[index].Path = projectRelativePath(base, cfg.Sources[index].Path)
	}
	return cfg
}

func projectRelativePath(base string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~/") {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

func workflowConfigWithGitHubToken(workflow workflowconfig.Config, token string) workflowconfig.Config {
	return workflow.WithRuntimeGitHubToken(token)
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

func (p *Project) recordRuntimeError(err error) {
	if p == nil || err == nil {
		return
	}

	p.recordRuntimeErrorState(err, time.Now().UTC(), time.Time{}, true)
}

func (p *Project) recordRuntimeErrorState(err error, at time.Time, nextRetryAt time.Time, terminal bool) {
	if p == nil || err == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.runtimeErr = RuntimeError{
		Message:     err.Error(),
		At:          at.UTC(),
		NextRetryAt: nextRetryAt.UTC(),
		Terminal:    terminal,
		Transient:   transientStartupError(err),
	}
}

type workflowUpdater interface {
	UpdateWorkflow(workflowconfig.Workflow)
}

func prepareRunnerWorkflow(value orchestrator.Runner, workflow workflowconfig.Workflow) (func(), error) {
	if updater, ok := value.(interface {
		PrepareWorkflowUpdate(workflowconfig.Workflow) (func(), error)
	}); ok {
		return updater.PrepareWorkflowUpdate(workflow)
	}
	return func() {
		if updater, ok := value.(workflowUpdater); ok {
			updater.UpdateWorkflow(workflow)
		}
	}, nil
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
	return func(cfg workflowconfig.Config) (connector.Connector, error) {
		return defaultConnectorFactoryWithRefresh(cfg, deps.RefreshGitHubToken)
	}
}

func buildConnector(ctx context.Context, cfg workflowconfig.Config, connectorFactory ConnectorFactory, paused bool) (connector.Connector, error) {
	projectConnector, err := connectorFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: create project connector: %w", ErrConnectorCreation, err)
	}
	if projectConnector == nil {
		return nil, ErrMissingConnector
	}

	if !paused {
		refreshMergeQueuePolicy(ctx, projectConnector)
	}
	return projectConnector, nil
}

func refreshMergeQueuePolicy(ctx context.Context, projectConnector connector.Connector) {
	refresher, ok := projectConnector.(interface{ RefreshMergeQueuePolicy(context.Context) error })
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := refresher.RefreshMergeQueuePolicy(ctx); err != nil {
		slog.Warn("refresh github branch merge policy failed; retaining pull request inspection", "error", err)
	}
}

func closeConnector(projectConnector connector.Connector) error {
	closer, ok := projectConnector.(connector.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

func closeScheduleOwner(owner *scheduleowner.Manager) error {
	if owner == nil {
		return nil
	}
	return owner.Close()
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
	dispatch := orchestrator.ConfigFromWorkflow(cfg)
	projectScheduler, err := scheduler.NewFromConfig(scheduler.Config{
		Capacity:        cfg.Agent.MaxConcurrentAgents,
		CapacityByState: dispatch.MaxConcurrentAgentsByState,
		CapacityPerHost: maxConcurrentAgentsPerHost(cfg),
	})
	if err != nil {
		return nil, err
	}

	return projectScheduler, nil
}

func defaultConnectorFactoryWithRefresh(cfg workflowconfig.Config, refreshGitHubToken func(context.Context) (string, error)) (connector.Connector, error) {
	return factory.NewFromConfig(factory.Config{
		Kind: cfg.Tracker.Kind,

		Endpoint:                    cfg.Tracker.Endpoint,
		APIKey:                      cfg.Tracker.APIKey,
		GitHubTokenRefresh:          refreshGitHubToken,
		HTTPMaxIdleConns:            cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost:     cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:       cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubRESTMinReserve:        cfg.Tracker.GitHubRESTMinReserve,
		GitHubGraphQLMinReserve:     cfg.Tracker.GitHubGraphQLMinReserve,
		GitHubRESTFanoutMaxRequests: cfg.Tracker.GitHubRESTFanoutMaxRequests,
		GitHubUnstartedSeconds:      cfg.Tracker.GitHubUnstartedSeconds,
		GitHubRESTDebugLogging:      cfg.Tracker.GitHubRESTDebugLogging,
		ConditionalRequests:         &cfg.Polling.Conditional,
		GitHubAppID:                 cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:         cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath:     cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID:     cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:          cfg.Tracker.GitHubStatusSource,
		DependencySource:            cfg.Dependencies.Source,
		ProjectSlug:                 cfg.Tracker.ProjectSlug,
		Repository:                  cfg.Tracker.Repository,
		StatusField:                 cfg.Tracker.StatusField,
		StatusLabelPrefix:           cfg.Tracker.StatusLabelPrefix,
		ActiveStates:                cfg.Tracker.ActiveStates,
		ObservedStates:              cfg.Tracker.ObservedStates,
		PlanStop:                    cfg.Plan.Stop,
		TerminalStates:              cfg.Tracker.TerminalStates,
		StateMap:                    trackerStateMap(cfg.Tracker.StateMap),
		PriorityMap:                 trackerPriorityMap(cfg.Tracker.PriorityMap),
		RequiredStatusChecks:        cfg.Gate.RequiredStatusChecks,
		LocalStatus:                 cfg.Gate.LocalStatus,
		Publication:                 cfg.Tracker.Publication,
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

func resolveWorkflowWatcherFactory(
	ctx context.Context,
	deps Dependencies,
	project globalconfig.Project,
	githubToken string,
	logger *slog.Logger,
	currentProject func() globalconfig.Project,
) WorkflowWatcherFactory {
	if deps.WorkflowWatcherFactory != nil {
		return deps.WorkflowWatcherFactory
	}
	if strings.TrimSpace(project.WorkflowRef) != "" {
		return func(string) (WorkflowWatcher, error) {
			watcher, err := newGitRefWorkflowWatcher(project, 0, logger)
			if err != nil {
				return nil, err
			}
			watcher.currentProject = currentProject
			watcher.fallback = func(ctx context.Context) (workflowconfig.Workflow, error) {
				return loadWorkflowForScheduling(ctx, currentProject(), deps.Scheduling)
			}
			return watcher, nil
		}
	}
	return func(path string) (WorkflowWatcher, error) {
		return configwatcher.New(path,
			configwatcher.WithLoader(func(path string) (workflowconfig.Workflow, error) {
				project := currentProject()
				workflow, err := loadWorkflowForScheduling(context.WithoutCancel(ctx), project, deps.Scheduling)
				if err != nil {
					return workflow, err
				}
				workflow = normalizeWorkflow(workflow)
				workflow.Config = applyProjectAgentDefaults(workflow.Config, project)
				workflow.Config = workflowConfigWithProjectIdentity(project, workflow.Config)
				workflow.Config = workflowConfigWithGitHubToken(workflow.Config, githubToken)
				return workflow, nil
			}),
			configwatcher.WithLogger(logger),
		)
	}
}

func normalizeWorkflow(workflow workflowconfig.Workflow) workflowconfig.Workflow {
	if workflow.SharedPrompt == "" && workflow.Overlay.Path == "" {
		workflow.SharedPrompt = workflow.Prompt
	}
	if !emptyWorkflowConfig(workflow.Config) {
		return workflow
	}

	workflow.Config = workflowconfig.Default()
	workflow.Config.Tracker.Kind = workflowconfig.TrackerHubNative
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

func normalizeProjectID(id ID) ID {
	return ID(strings.TrimSpace(string(id)))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func applyProjectAgentDefaults(cfg workflowconfig.Config, project globalconfig.Project) workflowconfig.Config {
	if project.ModelSelection != nil {
		cfg.Agents.ModelSelection = *project.ModelSelection
	}
	return cfg.WithAgentDefaults(project.GlobalAgents, project.GlobalBudget).WithWorkerDefaults(project.GlobalWorker)
}
