package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/notes"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	defaultAfterRunTimeout = time.Minute
	liveDiffStatsInterval  = 2 * time.Second
	recentActivityLimit    = 5
	defaultProjectID       = "default"
)

var (
	ErrMissingWorkspace    = errors.New("runner workspace backend is required")
	ErrMissingAgentBackend = errors.New("runner agent backend is required")
)

type SessionStore interface {
	StartSession(context.Context, store.SessionStart) (int64, error)
	FinishSession(context.Context, int64, store.SessionFinish) error
	RecordUsageEvent(context.Context, store.UsageEvent) (int64, error)
}

type BudgetChecker interface {
	CheckDispatch(context.Context, budget.DispatchRequest) (budget.Decision, error)
}

type DispatchEstimator interface {
	EstimateDispatch(context.Context, string) (budget.TokenEstimate, error)
}

type BudgetGuardBuilder func(config.Budget) (BudgetChecker, DispatchEstimator, error)

type workflowPhaseStore interface {
	RecordWorkflowPhaseEvent(context.Context, store.WorkflowPhaseEvent) (int64, error)
}

type AgentBackendFactory interface {
	NewAgentBackend(config.AgentBackend) (AgentBackend, error)
}

type AgentBackendFactoryFunc func(config.AgentBackend) (AgentBackend, error)

func (f AgentBackendFactoryFunc) NewAgentBackend(cfg config.AgentBackend) (AgentBackend, error) {
	return f(cfg)
}

type Dependencies struct {
	ProjectID           string
	Workflow            config.Workflow
	Workspace           workspace.Backend
	AgentBackend        AgentBackend
	AgentBackends       map[string]AgentBackend
	AgentBackendFactory AgentBackendFactory
	Store               SessionStore
	Pricing             budget.PricingTable
	BudgetChecker       BudgetChecker
	DispatchEstimator   DispatchEstimator
	BudgetGuardBuilder  BudgetGuardBuilder
	Now                 func() time.Time
	Logger              *slog.Logger
	AfterRunTimeout     time.Duration
}

type Runner struct {
	mu                  sync.RWMutex
	projectID           string
	workflow            config.Workflow
	workspace           workspace.Backend
	agentRuntime        agentRuntime
	agentBackendFactory AgentBackendFactory
	store               SessionStore
	pricing             budget.PricingTable
	budgetChecker       BudgetChecker
	dispatchEstimator   DispatchEstimator
	budgetGuardBuilder  BudgetGuardBuilder
	now                 func() time.Time
	logger              *slog.Logger
	afterRunTimeout     time.Duration
}

func NewRunner(deps Dependencies) (*Runner, error) {
	if deps.Workspace == nil {
		return nil, ErrMissingWorkspace
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.AfterRunTimeout <= 0 {
		deps.AfterRunTimeout = defaultAfterRunTimeout
	}
	projectID := strings.TrimSpace(deps.ProjectID)
	if projectID == "" {
		projectID = defaultProjectID
	}
	agentBackends := cloneAgentBackends(deps.AgentBackends)
	if deps.AgentBackend != nil {
		if agentBackends == nil {
			agentBackends = map[string]AgentBackend{}
		}
		agentBackends[config.DefaultAgentBackendID] = deps.AgentBackend
	}
	runtime, err := newAgentRuntime(deps.Workflow, agentBackends, deps.AgentBackendFactory)
	if err != nil {
		return nil, err
	}

	budgetChecker := deps.BudgetChecker
	dispatchEstimator := deps.DispatchEstimator
	if deps.BudgetGuardBuilder != nil {
		budgetChecker, dispatchEstimator, err = deps.BudgetGuardBuilder(deps.Workflow.Config.Budget)
		if err != nil {
			return nil, err
		}
	}

	return &Runner{
		projectID:           projectID,
		workflow:            deps.Workflow,
		workspace:           deps.Workspace,
		agentRuntime:        runtime,
		agentBackendFactory: deps.AgentBackendFactory,
		store:               deps.Store,
		pricing:             deps.Pricing,
		budgetChecker:       budgetChecker,
		dispatchEstimator:   dispatchEstimator,
		budgetGuardBuilder:  deps.BudgetGuardBuilder,
		now:                 deps.Now,
		logger:              deps.Logger,
		afterRunTimeout:     deps.AfterRunTimeout,
	}, nil
}

func (r *Runner) UpdateWorkflow(workflow config.Workflow) {
	r.mu.RLock()
	currentBackends := cloneAgentBackends(r.agentRuntime.backends)
	factory := r.agentBackendFactory
	budgetGuardBuilder := r.budgetGuardBuilder
	currentBudgetChecker := r.budgetChecker
	currentDispatchEstimator := r.dispatchEstimator
	r.mu.RUnlock()

	runtime, err := newAgentRuntime(workflow, currentBackends, factory)
	budgetChecker := currentBudgetChecker
	dispatchEstimator := currentDispatchEstimator
	var budgetErr error
	if budgetGuardBuilder != nil {
		budgetChecker, dispatchEstimator, budgetErr = budgetGuardBuilder(workflow.Config.Budget)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.workflow = workflow
	if budgetErr != nil {
		r.logger.Warn("reload budget dispatch guards failed", "error", budgetErr)
	} else {
		r.budgetChecker = budgetChecker
		r.dispatchEstimator = dispatchEstimator
	}
	if err != nil {
		r.logger.Warn("reload agent runtime failed", "error", err)
		return
	}
	r.agentRuntime = runtime
}

type agentRuntime struct {
	backends     map[string]AgentBackend
	backendKinds map[string]string
	router       *Router
}

func newAgentRuntime(
	workflow config.Workflow,
	staticBackends map[string]AgentBackend,
	factory AgentBackendFactory,
) (agentRuntime, error) {
	backendConfigs := workflow.Config.AgentBackendConfigs()
	backends := make(map[string]AgentBackend, len(backendConfigs))
	backendKinds := make(map[string]string, len(backendConfigs))
	for _, backendConfig := range backendConfigs {
		if strings.TrimSpace(backendConfig.ID) == "" {
			continue
		}
		backendKinds[backendConfig.ID] = backendConfig.Kind
		if factory != nil {
			backend, err := factory.NewAgentBackend(backendConfig)
			if err != nil {
				return agentRuntime{}, fmt.Errorf("create agent backend %s: %w", backendConfig.ID, err)
			}
			backends[backendConfig.ID] = backend
			continue
		}
		backend, ok := staticBackends[backendConfig.ID]
		if !ok {
			return agentRuntime{}, fmt.Errorf("%w: %s", ErrMissingAgentBackend, backendConfig.ID)
		}
		backends[backendConfig.ID] = backend
	}
	if len(backends) == 0 {
		return agentRuntime{}, ErrMissingAgentBackend
	}

	router, err := NewRouter(routesFromConfig(workflow.Config.AgentRouteConfigs()))
	if err != nil {
		return agentRuntime{}, err
	}
	for _, route := range router.routes {
		if _, ok := backends[route.BackendID]; !ok {
			return agentRuntime{}, fmt.Errorf("%w: %s", ErrMissingAgentBackend, route.BackendID)
		}
	}

	return agentRuntime{
		backends:     backends,
		backendKinds: backendKinds,
		router:       router,
	}, nil
}

func routesFromConfig(routes []config.AgentRoute) []Route {
	out := make([]Route, 0, len(routes))
	for _, route := range routes {
		out = append(out, Route{
			Name:       route.Name,
			Role:       route.Role,
			BackendID:  route.Backend,
			Model:      route.Model,
			ModelField: route.ModelField,
			Default:    route.Default,
			Selector:   route.Selector,
		})
	}
	return out
}

func (r agentRuntime) selectBackendForRole(issue connector.Issue, ctx selector.Context, role string) (RouteSelection, AgentBackend, string, error) {
	selection, err := r.router.RouteForRole(issue, ctx, role)
	if err != nil {
		return RouteSelection{}, nil, "", err
	}
	backend, ok := r.backends[selection.BackendID]
	if !ok {
		return RouteSelection{}, nil, "", fmt.Errorf("%w: %s", ErrMissingAgentBackend, selection.BackendID)
	}
	backendKind, ok := r.backendKinds[selection.BackendID]
	if !ok {
		return RouteSelection{}, nil, "", fmt.Errorf("agent backend kind not found: %s", selection.BackendID)
	}
	return selection, backend, backendKind, nil
}

func (r agentRuntime) defaultModelForRole(role string) string {
	if r.router == nil {
		return ""
	}
	role = normalizeRole(role)
	index, ok := r.router.defaultIndexes[role]
	if !ok && role != RoleCode {
		index, ok = r.router.defaultIndexes[RoleCode]
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(r.router.routes[index].Model)
}

func (r agentRuntime) effectiveRunRole(role string) string {
	role = normalizeRole(role)
	if role == RoleCode || r.router.HasRole(role) {
		return role
	}
	return RoleCode
}

func cloneAgentBackends(in map[string]AgentBackend) map[string]AgentBackend {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]AgentBackend, len(in))
	maps.Copy(out, in)
	return out
}

func selectorContext(ctx selector.Context, workflow config.Workflow) selector.Context {
	if strings.TrimSpace(ctx.Persona) == "" {
		ctx.Persona = workflow.Config.Tracker.Assignee
	}
	return ctx
}

func normalizeRunMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", RunModeImplement:
		return RunModeImplement
	case RunModePlan:
		return RunModePlan
	case RunModeMerge:
		return RunModeMerge
	default:
		return RunModeImplement
	}
}

