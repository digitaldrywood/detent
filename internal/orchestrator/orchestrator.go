package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultPollInterval                 = 30 * time.Second
	defaultRunningReconcileInterval     = 2 * time.Minute
	defaultWorkspaceCleanupIdleTTL      = 24 * time.Hour
	defaultWorkspaceCleanupSweep        = 10 * time.Minute
	gitHubGraphQLPauseRemaining         = 100
	gitHubGraphQLBackoffRemaining       = 500
	defaultGitHubGraphQLWarnRemaining   = 500
	defaultGitHubGraphQLMinReserve      = 1000
	defaultGitHubRESTMinReserve         = 1000
	defaultMaxConcurrentAgents          = 1
	defaultMaxRetryBackoff              = 5 * time.Minute
	defaultOverloadRetryDelay           = 45 * time.Second
	defaultContinuationRetry            = time.Second
	defaultFailureRetryBaseDelay        = 10 * time.Second
	defaultFailureBreakerSameClassLimit = 5
	defaultFailureBreakerWindow         = time.Hour
	defaultFailureBreakerCooldown       = time.Hour
	maxMergeWorkerRunnerFailures        = 3
	instantFailureThreshold             = 5
	instantFailureMaxDuration           = 10 * time.Second
	instantFailureBlockedReasonPrefix   = "instant fail circuit breaker: "
	repeatedFailureThreshold            = 5
	repeatedFailureBlockedReasonPrefix  = "repeated failure circuit breaker: "
	tokenCeilingBlockedReasonPrefix     = "token ceiling circuit breaker: "
	continuationDispatchBackoff         = 100 * time.Millisecond
	runUpdateBufferSize                 = 128
	maxRecentEvents                     = 50
	blockedStatusState                  = "Blocked"
	blockedReasonDependency             = "blocked by non-terminal dependency"
	blockedReasonProjectStatus          = "blocked by project status"
	mergeWorkerTerminalStateMissing     = "merge worker completed without reaching a terminal issue or pull request state"
	mergeWorkerRetryExhaustedReason     = "merge_worker_retry_exhausted"
)

var (
	ErrMissingConnector = errors.New("orchestrator connector is required")
	ErrStopped          = errors.New("orchestrator stopped")
)

type Config struct {
	PollInterval                  time.Duration
	MaxConcurrentAgents           int
	MaxConcurrentAgentsByState    map[string]int
	DispatchPriorityByState       []string
	DispatchPriorityByLabel       []string
	PrioritizeUnblockers          bool
	MergeFastPathEnabled          bool
	MergeMethod                   string
	ResumeOrphanedSessions        bool
	StopRunTargetState            string
	StopRunPriorityNames          map[int]string
	MaxConcurrentAgentsPerHost    int
	MaxRetryBackoff               time.Duration
	OverloadRetryDelay            time.Duration
	NoProgressSpendLimitUSD       float64
	BillingMode                   string
	FailureBreaker                FailureBreakerConfig
	Project                       scheduler.ProjectCandidate
	Claiming                      ClaimingConfig
	AutoPromote                   AutoPromoteConfig
	Plan                          gate.PlanConfig
	DependencySource              string
	DependencyAutoUnblock         DependencyAutoUnblockConfig
	BlockedRecovery               BlockedRecoveryConfig
	BlockerAutoPromote            BlockerAutoPromoteConfig
	ActiveStates                  []string
	ObservedStates                []string
	TerminalStates                []string
	Authorization                 selector.Selector
	SelectorContext               selector.Context
	WorkerHosts                   []string
	BudgetRefusalCooldown         time.Duration
	WorkspaceCleanupIdleTTL       time.Duration
	WorkspaceCleanupSweepInterval time.Duration
	ContinuationRetryDelay        time.Duration
	FailureRetryBaseDelay         time.Duration
	SelectorPersona               string
	GitHubGraphQLWarnRemaining    int64
	GitHubGraphQLMinReserve       int64
	GitHubRESTMinReserve          int64
	OutputTruncationMaxBytes      int
	EfficiencyThresholds          efficiency.Thresholds
	Lessons                       LessonCaptureConfig
}

type LessonCaptureConfig struct {
	Enabled    bool
	Path       string
	MaxEntries int
}

type FailureBreakerConfig struct {
	SameClassLimit int
	Window         time.Duration
	Cooldown       time.Duration
}