func runRole(mode string, issue connector.Issue) string {
	if normalizeRunMode(mode) == RunModePlan {
		return RolePlan
	}
	switch strings.ToLower(strings.TrimSpace(issue.State)) {
	case RoleRework:
		return RoleRework
	case RoleMerge, "merging":
		return RoleMerge
	default:
		return RoleCode
	}
}

func extraWritableRootsForWorkspace(ctx context.Context, workspacePath string, logger *slog.Logger) []string {
	roots, err := workspace.GitMetadataWritableRoots(ctx, workspacePath)
	if err != nil {
		if logger != nil {
			logger.Warn("workspace git metadata writable roots unavailable", slog.String("workspace_path", workspacePath), slog.Any("error", err))
		}
		return nil
	}
	return roots
}

func (r *Runner) prepareMergeFastPath(
	ctx context.Context,
	req RunRequest,
	info workspace.Info,
	issue workspace.Issue,
) (RunResult, workspace.MergePrepareResult, bool, error) {
	preparer, ok := r.workspace.(workspace.MergePreparer)
	if !ok {
		return RunResult{}, workspace.MergePrepareResult{}, false, nil
	}
	precheck, err := preparer.PrepareMerge(ctx, info, issue, workspace.MergePrepareOptions{})
	if err != nil {
		return RunResult{}, precheck, false, fmt.Errorf("merge fast-path precheck: %w", err)
	}
	switch precheck.Status {
	case workspace.MergePrepareStatusClean:
		r.logWorkerEvent(req.Issue, "worker_merge_fast_path_clean",
			"workspace_path", info.Path,
			"workspace_branch", info.Branch,
		)
		return RunResult{
			FinalState: FinalStateCompleted,
			Output:     RunOutputMergeFastPathClean,
			DiffStats:  diffStatsFromWorkspace(precheck.DiffStat),
		}, precheck, true, nil
	case workspace.MergePrepareStatusConflict, workspace.MergePrepareStatusDirty:
		r.logWorkerEvent(req.Issue, "worker_merge_fast_path_fallback",
			"workspace_path", info.Path,
			"workspace_branch", info.Branch,
			"status", string(precheck.Status),
		)
		return RunResult{}, precheck, false, nil
	default:
		return RunResult{}, precheck, false, fmt.Errorf("merge fast-path precheck returned unknown status %q", precheck.Status)
	}
}

type agentTurnExecution struct {
	turnResult  AgentTurnResult
	result      RunResult
	err         error
	cleanupErr  error
	turnStarted bool
}

func (r *Runner) runAgentTurn(
	ctx context.Context,
	backend AgentBackend,
	turnRequest AgentTurnRequest,
	runRequest RunRequest,
	info workspace.Info,
	workspaceIssue workspace.Issue,
	agentConfig config.Agent,
	runStartedAt time.Time,
) agentTurnExecution {
	result := RunResult{FinalState: FinalStateCompleted}
	progress := newAgentRunProgress()
	turnStarted := false
	turnResult, turnErr := backend.RunTurn(ctx, turnRequest, func(update AgentUpdate) error {
		if update.Type == AgentUpdateTurnStarted || strings.TrimSpace(update.TurnID) != "" {
			turnStarted = true
		}
		r.logAgentUpdate(runRequest.Issue, update)
		applyAgentUpdate(&result, update)
		eventAt := r.now()
		progress.apply(update, eventAt)
		if err := r.publishRunUpdate(ctx, runRequest, info, workspaceIssue, progress, result, eventAt, runStartedAt); err != nil {
			return err
		}
		if err := r.enforceSessionTokenCeiling(agentConfig, runRequest.Issue, info.Path, update, eventAt); err != nil {
			return err
		}
		return nil
	})
	result.Output = progress.outputText()
	cleanupErr := agentTurnCleanupError(turnErr, turnResult)
	if cleanupErr != nil {
		turnErr = nil
	}
	if turnErr != nil {
		result.FinalState = finalStateForTurnError(turnErr)
	}
	return agentTurnExecution{
		turnResult:  turnResult,
		result:      result,
		err:         turnErr,
		cleanupErr:  cleanupErr,
		turnStarted: turnStarted,
	}
}

func (r *Runner) agentResumeState(
	ctx context.Context,
	cfg config.Agent,
	issue connector.Issue,
	model string,
	backendID string,
	backendKind string,
	agentRole string,
) store.AgentResumeState {
	if !cfg.ExperimentalThreadResume {
		return store.AgentResumeState{}
	}
	model = strings.TrimSpace(model)
	backendID = strings.TrimSpace(backendID)
	backendKind = strings.TrimSpace(backendKind)
	agentRole = strings.TrimSpace(agentRole)
	if model == "" || backendID == "" || backendKind == "" || agentRole == "" {
		return store.AgentResumeState{}
	}
	resumeStore, ok := r.store.(store.AgentResumeStore)
	if !ok {
		return store.AgentResumeState{}
	}
	state, err := resumeStore.LatestCompletedAgentResumeState(ctx, store.AgentResumeLookup{
		IssueID:          issue.ID,
		Identifier:       issue.Identifier,
		IssueURL:         issue.URL,
		RequestedModel:   model,
		AgentBackendID:   backendID,
		AgentBackendKind: backendKind,
		AgentRole:        agentRole,
	})
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.logger.Warn(
				"agent resume state lookup failed",
				slog.String("issue_id", issue.ID),
				slog.String("issue_identifier", issue.Identifier),
				slog.String("model", model),
				slog.String("backend_id", backendID),
				slog.String("backend_kind", backendKind),
				slog.String("agent_role", agentRole),
				slog.Any("error", err),
			)
		}
		return store.AgentResumeState{}
	}
	return state
}

func agentResumeFromState(state store.AgentResumeState) AgentResume {
	return AgentResume{
		ThreadID:  strings.TrimSpace(state.ProviderThreadID),
		SessionID: strings.TrimSpace(state.ProviderSessionID),
	}
}