type ClaimingConfig struct {
	Enabled           bool
	OwnershipMode     string
	Owner             string
	AssigneeLogin     string
	OwnerField        string
	LeaseField        string
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
}

type Dependencies struct {
	Connector          connector.Connector
	Runner             Runner
	WorkspaceReaper    WorkspaceReaper
	WorkflowMetrics    WorkflowMetricsRecorder
	Efficiency         efficiency.Recorder
	LifecycleExporter  efficiency.LifecycleExporter
	WorkAttempts       store.WorkAttemptStore
	ProgressSpend      store.ProgressSpendStore
	AgentResume        store.AgentResumeStore
	OrphanSessions     store.OrphanSessionStore
	ValidatorMemo      store.ValidatorMemoStore
	Activity           *activity.Broker
	Release            releasepkg.Coordinator
	GlobalDispatchGate scheduler.ProjectDispatchGate
	Now                func() time.Time
	Logger             *slog.Logger
	Retrospector       Retrospector
	WorkerProcesses    WorkerProcessStore
	ReapWorkerProcess  WorkerProcessReapFunc
	WorkerReapGrace    time.Duration
}

type WorkerProcessStore interface {
	ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error)
	MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error
}

type WorkerProcessReapFunc func(context.Context, procgroup.Identity, time.Duration) (procgroup.TerminationOutcome, error)

type Retrospector interface {
	Trigger(string)
}

type WorkspaceReapResult = runpkg.WorkspaceReapResult

type WorkspaceReaper = runpkg.WorkspaceReaper

type RuntimeUpdate struct {
	Config         Config
	Connector      connector.Connector
	Release        releasepkg.Coordinator
	ReplaceRelease bool
}

type Orchestrator struct {
	cfg                     Config
	connector               connector.Connector
	workflowMetrics         WorkflowMetricsRecorder
	efficiency              efficiency.Recorder
	lifecycleExporter       efficiency.LifecycleExporter
	workAttempts            store.WorkAttemptStore
	operatorStops           store.OperatorStopStore
	progressSpend           store.ProgressSpendStore
	agentResume             store.AgentResumeStore
	orphanSessions          store.OrphanSessionStore
	supervisor              *runpkg.Supervisor
	validator               Validator
	reaper                  WorkspaceReaper
	logger                  *slog.Logger
	globalDispatchGate      scheduler.ProjectDispatchGate
	validatorMu             sync.Mutex
	validatorWG             sync.WaitGroup
	validatorRuns           map[string]struct{}
	validatorResults        map[string]validatorStageResult
	validatorFailures       map[string]validatorStageFailure
	validatorMemo           store.ValidatorMemoStore
	activity                *activity.Broker
	release                 releasepkg.Coordinator
	capacityController      runpkg.CapacityController
	capacityStatus          runpkg.CapacityStatusController
	validatorCapacity       runpkg.ValidatorCapacityController
	dailyBudgetStatus       runpkg.DailyBudgetStatusProvider
	issueBudgetStatus       runpkg.IssueBudgetStatusProvider
	now                     func() time.Time
	retrospector            Retrospector
	workerProcesses         WorkerProcessStore
	reapWorkerProcess       WorkerProcessReapFunc
	workerReapGrace         time.Duration
	heartbeats              *heartbeatManager
	hydrationSkipStreaks    map[string]int
	hydrationWarned         bool
	dispatchGateSampleMu    sync.Mutex
	dispatchGateSamples     map[dispatchGateSampleKey]time.Time
	ciTriggerLabelMu        sync.Mutex
	ciTriggerLabelHeads     map[string]ciTriggerLabelHead
	stateRequests           chan stateRequest
	drainRequests           chan drainRequest
	forceRequests           chan forceRequest
	recoveryRequests        chan workAttemptRecoveryRequest
	operatorMoves           chan operatorMoveRequest
	configUpdates           chan configUpdateRequest
	refreshes               chan manualRefreshRequest
	reconciles              chan targetedRefreshRequest
	capacityClearRequests   chan capacityClearRequest
	failureCanaryRequests   chan failureBreakerCanaryRequest
	stopRequests            chan stopRunRequest
	runResults              chan runpkg.Completion
	runUpdates              chan runUpdate
	validatorCapacityEvents chan validatorCapacityEvent
	done                    chan struct{}
	pendingStops            map[string]*pendingStopRun
	pendingMergeRevocations map[string]mergeRevocation
	completedStops          map[string]StopRunResult
	refreshSeq              atomic.Uint64
}