func agentResumeEmpty(resume AgentResume) bool {
	return strings.TrimSpace(resume.ThreadID) == "" && strings.TrimSpace(resume.SessionID) == ""
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, agentRuntime, budgetChecker, dispatchEstimator := r.runtimeSnapshot()

	workspaceIssue := workspaceIssue(r.projectID, req.Issue)
	r.logWorkerEvent(req.Issue, "worker_workspace_create_started")
	info, err := r.workspace.Create(ctx, workspaceIssue)
	if err != nil {
		return RunResult{}, fmt.Errorf("create workspace: %w", err)
	}
	r.logWorkerEvent(req.Issue, "worker_workspace_created",
		"workspace_path", info.Path,
		"workspace_branch", info.Branch,
	)

	if err := r.workspace.BeforeRun(ctx, info, workspaceIssue); err != nil {
		return RunResult{}, fmt.Errorf("workspace before_run: %w", err)
	}
	r.logWorkerEvent(req.Issue, "worker_before_run_finished",
		"workspace_path", info.Path,
	)

	afterRunPending := true
	defer func() {
		if afterRunPending {
			r.afterRun(info, workspaceIssue)
		}
	}()

	mode := normalizeRunMode(req.Mode)
	mergePrecheck := workspace.MergePrepareResult{}
	mergeFallback := false
	if mode == RunModeMerge {
		precheckResult, precheck, handled, err := r.prepareMergeFastPath(ctx, req, info, workspaceIssue)
		if err != nil {
			return RunResult{}, err
		}
		mergePrecheck = precheck
		if handled {
			r.afterRun(info, workspaceIssue)
			afterRunPending = false
			r.logWorkerEvent(req.Issue, "worker_after_run_finished",
				"workspace_path", info.Path,
			)
			return precheckResult, nil
		}
		mergeFallback = true
	}

	attempt := req.Attempt
	availableSkills, err := r.availableSkills(workflow, info.Path)
	if err != nil {
		return RunResult{}, err
	}
	prompt, err := BuildPrompt(workflow, req.Issue, PromptOptions{
		Attempt:              &attempt,
		PlanOnly:             mode == RunModePlan,
		MergeFallback:        mergeFallback,
		MergePrecheckStatus:  string(mergePrecheck.Status),
		MergePrecheckMessage: mergePrecheck.Message,
		WorkspacePath:        info.Path,
		Branch:               info.Branch,
		AvailableSkills:      availableSkills,
		PriorAttempt:         req.PriorAttempt,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("build prompt: %w", err)
	}
	role := runRole(req.Mode, req.Issue)
	routeRole := agentRuntime.effectiveRunRole(role)
	selection, backend, backendKind, err := agentRuntime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), routeRole)
	if err != nil {
		return RunResult{}, err
	}

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	runStartedAt := r.now()
	selectedModel := selection.Model
	sessionModel := effectiveModel("", selectedModel, agentRuntime.defaultModelForRole(role))
	if result, refused, err := r.checkDispatchBudget(ctx, budgetChecker, dispatchEstimator, req.Issue, sessionModel, startedAt); err != nil {
		return RunResult{}, err
	} else if refused {
		r.logWorkerEvent(req.Issue, "worker_budget_refused",
			"workspace_path", info.Path,
			"backend_id", selection.BackendID,
			"route", selection.RouteName,
			"role", role,
			"model", sessionModel,
			"code", result.BudgetRefusal.Code,
		)
		return result, nil
	}
	resumeState := r.agentResumeState(ctx, workflow.Config.Agent, req.Issue, sessionModel, selection.BackendID, backendKind, role)
	sessionID, sessionStarted, err := r.startSession(ctx, req.Issue, startedAt, sessionModel, selection.BackendID, backendKind, role)
	if err != nil {
		return RunResult{}, err
	}

	r.logWorkerEvent(req.Issue, "worker_command_started",
		"workspace_path", info.Path,
		"backend_id", selection.BackendID,
		"route", selection.RouteName,
		"role", role,
		"model", sessionModel,
		"mode", mode,
	)
	turnRequest := AgentTurnRequest{
		Workspace:          info.Path,
		Prompt:             prompt,
		Model:              selectedModel,
		Resume:             agentResumeFromState(resumeState),
		ExtraWritableRoots: extraWritableRootsForWorkspace(ctx, info.Path, r.logger),
	}
	execution := r.runAgentTurn(ctx, backend, turnRequest, req, info, workspaceIssue, workflow.Config.Agent, runStartedAt)
	if execution.err != nil && !agentResumeEmpty(turnRequest.Resume) && !execution.turnStarted {
		r.logWorkerEvent(req.Issue, "worker_resume_failed_fallback",
			"workspace_path", info.Path,
			"backend_id", selection.BackendID,
			"route", selection.RouteName,
			"role", role,
			"thread_id", turnRequest.Resume.ThreadID,
			"provider_session_id", turnRequest.Resume.SessionID,
			"error", errorString(execution.err),
		)
		turnRequest.Resume = AgentResume{}
		resumeState = store.AgentResumeState{}
		execution = r.runAgentTurn(ctx, backend, turnRequest, req, info, workspaceIssue, workflow.Config.Agent, runStartedAt)
	}
	turnResult := execution.turnResult
	turnErr := execution.err
	cleanupErr := execution.cleanupErr
	result := execution.result
	commandFinishedAttrs := []any{
		"workspace_path", info.Path,
		"backend_id", selection.BackendID,
		"route", selection.RouteName,
		"role", role,
		"model", effectiveModel(result.Model, sessionModel),
		"outcome", workerRunOutcome(turnErr, result.FinalState),
		"error", errorString(turnErr),
	}
	commandFinishedAttrs = append(commandFinishedAttrs, backendErrorAttrs(turnErr)...)
	if cleanupErr != nil {
		commandFinishedAttrs = append(commandFinishedAttrs,
			"cleanup_error", errorString(cleanupErr),
			"cleanup_class", "agent_turn_cleanup",
		)
	}
	if turnErr != nil || cleanupErr != nil {
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_command_finished", commandFinishedAttrs...)
	} else {
		r.logWorkerEvent(req.Issue, "worker_command_finished", commandFinishedAttrs...)
	}
	failureNoteRecorded := false
	if strings.EqualFold(strings.TrimSpace(result.FinalState), FinalStateFailed) {
		r.recordFailedRunNote(info.Path, req.Issue, result, turnErr, r.now().UTC())
		failureNoteRecorded = true
	}

	r.afterRun(info, workspaceIssue)
	afterRunPending = false
	r.logWorkerEvent(req.Issue, "worker_after_run_finished",
		"workspace_path", info.Path,
	)

	if turnErr != nil {
		finishedAt := r.now().UTC()
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
		return result, errors.Join(
			fmt.Errorf("run agent turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, sessionModel, backendKind, 1, turnResult, resumeState.DetentSessionID),
		)
	}

	diffStat, err := r.workspace.DiffStat(ctx, info, workspaceIssue)
	if err != nil {
		if workspace.IsMissingWorkspaceError(err) {
			r.logger.Info(
				"workspace final diff stat skipped",
				slog.String("issue_id", workspaceIssue.ID),
				slog.String("issue_identifier", workspaceIssue.Identifier),
				slog.String("workspace_path", info.Path),
				slog.String("phase", "final"),
				slog.String("error", err.Error()),
			)
			finishedAt := r.now().UTC()
			result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
			if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, sessionModel, backendKind, 1, turnResult, resumeState.DetentSessionID); err != nil {
				return result, err
			}
			return result, nil
		}
		result.FinalState = FinalStateFailed
		if !failureNoteRecorded {
			r.recordFailedRunNote(info.Path, req.Issue, result, err, r.now().UTC())
		}
		finishedAt := r.now().UTC()
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
		return result, errors.Join(
			fmt.Errorf("workspace diff stat: %w", err),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, sessionModel, backendKind, 1, turnResult, resumeState.DetentSessionID),
		)
	}

	result.DiffStats = diffStatsFromWorkspace(diffStat)
	finishedAt := r.now().UTC()
	result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
	if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, sessionModel, backendKind, 1, turnResult, resumeState.DetentSessionID); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Runner) checkDispatchBudget(ctx context.Context, checker BudgetChecker, estimator DispatchEstimator, issue connector.Issue, model string, now time.Time) (RunResult, bool, error) {
	if checker == nil {
		return RunResult{}, false, nil
	}

	estimate := budget.TokenEstimate{}
	if estimator != nil {
		var err error
		estimate, err = estimator.EstimateDispatch(ctx, model)
		if err != nil {
			return RunResult{}, false, fmt.Errorf("estimate dispatch budget: %w", err)
		}
	}
	decision, err := checker.CheckDispatch(ctx, budget.DispatchRequest{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
		Model:      model,
		Now:        now,
		Estimate:   estimate,
	})
	if err != nil {
		return RunResult{}, false, fmt.Errorf("check dispatch budget: %w", err)
	}
	if decision.Allowed || decision.Refusal == nil {
		return RunResult{}, false, nil
	}
	return RunResult{
		FinalState:    FinalStateCompleted,
		BudgetRefusal: budgetRefusalFromDecision(issue, *decision.Refusal),
	}, true, nil
}

func budgetRefusalFromDecision(issue connector.Issue, refusal budget.Refusal) *BudgetRefusal {
	var maxUSD *float64
	if refusal.MaxUSD != nil {
		value := *refusal.MaxUSD
		maxUSD = &value
	}
	var resetAt *time.Time
	if refusal.ResetAt != nil {
		value := *refusal.ResetAt
		resetAt = &value
	}
	return &BudgetRefusal{
		Issue:            issue,
		Code:             string(refusal.Code),
		Message:          refusal.Message,
		Comment:          refusal.Comment(),
		CurrentSpendUSD:  refusal.CurrentSpendUSD,
		ProjectedCostUSD: refusal.ProjectedCostUSD,
		MaxUSD:           maxUSD,
		ResetAt:          resetAt,
		RefusedAt:        refusal.RefusedAt,
	}
}

func (r *Runner) Validate(ctx context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, agentRuntime, _, _ := r.runtimeSnapshot()

	workspaceIssue := workspaceIssue(r.projectID, req.Issue)
	r.logWorkerEvent(req.Issue, "worker_check_workspace_create_started")
	info, err := r.workspace.Create(ctx, workspaceIssue)
	if err != nil {
		return gate.ValidatorResult{}, fmt.Errorf("create workspace: %w", err)
	}
	r.logWorkerEvent(req.Issue, "worker_check_workspace_created",
		"workspace_path", info.Path,
		"workspace_branch", info.Branch,
	)

	if err := r.workspace.BeforeRun(ctx, info, workspaceIssue); err != nil {
		return gate.ValidatorResult{}, fmt.Errorf("workspace before_run: %w", err)
	}
	r.logWorkerEvent(req.Issue, "worker_check_before_run_finished",
		"workspace_path", info.Path,
	)

	afterRunPending := true
	defer func() {
		if afterRunPending {
			r.afterRun(info, workspaceIssue)
		}
	}()

	validator := gate.Effective(workflow.Config.Gate).Validator
	promptOptions := r.validatorPromptOptions(ctx, info, workspaceIssue, validatorMaxInlineDiffBytes(validator))
	prompt := BuildValidatorPrompt(workflow, req.Issue, promptOptions)
	selection, backend, backendKind, err := agentRuntime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), RoleValidator)
	if err != nil {
		return gate.ValidatorResult{}, err
	}

	selectedModel := selection.Model
	if override := strings.TrimSpace(validator.Model); override != "" {
		selectedModel = override
	}
	sessionModel := effectiveModel("", selectedModel, agentRuntime.defaultModelForRole(RoleValidator))

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	runStartedAt := r.now()
	sessionID, sessionStarted, err := r.startSession(ctx, req.Issue, startedAt, sessionModel, selection.BackendID, backendKind, RoleValidator)
	if err != nil {
		return gate.ValidatorResult{}, err
	}

	r.logWorkerEvent(req.Issue, "worker_check_started",
		"workspace_path", info.Path,
		"backend_id", selection.BackendID,
		"route", selection.RouteName,
		"role", RoleValidator,
		"model", sessionModel,
	)
	runReq := RunRequest{
		Issue:           req.Issue,
		StartedAt:       req.StartedAt,
		SelectorContext: req.SelectorContext,
		OnUsageUpdate:   req.OnUsageUpdate,
	}
	runResult := RunResult{FinalState: FinalStateCompleted}
	progress := newAgentRunProgress()
	var output strings.Builder
	turnResult, turnErr := backend.RunTurn(ctx, AgentTurnRequest{
		Workspace:          info.Path,
		Prompt:             prompt,
		Model:              selectedModel,
		TurnTimeout:        durationFromMillis(validator.TurnTimeoutMS),
		ExtraWritableRoots: extraWritableRootsForWorkspace(ctx, info.Path, r.logger),
	}, func(update AgentUpdate) error {
		r.logAgentUpdate(req.Issue, update)
		if update.Type == AgentUpdateMessageDelta {
			output.WriteString(update.Delta)
		}
		applyAgentUpdate(&runResult, update)
		eventAt := r.now()
		progress.apply(update, eventAt)
		if err := r.publishRunUpdate(ctx, runReq, info, workspaceIssue, progress, runResult, eventAt, runStartedAt); err != nil {
			return err
		}
		if err := r.enforceSessionTokenCeiling(workflow.Config.Agent, req.Issue, info.Path, update, eventAt); err != nil {
			return err
		}
		return nil
	})
	checkFinishedAttrs := []any{
		"workspace_path", info.Path,
		"backend_id", selection.BackendID,
		"route", selection.RouteName,
		"role", RoleValidator,
		"model", effectiveModel(runResult.Model, sessionModel),
		"outcome", workerRunOutcome(turnErr, runResult.FinalState),
		"error", errorString(turnErr),
	}
	checkFinishedAttrs = append(checkFinishedAttrs, backendErrorAttrs(turnErr)...)
	if turnErr != nil {
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_check_finished", checkFinishedAttrs...)
	} else {
		r.logWorkerEvent(req.Issue, "worker_check_finished", checkFinishedAttrs...)
	}

	r.afterRun(info, workspaceIssue)
	afterRunPending = false
	r.logWorkerEvent(req.Issue, "worker_check_after_run_finished",
		"workspace_path", info.Path,
	)

	finishedAt := r.now().UTC()
	runResult.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
	if turnErr != nil {
		runResult.FinalState = finalStateForTurnError(turnErr)
		return gate.ValidatorResult{}, errors.Join(
			fmt.Errorf("run validator turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, sessionModel, backendKind, 1, turnResult, 0),
		)
	}

	validation, err := parseValidatorResult(output.String())
	if err != nil {
		runResult.FinalState = FinalStateFailed
		return gate.ValidatorResult{}, errors.Join(
			fmt.Errorf("parse validator result: %w", err),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, sessionModel, backendKind, 1, turnResult, 0),
		)
	}
	if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, sessionModel, backendKind, 1, turnResult, 0); err != nil {
		return gate.ValidatorResult{}, err
	}
	return validation, nil
}

func (r *Runner) validatorPromptOptions(ctx context.Context, info workspace.Info, issue workspace.Issue, maxInlineDiffBytes int) ValidatorPromptOptions {
	opts := ValidatorPromptOptions{
		WorkspacePath:      info.Path,
		Branch:             info.Branch,
		MaxInlineDiffBytes: maxInlineDiffBytes,
	}

	if provider, ok := r.workspace.(workspace.DiffProvider); ok {
		diff, err := provider.Diff(ctx, info, issue, maxInlineDiffBytes)
		if err == nil {
			opts.DiffStat = &diff.Stat
			opts.DiffPatch = diff.Patch
			opts.DiffTruncated = diff.Truncated
			return opts
		}
		opts.DiffError = err.Error()
		r.logValidatorDiffError(issue, info, "workspace diff failed", err)
	}

	stat, err := r.workspace.DiffStat(ctx, info, issue)
	if err != nil {
		if opts.DiffError == "" {
			opts.DiffError = err.Error()
		}
		r.logValidatorDiffError(issue, info, "workspace diff stat failed", err)
		return opts
	}
	opts.DiffStat = &stat
	return opts
}