type validatorStageResult struct {
	Result    gate.ValidatorResult
	Commented bool
}

type validatorStageFailure struct {
	Attempt     int
	NextRetryAt time.Time
	Error       string
}

type stateRequest struct {
	reply chan State
}

type drainRequest struct {
	at    time.Time
	reply chan struct{}
}

type forceRequest struct {
	ctx   context.Context //nolint:containedctx // ForceQuit carries caller cancellation through the event loop.
	at    time.Time
	reply chan error
}

type configUpdateRequest struct {
	update RuntimeUpdate
	reply  chan struct{}
}

type runUpdate struct {
	issueID string
	usage   runpkg.UsageUpdate
}

type capacityClearRequest struct {
	scope string
	at    time.Time
	reply chan capacityClearReply
}

type capacityClearReply struct {
	cleared []BackendOutage
}

type failureBreakerCanaryRequest struct {
	at    time.Time
	reply chan FailureBreakerCanaryResult
}

func New(cfg Config, deps Dependencies) (*Orchestrator, error) {
	cfg = normalizeConfig(cfg)
	if deps.Connector == nil {
		return nil, ErrMissingConnector
	}

	runner := deps.Runner
	if runner == nil {
		runner = FakeRunner{}
	}
	reaper := deps.WorkspaceReaper
	if reaper == nil {
		if candidate, ok := runner.(WorkspaceReaper); ok {
			reaper = candidate
		}
	}
	var validator Validator
	if candidate, ok := runner.(Validator); ok {
		validator = candidate
	}
	var capacityController runpkg.CapacityController
	if candidate, ok := runner.(runpkg.CapacityController); ok {
		capacityController = candidate
	}
	var validatorCapacity runpkg.ValidatorCapacityController
	if candidate, ok := runner.(runpkg.ValidatorCapacityController); ok {
		validatorCapacity = candidate
	}
	var capacityStatus runpkg.CapacityStatusController
	if candidate, ok := runner.(runpkg.CapacityStatusController); ok {
		capacityStatus = candidate
	}
	var dailyBudgetStatus runpkg.DailyBudgetStatusProvider
	if candidate, ok := runner.(runpkg.DailyBudgetStatusProvider); ok {
		dailyBudgetStatus = candidate
	}
	var issueBudgetStatus runpkg.IssueBudgetStatusProvider
	if candidate, ok := runner.(runpkg.IssueBudgetStatusProvider); ok {
		issueBudgetStatus = candidate
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	validatorMemo := deps.ValidatorMemo
	if validatorMemo == nil {
		if candidate, ok := deps.WorkflowMetrics.(store.ValidatorMemoStore); ok {
			validatorMemo = candidate
		}
	}
	agentResume := deps.AgentResume
	if agentResume == nil {
		if candidate, ok := deps.WorkAttempts.(store.AgentResumeStore); ok {
			agentResume = candidate
		}
	}
	orphanSessions := deps.OrphanSessions
	if orphanSessions == nil {
		if candidate, ok := deps.WorkAttempts.(store.OrphanSessionStore); ok {
			orphanSessions = candidate
		}
	}
	if orphanSessions == nil {
		if candidate, ok := deps.WorkflowMetrics.(store.OrphanSessionStore); ok {
			orphanSessions = candidate
		}
	}
	if agentResume == nil {
		if candidate, ok := deps.WorkflowMetrics.(store.AgentResumeStore); ok {
			agentResume = candidate
		}
	}
	progressSpend := deps.ProgressSpend
	if progressSpend == nil {
		if candidate, ok := deps.WorkflowMetrics.(store.ProgressSpendStore); ok {
			progressSpend = candidate
		}
	}
	var operatorStops store.OperatorStopStore
	if candidate, ok := deps.WorkAttempts.(store.OperatorStopStore); ok {
		operatorStops = candidate
	}
	if operatorStops == nil {
		if candidate, ok := deps.WorkflowMetrics.(store.OperatorStopStore); ok {
			operatorStops = candidate
		}
	}
	workerProcesses := deps.WorkerProcesses
	if workerProcesses == nil {
		if candidate, ok := deps.WorkAttempts.(WorkerProcessStore); ok {
			workerProcesses = candidate
		}
	}
	if workerProcesses == nil {
		if candidate, ok := deps.WorkflowMetrics.(WorkerProcessStore); ok {
			workerProcesses = candidate
		}
	}
	reapWorkerProcess := deps.ReapWorkerProcess
	if reapWorkerProcess == nil {
		reapWorkerProcess = procgroup.Terminate
	}
	workerReapGrace := deps.WorkerReapGrace
	if workerReapGrace <= 0 {
		workerReapGrace = procgroup.DefaultTerminationGrace
	}

	supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
		OverloadRetryDelay:    cfg.OverloadRetryDelay,
		Now:                   now,
		Logger:                logger,
	})
	if err != nil {
		return nil, err
	}

	orchestrator := &Orchestrator{
		cfg:                     cfg,
		connector:               deps.Connector,
		workflowMetrics:         deps.WorkflowMetrics,
		efficiency:              deps.Efficiency,
		lifecycleExporter:       deps.LifecycleExporter,
		workAttempts:            deps.WorkAttempts,
		operatorStops:           operatorStops,
		progressSpend:           progressSpend,
		agentResume:             agentResume,
		orphanSessions:          orphanSessions,
		supervisor:              supervisor,
		validator:               validator,
		reaper:                  reaper,
		logger:                  logger,
		globalDispatchGate:      deps.GlobalDispatchGate,
		validatorRuns:           map[string]struct{}{},
		validatorResults:        map[string]validatorStageResult{},
		validatorFailures:       map[string]validatorStageFailure{},
		validatorMemo:           validatorMemo,
		activity:                deps.Activity,
		release:                 deps.Release,
		retrospector:            deps.Retrospector,
		workerProcesses:         workerProcesses,
		reapWorkerProcess:       reapWorkerProcess,
		workerReapGrace:         workerReapGrace,
		capacityController:      capacityController,
		capacityStatus:          capacityStatus,
		validatorCapacity:       validatorCapacity,
		dailyBudgetStatus:       dailyBudgetStatus,
		issueBudgetStatus:       issueBudgetStatus,
		now:                     now,
		dispatchGateSamples:     map[dispatchGateSampleKey]time.Time{},
		ciTriggerLabelHeads:     map[string]ciTriggerLabelHead{},
		stateRequests:           make(chan stateRequest),
		drainRequests:           make(chan drainRequest),
		forceRequests:           make(chan forceRequest),
		recoveryRequests:        make(chan workAttemptRecoveryRequest),
		operatorMoves:           make(chan operatorMoveRequest),
		configUpdates:           make(chan configUpdateRequest),
		refreshes:               make(chan manualRefreshRequest, 1),
		reconciles:              make(chan targetedRefreshRequest, 128),
		capacityClearRequests:   make(chan capacityClearRequest),
		failureCanaryRequests:   make(chan failureBreakerCanaryRequest),
		stopRequests:            make(chan stopRunRequest),
		runResults:              make(chan runpkg.Completion, max(cfg.MaxConcurrentAgents, 1)),
		runUpdates:              make(chan runUpdate, runUpdateBufferSize),
		validatorCapacityEvents: make(chan validatorCapacityEvent, max(cfg.MaxConcurrentAgents, 1)),
		done:                    make(chan struct{}),
		pendingStops:            map[string]*pendingStopRun{},
		pendingMergeRevocations: map[string]mergeRevocation{},
		completedStops:          map[string]StopRunResult{},
	}
	orchestrator.heartbeats = newHeartbeatManager(cfg, deps.Connector, deps.WorkAttempts, now, logger)
	return orchestrator, nil
}