func validatorMaxInlineDiffBytes(cfg gate.ValidatorConfig) int {
	if cfg.MaxInlineDiffBytes == nil {
		return gate.DefaultValidatorMaxInlineDiffBytes
	}
	return *cfg.MaxInlineDiffBytes
}

func (r *Runner) logValidatorDiffError(issue workspace.Issue, info workspace.Info, message string, err error) {
	if r == nil || r.logger == nil || err == nil {
		return
	}
	r.logger.Warn(
		message,
		slog.String("issue_id", issue.ID),
		slog.String("issue_identifier", issue.Identifier),
		slog.String("workspace_path", info.Path),
		slog.String("phase", "validator"),
		slog.String("error", err.Error()),
	)
}

type validatorJSONResult struct {
	Verdict    string                 `json:"verdict"`
	Score      float64                `json:"score"`
	Confidence float64                `json:"confidence"`
	TrustScore float64                `json:"trust_score"`
	Summary    string                 `json:"summary"`
	Findings   []validatorJSONFinding `json:"findings"`
}

type validatorJSONFinding struct {
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Message  string `json:"message"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
}

func parseValidatorResult(output string) (gate.ValidatorResult, error) {
	payload, err := validatorJSONPayload(output)
	if err != nil {
		return gate.ValidatorResult{}, err
	}

	var decoded validatorJSONResult
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return gate.ValidatorResult{}, err
	}

	score := decoded.Score
	if score == 0 {
		switch {
		case decoded.TrustScore > 0:
			score = decoded.TrustScore
		case decoded.Confidence > 0:
			score = decoded.Confidence
		}
	}

	findings := make([]gate.Finding, 0, len(decoded.Findings))
	for _, finding := range decoded.Findings {
		body := strings.TrimSpace(finding.Body)
		if body == "" {
			body = strings.TrimSpace(finding.Message)
		}
		findings = append(findings, gate.Finding{
			Severity: strings.ToLower(strings.TrimSpace(finding.Severity)),
			Body:     body,
			Path:     strings.TrimSpace(finding.Path),
			Line:     finding.Line,
		})
	}

	return gate.ValidatorResult{
		Submitted: true,
		Verdict:   strings.TrimSpace(decoded.Verdict),
		Score:     score,
		Summary:   strings.TrimSpace(decoded.Summary),
		Findings:  findings,
	}, nil
}

func validatorJSONPayload(output string) (string, error) {
	output = strings.TrimSpace(output)
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end < start {
		return "", errors.New("validator output did not contain a JSON object")
	}
	return output[start : end+1], nil
}

func (r *Runner) ReapWorkspace(ctx context.Context, issue connector.Issue) (WorkspaceReapResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	workspaceIssue := workspaceIssue(r.projectID, issue)
	if cleaner, ok := r.workspace.(workspace.IssueCleaner); ok {
		result, err := cleaner.CleanupIssue(ctx, workspaceIssue)
		return WorkspaceReapResult{
			Worktrees: result.Worktrees,
			Branches:  result.Branches,
			Processes: result.Processes,
		}, err
	}
	if err := r.workspace.Cleanup(ctx, issue.Identifier); err != nil {
		return WorkspaceReapResult{}, err
	}
	return WorkspaceReapResult{}, nil
}

func (r *Runner) runtimeSnapshot() (config.Workflow, agentRuntime, BudgetChecker, DispatchEstimator) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.workflow, r.agentRuntime, r.budgetChecker, r.dispatchEstimator
}

func (r *Runner) availableSkills(workflow config.Workflow, workspacePath string) ([]skills.Skill, error) {
	cfg := workflow.Config.Agent.Skills
	if !cfg.Enabled {
		return nil, nil
	}

	result, err := skills.Load(workspacePath, skills.Options{
		Path:              cfg.Path,
		MaxSkillsInPrompt: cfg.MaxSkillsInPrompt,
		Logger:            r.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	for _, validation := range result.Errors {
		r.logger.Warn(
			"invalid repo skill ignored",
			slog.String("path", validation.Path),
			slog.String("message", validation.Message),
		)
	}
	return result.Skills, nil
}

func (r *Runner) afterRun(info workspace.Info, issue workspace.Issue) {
	ctx, cancel := context.WithTimeout(context.Background(), r.afterRunTimeout)
	defer cancel()

	r.workspace.AfterRun(ctx, info, issue)
}

func (r *Runner) recordFailedRunNote(workspacePath string, issue connector.Issue, result RunResult, runErr error, at time.Time) {
	notesPath, err := notes.WorkspacePath(workspacePath)
	if err != nil {
		r.logger.Warn("resolve failed run note path failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		return
	}
	if err := notes.Append(notesPath, notes.Entry{
		Title: "Failed run output tail",
		Body:  failedRunNoteBody(result, runErr),
	}, notes.AppendOptions{Now: at, MaxBytes: notes.DefaultMaxBytes}); err != nil {
		r.logger.Warn("record failed run note failed", "issue_id", issue.ID, "identifier", issue.Identifier, "path", notesPath, "error", err)
	}
}

func failedRunNoteBody(result RunResult, runErr error) string {
	var b strings.Builder
	finalState := strings.TrimSpace(result.FinalState)
	if finalState == "" {
		finalState = FinalStateFailed
	}
	b.WriteString("- final_state: ")
	b.WriteString(finalState)
	if runErr != nil {
		b.WriteString("\n- error: ")
		b.WriteString(strings.TrimSpace(runErr.Error()))
	}
	output := strings.TrimSpace(notes.Tail(result.Output, notes.DefaultTailBytes))
	if output != "" {
		b.WriteString("\n\nOutput tail:\n\n```text\n")
		b.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```")
	}
	return b.String()
}

func (r *Runner) startSession(
	ctx context.Context,
	issue connector.Issue,
	startedAt time.Time,
	model string,
	backendID string,
	backendKind string,
	agentRole string,
) (int64, bool, error) {
	if r.store == nil {
		return 0, false, nil
	}

	sessionID, err := r.store.StartSession(ctx, store.SessionStart{
		IssueID:          issue.ID,
		Identifier:       issue.Identifier,
		IssueURL:         issue.URL,
		StartedAt:        startedAt,
		Model:            model,
		RequestedModel:   model,
		AgentBackendID:   backendID,
		AgentBackendKind: backendKind,
		AgentRole:        agentRole,
	})
	if err != nil {
		return 0, false, fmt.Errorf("start agent session: %w", err)
	}
	r.logWorkerEvent(issue, "worker_session_started",
		"session_id", sessionID,
		"model", model,
	)
	return sessionID, true, nil
}

func (r *Runner) finishSession(
	ctx context.Context,
	sessionID int64,
	started bool,
	issue connector.Issue,
	startedAt time.Time,
	finishedAt time.Time,
	result RunResult,
	model string,
	backendKind string,
	turns int64,
	turnResult AgentTurnResult,
	resumedFromSessionID int64,
) error {
	if !started {
		return nil
	}
	if result.FinalState == "" {
		result.FinalState = FinalStateCompleted
	}
	model = effectiveModel(result.Model, model)

	if err := r.store.FinishSession(ctx, sessionID, store.SessionFinish{
		CompletedAt:           finishedAt,
		Turns:                 turns,
		InputTokens:           result.Tokens.InputTokens,
		CachedInputTokens:     result.Tokens.CachedInputTokens,
		OutputTokens:          result.Tokens.OutputTokens,
		ReasoningOutputTokens: result.Tokens.ReasoningOutputTokens,
		TotalTokens:           result.Tokens.TotalTokens,
		ModelContextWindow:    result.Tokens.ModelContextWindow,
		RuntimeSeconds:        int64(math.Round(result.Tokens.RuntimeSeconds)),
		FinalState:            result.FinalState,
		Model:                 model,
		ProviderThreadID:      turnResult.ThreadID,
		ProviderSessionID:     turnResult.SessionID,
		ResumedFromSessionID:  resumedFromSessionID,
	}); err != nil {
		return fmt.Errorf("finish agent session: %w", err)
	}
	r.logWorkerEvent(issue, "worker_session_finished",
		"session_id", sessionID,
		"model", model,
		"final_state", result.FinalState,
		"turns", turns,
	)
	if _, err := r.store.RecordUsageEvent(ctx, store.UsageEvent{
		ProjectID:             r.projectID,
		SessionID:             sessionID,
		IssueID:               issue.ID,
		Identifier:            issue.Identifier,
		PRNumber:              pullRequestNumber(issue),
		Model:                 model,
		InputTokens:           result.Tokens.InputTokens,
		CachedInputTokens:     result.Tokens.CachedInputTokens,
		OutputTokens:          result.Tokens.OutputTokens,
		ReasoningOutputTokens: result.Tokens.ReasoningOutputTokens,
		TotalTokens:           result.Tokens.TotalTokens,
		ModelContextWindow:    result.Tokens.ModelContextWindow,
		CostUSD:               r.usageCostUSD(model, result.Tokens.InputTokens, result.Tokens.CachedInputTokens, result.Tokens.OutputTokens, backendKind),
		RuntimeSeconds:        int64(math.Round(result.Tokens.RuntimeSeconds)),
		StartedAt:             startedAt,
		FinishedAt:            finishedAt,
		Outcome:               result.FinalState,
	}); err != nil {
		return fmt.Errorf("record usage event: %w", err)
	}
	if err := r.recordAgentSessionPhase(ctx, sessionID, issue, startedAt, finishedAt, result, backendKind); err != nil {
		return err
	}
	return nil
}

func (r *Runner) recordAgentSessionPhase(
	ctx context.Context,
	sessionID int64,
	issue connector.Issue,
	startedAt time.Time,
	finishedAt time.Time,
	result RunResult,
	backendKind string,
) error {
	phaseStore, ok := r.store.(workflowPhaseStore)
	if !ok {
		return nil
	}
	if result.FinalState == "" {
		result.FinalState = FinalStateCompleted
	}
	endpointFamily := strings.TrimSpace(backendKind)
	if _, err := phaseStore.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:             r.projectID,
		SessionID:             sessionID,
		IssueID:               issue.ID,
		Identifier:            issue.Identifier,
		IssueURL:              issue.URL,
		PRNumber:              pullRequestNumber(issue),
		PhaseType:             store.WorkflowPhaseTypeAgentSession,
		PhaseName:             "agent_active",
		Status:                result.FinalState,
		StartedAt:             startedAt,
		FinishedAt:            finishedAt,
		DurationSeconds:       int64(math.Round(result.Tokens.RuntimeSeconds)),
		Turns:                 1,
		InputTokens:           result.Tokens.InputTokens,
		CachedInputTokens:     result.Tokens.CachedInputTokens,
		OutputTokens:          result.Tokens.OutputTokens,
		ReasoningOutputTokens: result.Tokens.ReasoningOutputTokens,
		TotalTokens:           result.Tokens.TotalTokens,
		ModelContextWindow:    result.Tokens.ModelContextWindow,
		EndpointFamily:        endpointFamily,
	}); err != nil {
		return fmt.Errorf("record agent session phase: %w", err)
	}
	return nil
}

func (r *Runner) usageCostUSD(model string, inputTokens int64, cachedInputTokens int64, outputTokens int64, backendKind string) float64 {
	model = strings.TrimSpace(model)
	if model == "" {
		r.logger.Debug("usage event model unavailable; skipping cost pricing", "backend_kind", strings.TrimSpace(backendKind))
		return 0
	}
	cost, ok := budget.UsageCostUSD(r.pricing, model, inputTokens, cachedInputTokens, outputTokens)
	if !ok {
		r.logger.Warn("usage event model pricing not found", "model", model, "backend_kind", strings.TrimSpace(backendKind))
		return 0
	}
	return cost
}

type sessionTokenCeiling struct {
	tokens             int64
	source             string
	modelContextWindow int64
	contextMultiplier  float64
}

func (r *Runner) enforceSessionTokenCeiling(cfg config.Agent, issue connector.Issue, workspacePath string, update AgentUpdate, eventAt time.Time) error {
	if update.Type != AgentUpdateTokenUsage || sessionTokenCeilingBypassed(cfg, issue) {
		return nil
	}
	ceiling, ok := sessionTokenCeilingForUsage(cfg, update.Tokens)
	if !ok || update.Tokens.TotalTokens <= ceiling.tokens {
		return nil
	}

	err := &SessionTokenCeilingError{
		TotalTokens:        update.Tokens.TotalTokens,
		CeilingTokens:      ceiling.tokens,
		Source:             ceiling.source,
		ModelContextWindow: ceiling.modelContextWindow,
		ContextMultiplier:  ceiling.contextMultiplier,
	}
	if appendErr := appendSessionTokenCeilingLesson(cfg.Lessons, issue, workspacePath, err, eventAt); appendErr != nil {
		r.logger.Warn("session token ceiling lesson append failed", "error", appendErr)
		return errors.Join(err, appendErr)
	}
	return err
}

func sessionTokenCeilingForUsage(cfg config.Agent, tokens AgentTokenUsage) (sessionTokenCeiling, bool) {
	var ceiling sessionTokenCeiling
	if cfg.MaxSessionTokens > 0 {
		ceiling = sessionTokenCeiling{
			tokens: cfg.MaxSessionTokens,
			source: TokenCeilingSourceAbsolute,
		}
	}
	if cfg.MaxSessionContextMultiplier > 0 && tokens.ModelContextWindow != nil && *tokens.ModelContextWindow > 0 {
		limit := int64(math.Ceil(float64(*tokens.ModelContextWindow) * cfg.MaxSessionContextMultiplier))
		if limit > 0 && (ceiling.tokens == 0 || limit < ceiling.tokens) {
			ceiling = sessionTokenCeiling{
				tokens:             limit,
				source:             TokenCeilingSourceContextWindow,
				modelContextWindow: *tokens.ModelContextWindow,
				contextMultiplier:  cfg.MaxSessionContextMultiplier,
			}
		}
	}
	return ceiling, ceiling.tokens > 0
}

func sessionTokenCeilingBypassed(cfg config.Agent, issue connector.Issue) bool {
	if label := strings.TrimSpace(cfg.MaxSessionTokenOverrideLabel); label != "" && issueHasLabel(issue, label) {
		return true
	}
	if field := strings.TrimSpace(cfg.MaxSessionTokenOverrideField); field != "" {
		value, ok := issueFieldValue(issue.Fields, field)
		return ok && tokenCeilingOverrideEnabled(value)
	}
	return false
}

func issueHasLabel(issue connector.Issue, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	for _, label := range issue.Labels {
		if strings.ToLower(strings.TrimSpace(label)) == want {
			return true
		}
	}
	return false
}

func tokenCeilingOverrideEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on", "allow", "allowed", "bypass", "disabled":
		return true
	default:
		return false
	}
}

func appendSessionTokenCeilingLesson(cfg config.Lessons, issue connector.Issue, workspacePath string, ceilingErr *SessionTokenCeilingError, eventAt time.Time) error {
	if strings.TrimSpace(workspacePath) == "" {
		return nil
	}
	path := cfg.Path
	if strings.TrimSpace(path) == "" {
		path = lessons.DefaultPath
	}
	lessonPath, err := promptWorkspaceRelativePath(workspacePath, path)
	if err != nil {
		return err
	}
	return lessons.Append(lessonPath, lessons.Entry{
		IssueNumber: githubIssueNumber(issue.Identifier),
		IssueRef:    issue.Identifier,
		Title:       issue.Title,
		FailureKind: FinalStateTokenCeilingExceeded,
		Symptom:     fmt.Sprintf("session reached %d tokens, above configured ceiling %d", ceilingErr.TotalTokens, ceilingErr.CeilingTokens),
		Hypothesis:  "the agent session is consuming tokens faster than the configured per-session ceiling permits",
		Hint:        "retry with a narrower task split, stronger stop conditions, or a deliberate per-issue token ceiling override",
	}, lessons.AppendOptions{Date: eventAt.UTC(), MaxEntries: cfg.MaxEntries})
}

func finalStateForTurnError(err error) string {
	if errors.Is(err, ErrSessionTokenCeilingExceeded) {
		return FinalStateTokenCeilingExceeded
	}
	return FinalStateFailed
}

func agentTurnCleanupError(err error, result AgentTurnResult) error {
	if err == nil || !errors.Is(err, ErrAgentTurnCleanup) {
		return nil
	}
	if strings.TrimSpace(result.ThreadID) == "" || strings.TrimSpace(result.TurnID) == "" {
		return nil
	}
	return err
}

func durationFromMillis(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func effectiveModel(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func workspaceIssue(projectID string, issue connector.Issue) workspace.Issue {
	baseRef := ""
	if issue.PullRequest != nil {
		baseRef = strings.TrimSpace(issue.PullRequest.BaseSHA)
	}
	return workspace.Issue{
		ProjectID:  projectID,
		ID:         issue.ID,
		Identifier: issue.Identifier,
		BranchName: issue.BranchName,
		BaseRef:    baseRef,
	}
}

func applyAgentUpdate(result *RunResult, update AgentUpdate) {
	if model := strings.TrimSpace(update.Model); model != "" {
		result.Model = model
	}
	switch update.Type {
	case AgentUpdateTokenUsage:
		result.Tokens.InputTokens = update.Tokens.InputTokens
		result.Tokens.CachedInputTokens = update.Tokens.CachedInputTokens
		result.Tokens.OutputTokens = update.Tokens.OutputTokens
		result.Tokens.ReasoningOutputTokens = update.Tokens.ReasoningOutputTokens
		result.Tokens.TotalTokens = update.Tokens.TotalTokens
		result.Tokens.ModelContextWindow = update.Tokens.ModelContextWindow
	case AgentUpdateRateLimits:
		result.RateLimits = update.RateLimits
	}
}

type agentRunProgress struct {
	sessionID          string
	processIdentity    string
	turnIDs            map[string]struct{}
	messages           map[string]string
	messageOrder       []string
	lastEventAt        time.Time
	lastEvent          string
	lastMessage        string
	recentEvents       []telemetry.ActivityEvent
	diffStats          DiffStats
	diffStatsCollected bool
	diffStatsCheckedAt time.Time
}

func newAgentRunProgress() *agentRunProgress {
	return &agentRunProgress{
		turnIDs:  map[string]struct{}{},
		messages: map[string]string{},
	}
}

func (p *agentRunProgress) apply(update AgentUpdate, eventAt time.Time) {
	if update.ProcessIdentity != "" {
		p.processIdentity = update.ProcessIdentity
	}
	if update.ThreadID != "" && update.TurnID != "" {
		p.sessionID = update.ThreadID + "-" + update.TurnID
		p.turnIDs[update.TurnID] = struct{}{}
	}
	if update.Type != "" {
		p.lastEvent = string(update.Type)
	} else {
		p.lastEvent = update.Method
	}
	p.lastEventAt = eventAt.UTC()

	eventMessage := ""
	switch update.Type {
	case AgentUpdateMessageDelta:
		key := update.ItemID
		if key == "" {
			key = update.TurnID
		}
		if _, ok := p.messages[key]; !ok {
			p.messageOrder = append(p.messageOrder, key)
		}
		p.messages[key] += update.Delta
		p.lastMessage = strings.TrimSpace(p.messages[key])
		eventMessage = p.lastMessage
	case AgentUpdateTurnStarted:
		p.lastMessage = "turn started"
		eventMessage = p.lastMessage
	case AgentUpdateTurnCompleted:
		status := update.Status
		if status == "" {
			status = "completed"
		}
		p.lastMessage = "turn " + status
		eventMessage = p.lastMessage
	case AgentUpdateTokenUsage:
		eventMessage = tokenUsageActivityMessage(update.Tokens)
	case AgentUpdateRateLimits:
		eventMessage = rateLimitsActivityMessage(update.RateLimits)
	case AgentUpdateProcessStarted:
		if p.processIdentity != "" {
			eventMessage = "process " + p.processIdentity + " started"
		} else {
			eventMessage = "process started"
		}
	}

	p.addRecentEvent(telemetry.ActivityEvent{
		At:      p.lastEventAt,
		Event:   p.lastEvent,
		Message: eventMessage,
	})
}

func tokenUsageActivityMessage(tokens AgentTokenUsage) string {
	if tokens.TotalTokens > 0 && (tokens.InputTokens > 0 || tokens.OutputTokens > 0) {
		return fmt.Sprintf("%d total tokens (%d in, %d out)", tokens.TotalTokens, tokens.InputTokens, tokens.OutputTokens)
	}
	if tokens.TotalTokens > 0 {
		return fmt.Sprintf("%d total tokens", tokens.TotalTokens)
	}
	return "tokens updated"
}

func rateLimitsActivityMessage(snapshot *telemetry.RateLimits) string {
	if snapshot == nil {
		return "rate limits updated"
	}
	name := strings.TrimSpace(snapshot.LimitName)
	if name == "" {
		name = strings.TrimSpace(snapshot.LimitID)
	}
	if name == "" {
		return "rate limits updated"
	}
	return name + " rate limits updated"
}

func (p *agentRunProgress) turnCount() int {
	return len(p.turnIDs)
}

func (p *agentRunProgress) addRecentEvent(event telemetry.ActivityEvent) {
	if event.Event == "" && event.Message == "" {
		return
	}
	p.recentEvents = append(p.recentEvents, event)
	if len(p.recentEvents) > recentActivityLimit {
		p.recentEvents = p.recentEvents[len(p.recentEvents)-recentActivityLimit:]
	}
}

func (p *agentRunProgress) recentActivity() []telemetry.ActivityEvent {
	if len(p.recentEvents) == 0 {
		return nil
	}
	out := make([]telemetry.ActivityEvent, len(p.recentEvents))
	copy(out, p.recentEvents)
	return out
}

func (p *agentRunProgress) outputText() string {
	var out strings.Builder
	for _, key := range p.messageOrder {
		out.WriteString(p.messages[key])
	}
	return out.String()
}

func (r *Runner) publishRunUpdate(
	ctx context.Context,
	req RunRequest,
	info workspace.Info,
	issue workspace.Issue,
	progress *agentRunProgress,
	result RunResult,
	eventAt time.Time,
	runStartedAt time.Time,
) error {
	if req.OnUsageUpdate == nil {
		return nil
	}

	result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, eventAt)
	usage := UsageUpdate{
		SessionID:       progress.sessionID,
		ProcessIdentity: progress.processIdentity,
		WorkspacePath:   info.Path,
		TurnCount:       progress.turnCount(),
		LastEventAt:     progress.lastEventAt,
		LastEvent:       progress.lastEvent,
		LastMessage:     progress.lastMessage,
		RecentEvents:    progress.recentActivity(),
		Tokens:          result.Tokens,
		RateLimits:      result.RateLimits,
	}
	diffStats, ok := r.liveDiffStats(ctx, info, issue, progress, eventAt)
	if ok {
		usage.DiffStats = diffStats
	}
	return req.OnUsageUpdate(usage)
}

func (r *Runner) liveDiffStats(
	ctx context.Context,
	info workspace.Info,
	issue workspace.Issue,
	progress *agentRunProgress,
	eventAt time.Time,
) (DiffStats, bool) {
	if !progress.shouldRefreshDiffStats(eventAt) {
		return progress.cachedDiffStats()
	}

	progress.diffStatsCheckedAt = eventAt
	stat, err := r.workspace.DiffStat(ctx, info, issue)
	if err != nil {
		if workspace.IsMissingWorkspaceError(err) {
			r.logger.Info(
				"workspace live diff stat skipped",
				slog.String("issue_id", issue.ID),
				slog.String("issue_identifier", issue.Identifier),
				slog.String("workspace_path", info.Path),
				slog.String("phase", "live"),
				slog.String("error", err.Error()),
			)
			return progress.cachedDiffStats()
		}
		r.logger.Warn(
			"workspace live diff stat failed",
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("workspace_path", info.Path),
			slog.String("phase", "live"),
			slog.String("error", err.Error()),
		)
		return progress.cachedDiffStats()
	}

	diffStats := diffStatsFromWorkspace(stat)
	diffStats.Status = "ok"
	progress.diffStats = diffStats
	progress.diffStatsCollected = true
	return diffStats, true
}

func (p *agentRunProgress) shouldRefreshDiffStats(eventAt time.Time) bool {
	if p.diffStatsCheckedAt.IsZero() {
		return true
	}
	return eventAt.Sub(p.diffStatsCheckedAt) >= liveDiffStatsInterval
}

func (p *agentRunProgress) cachedDiffStats() (DiffStats, bool) {
	if !p.diffStatsCollected {
		return DiffStats{}, false
	}
	return p.diffStats, true
}

func pullRequestNumber(issue connector.Issue) *int64 {
	if issue.PRNumber != nil && *issue.PRNumber > 0 {
		number := int64(*issue.PRNumber)
		return &number
	}

	value := strings.TrimSpace(issue.URL)
	const marker = "/pull/"
	index := strings.LastIndex(value, marker)
	if index == -1 {
		return nil
	}

	value = value[index+len(marker):]
	end := strings.IndexFunc(value, func(r rune) bool {
		return r < '0' || r > '9'
	})
	if end != -1 {
		value = value[:end]
	}

	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 {
		return nil
	}
	return &number
}

func diffStatsFromWorkspace(stat workspace.DiffStat) DiffStats {
	status := "clean"
	if stat.Files != 0 || stat.Added != 0 || stat.Removed != 0 {
		status = "changed"
	}
	return DiffStats{
		FilesChanged: stat.Files,
		AddedLines:   stat.Added,
		RemovedLines: stat.Removed,
		Status:       status,
	}
}

func (r *Runner) logWorkerEvent(issue connector.Issue, event string, attrs ...any) {
	r.logWorkerEventLevel(slog.LevelDebug, issue, event, attrs...)
}

func (r *Runner) logWorkerEventLevel(level slog.Level, issue connector.Issue, event string, attrs ...any) {
	if r == nil || r.logger == nil {
		return
	}
	all := []any{
		"event", strings.TrimSpace(event),
		"project_id", strings.TrimSpace(r.projectID),
		"issue_id", strings.TrimSpace(issue.ID),
		"issue_identifier", strings.TrimSpace(issue.Identifier),
		"issue_state", strings.TrimSpace(issue.State),
	}
	all = append(all, attrs...)
	message := strings.TrimSpace(event)
	switch {
	case level >= slog.LevelError:
		r.logger.Error(message, all...)
	case level >= slog.LevelWarn:
		r.logger.Warn(message, all...)
	case level >= slog.LevelInfo:
		r.logger.Info(message, all...)
	default:
		r.logger.Debug(message, all...)
	}
}

func (r *Runner) logAgentUpdate(issue connector.Issue, update AgentUpdate) {
	event := strings.TrimSpace(string(update.Type))
	if event == "" {
		event = strings.TrimSpace(update.Method)
	}
	switch update.Type {
	case AgentUpdateProcessStarted:
		r.logWorkerEvent(issue, "worker_process_started",
			"process_identity", strings.TrimSpace(update.ProcessIdentity),
		)
	case AgentUpdateTurnStarted:
		r.logWorkerEvent(issue, "worker_turn_started",
			"thread_id", strings.TrimSpace(update.ThreadID),
			"turn_id", strings.TrimSpace(update.TurnID),
		)
	case AgentUpdateTurnCompleted:
		attrs := []any{
			"thread_id", strings.TrimSpace(update.ThreadID),
			"turn_id", strings.TrimSpace(update.TurnID),
			"status", strings.TrimSpace(update.Status),
		}
		attrs = append(attrs, agentUpdateBackendErrorAttrs(update)...)
		if failedAgentTurnStatus(update.Status) {
			r.logWorkerEventLevel(slog.LevelWarn, issue, "worker_turn_finished", attrs...)
		} else {
			r.logWorkerEvent(issue, "worker_turn_finished", attrs...)
		}
	case AgentUpdateTokenUsage:
		r.logWorkerEvent(issue, "worker_usage_updated",
			"thread_id", strings.TrimSpace(update.ThreadID),
			"turn_id", strings.TrimSpace(update.TurnID),
			"total_tokens", update.Tokens.TotalTokens,
			"input_tokens", update.Tokens.InputTokens,
			"output_tokens", update.Tokens.OutputTokens,
		)
	case AgentUpdateRateLimits:
		r.logWorkerEvent(issue, "worker_rate_limits_updated",
			"thread_id", strings.TrimSpace(update.ThreadID),
			"turn_id", strings.TrimSpace(update.TurnID),
		)
	default:
		if event != "" && update.Type != AgentUpdateMessageDelta {
			r.logWorkerEvent(issue, "worker_agent_update",
				"agent_event", event,
				"thread_id", strings.TrimSpace(update.ThreadID),
				"turn_id", strings.TrimSpace(update.TurnID),
			)
		}
	}
}

type backendErrorCarrier interface {
	BackendErrorBody() string
	BackendErrorMessage() string
}

func backendErrorAttrs(err error) []any {
	if err == nil {
		return nil
	}
	var carrier backendErrorCarrier
	if !errors.As(err, &carrier) {
		return nil
	}
	return backendErrorStringsAttrs(carrier.BackendErrorBody(), carrier.BackendErrorMessage())
}

func agentUpdateBackendErrorAttrs(update AgentUpdate) []any {
	return backendErrorStringsAttrs(update.BackendErrorBody, update.BackendErrorMessage)
}

func backendErrorStringsAttrs(body string, message string) []any {
	attrs := []any{}
	if body = strings.TrimSpace(body); body != "" {
		attrs = append(attrs, "backend_error_body", body)
	}
	if message = strings.TrimSpace(message); message != "" {
		attrs = append(attrs, "backend_error_message", message)
	}
	return attrs
}

func failedAgentTurnStatus(status string) bool {
	status = strings.TrimSpace(status)
	return status != "" && !strings.EqualFold(status, "completed")
}

func workerRunOutcome(err error, finalState string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	case errors.Is(err, ErrSessionTokenCeilingExceeded):
		return FinalStateTokenCeilingExceeded
	case err != nil:
		return "failed"
	case strings.EqualFold(strings.TrimSpace(finalState), FinalStateCompleted), strings.TrimSpace(finalState) == "":
		return "succeeded"
	default:
		return strings.TrimSpace(finalState)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func runtimeSeconds(startedAt, completedAt time.Time) float64 {
	if startedAt.IsZero() || completedAt.IsZero() || completedAt.Before(startedAt) {
		return 0
	}
	return completedAt.Sub(startedAt).Seconds()
}