type ciTriggerLabelHead struct {
	HeadSHA string
	Pending bool
}

func (o *Orchestrator) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	defer close(o.done)
	defer o.markGlobalProjectIdle()
	heartbeatCtx, stopHeartbeats := context.WithCancel(ctx)
	heartbeatsDone := make(chan struct{})
	var heartbeatResults <-chan heartbeatResult
	if o.heartbeats != nil {
		heartbeatResults = o.heartbeats.results
	}
	go func() {
		defer close(heartbeatsDone)
		o.heartbeats.Run(heartbeatCtx)
	}()
	defer func() {
		stopHeartbeats()
		<-heartbeatsDone
	}()

	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()

	state := newState(o.cfg)
	defer o.validatorWG.Wait()
	defer o.releaseRunningSlots(&state)
	o.recoverDurableWorkAttempts(ctx, &state, time.Now())
	o.tick(ctx, &state, time.Now())
	resetTicker(ticker, state.PollInterval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			o.tick(ctx, &state, now)
			resetTicker(ticker, state.PollInterval)
		case request := <-o.refreshes:
			o.tickManual(ctx, &state, request)
			resetTicker(ticker, state.PollInterval)
		case request := <-o.reconciles:
			o.reconcileTarget(ctx, &state, request)
			resetTicker(ticker, state.PollInterval)
		case request := <-o.capacityClearRequests:
			request.reply <- capacityClearReply{cleared: o.clearBackendCapacity(&state, request.scope, request.at)}
		case request := <-o.failureCanaryRequests:
			result := o.requestProjectFailureBreakerCanary(&state, request.at)
			request.reply <- result
			if result.Requested {
				o.tick(ctx, &state, request.at)
				resetTicker(ticker, state.PollInterval)
			}
		case request := <-o.stopRequests:
			o.handleStopRunRequest(ctx, &state, request)
		case result := <-o.runResults:
			o.handleRunResult(ctx, &state, result)
		case update := <-o.runUpdates:
			o.handleRunUpdate(&state, update)
		case result := <-heartbeatResults:
			o.handleHeartbeatResult(&state, result)
		case event := <-o.validatorCapacityEvents:
			o.handleValidatorCapacityEvent(&state, event)
		case request := <-o.drainRequests:
			o.startDrain(&state, request.at)
			request.reply <- struct{}{}
		case request := <-o.forceRequests:
			request.reply <- o.forceQuit(request.ctx, &state, request.at)
		case request := <-o.recoveryRequests:
			response, err := o.handleWorkAttemptRecovery(ctx, &state, request.request, request.at)
			request.reply <- workAttemptRecoveryReply{response: response, err: err}
		case request := <-o.operatorMoves:
			request.reply <- o.handleOperatorMove(&state, request.request, request.at)
		case update := <-o.configUpdates:
			o.applyRuntimeUpdate(&state, update.update, ticker)
			update.reply <- struct{}{}
		case request := <-o.stateRequests:
			request.reply <- state.clone()
		}
	}
}

func (o *Orchestrator) ClearBackendCapacity(ctx context.Context, scope string) ([]BackendOutage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request := capacityClearRequest{
		scope: strings.TrimSpace(scope),
		at:    o.clockNow(),
		reply: make(chan capacityClearReply, 1),
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.done:
		return nil, ErrStopped
	case o.capacityClearRequests <- request:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.done:
		return nil, ErrStopped
	case reply := <-request.reply:
		return reply.cleared, nil
	}
}

func (o *Orchestrator) RequestProjectFailureBreakerCanary(ctx context.Context) (FailureBreakerCanaryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request := failureBreakerCanaryRequest{
		at:    o.clockNow(),
		reply: make(chan FailureBreakerCanaryResult, 1),
	}
	select {
	case <-ctx.Done():
		return FailureBreakerCanaryResult{}, ctx.Err()
	case <-o.done:
		return FailureBreakerCanaryResult{}, ErrStopped
	case o.failureCanaryRequests <- request:
	}
	select {
	case <-ctx.Done():
		return FailureBreakerCanaryResult{}, ctx.Err()
	case <-o.done:
		return FailureBreakerCanaryResult{}, ErrStopped
	case result := <-request.reply:
		return result, nil
	}
}

func resetTicker(ticker *time.Ticker, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker.Reset(interval)
}

func (o *Orchestrator) UpdateConfig(ctx context.Context, cfg Config) error {
	return o.UpdateRuntime(ctx, RuntimeUpdate{Config: cfg})
}

func (o *Orchestrator) UpdateRuntime(ctx context.Context, update RuntimeUpdate) error {
	if ctx == nil {
		ctx = context.Background()
	}

	request := configUpdateRequest{
		update: update,
		reply:  make(chan struct{}, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case o.configUpdates <- request:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case <-request.reply:
		return nil
	}
}

func (o *Orchestrator) State(ctx context.Context) (State, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	request := stateRequest{reply: make(chan State, 1)}
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	case <-o.done:
		return State{}, ErrStopped
	case o.stateRequests <- request:
	}

	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	case <-o.done:
		return State{}, ErrStopped
	case state := <-request.reply:
		pool := o.dispatchPoolSnapshot()
		state.PoolName = pool.Name
		state.PoolCapacity = pool.Capacity
		return state, nil
	}
}

func (o *Orchestrator) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	request := drainRequest{
		at:    time.Now().UTC(),
		reply: make(chan struct{}, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case o.drainRequests <- request:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case <-request.reply:
		return nil
	}
}

func (o *Orchestrator) ForceQuit(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	request := forceRequest{
		ctx:   ctx,
		at:    time.Now().UTC(),
		reply: make(chan error, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case o.forceRequests <- request:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrStopped
	case err := <-request.reply:
		return err
	}
}

func (o *Orchestrator) applyRuntimeUpdate(state *State, update RuntimeUpdate, ticker *time.Ticker) {
	cfg := normalizeConfig(update.Config)
	o.cfg = cfg
	now := time.Now
	if o.now != nil {
		now = o.now
	}
	o.reloadProjectFailureBreaker(state, cfg.FailureBreaker, now())
	if update.Connector != nil {
		o.connector = update.Connector
	}
	o.heartbeats.configure(cfg, o.connector, o.workAttempts)
	if update.ReplaceRelease {
		o.release = update.Release
		if update.Release == nil {
			state.Release = releasepkg.Status{}
		}
	}
	o.supervisor.UpdateConfig(runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
		OverloadRetryDelay:    cfg.OverloadRetryDelay,
	})
	state.PollInterval = cfg.PollInterval
	state.MaxConcurrentAgents = cfg.MaxConcurrentAgents
	state.AutoPromoteQuietDuration = cfg.AutoPromote.QuietDuration
	state.AutoPromote = cloneAutoPromoteConfig(cfg.AutoPromote)
	state.ActiveStates = append([]string(nil), cfg.ActiveStates...)
	state.TerminalStates = append([]string(nil), cfg.TerminalStates...)
	state.StopRunTargetState = cfg.StopRunTargetState
	state.PrioritizeUnblockers = cfg.PrioritizeUnblockers
	state.Instance = instanceSnapshot(cfg)
	state.Authorization = cloneSelector(cfg.Authorization)
	state.SelectorContext = cfg.SelectorContext
	if !state.LastRefreshAt.IsZero() && cfg.PollInterval > 0 {
		state.NextRefreshAt = state.LastRefreshAt.Add(cfg.PollInterval)
	}
	ticker.Reset(cfg.PollInterval)
}

func (o *Orchestrator) startDrain(state *State, now time.Time) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !state.Draining {
		state.Draining = true
		state.DrainStartedAt = now
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "shutdown_drain_started",
			Message: "shutdown drain started",
		})
	}
	o.markGlobalProjectIdle()
}

func (o *Orchestrator) forceQuit(ctx context.Context, state *State, now time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if state == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	state.Draining = true
	if state.DrainStartedAt.IsZero() {
		state.DrainStartedAt = now
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "shutdown_force_requested",
		Message: "shutdown force requested",
	})

	var err error
	for _, issueID := range sortedKeys(state.Running) {
		o.cancelRunning(state, issueID)
		o.heartbeats.remove(issueID)
		err = errors.Join(err, o.abandonClaim(ctx, issueID))
		delete(state.Running, issueID)
		delete(state.Claimed, issueID)
		delete(state.Retry, issueID)
		delete(state.BudgetRefusals, issueID)
		delete(state.PriorAttempts, issueID)
	}
	o.markGlobalProjectIdle()
	return err
}

func (o *Orchestrator) abandonClaim(ctx context.Context, issueID string) error {
	if !o.cfg.Claiming.Enabled || strings.TrimSpace(o.cfg.Claiming.LeaseField) == "" {
		return nil
	}
	if strings.TrimSpace(issueID) == "" || o.connector == nil {
		return nil
	}
	if err := o.connector.SetField(ctx, issueID, o.cfg.Claiming.LeaseField, ""); err != nil {
		if o.logger != nil {
			o.logger.Warn("abandon claim lease failed", "issue_id", issueID, "error", err)
		}
		return err
	}
	return nil
}

func (o *Orchestrator) cleanupDrainedRun(ctx context.Context, state *State, issueID string) {
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon completed drain claim failed", "issue_id", issueID, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	delete(state.InstantFailures, issueID)
	delete(state.RepeatedFailures, issueID)
}
