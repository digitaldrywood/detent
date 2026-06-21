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

	return &Runner{
		projectID:           projectID,
		workflow:            deps.Workflow,
		workspace:           deps.Workspace,
		agentRuntime:        runtime,
		agentBackendFactory: deps.AgentBackendFactory,
		store:               deps.Store,
		pricing:             deps.Pricing,
		now:                 deps.Now,
		logger:              deps.Logger,
		afterRunTimeout:     deps.AfterRunTimeout,
	}, nil
}

func (r *Runner) UpdateWorkflow(workflow config.Workflow) {
	r.mu.RLock()
	currentBackends := cloneAgentBackends(r.agentRuntime.backends)
	factory := r.agentBackendFactory
	r.mu.RUnlock()

	runtime, err := newAgentRuntime(workflow, currentBackends, factory)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.workflow = workflow
	if err != nil {
		r.logger.Warn("reload agent runtime failed", "error", err)
		return
	}
	r.agentRuntime = runtime
}

type agentRuntime struct {
	backends       map[string]AgentBackend
	backendConfigs map[string]config.AgentBackend
	router         *Router
}

func newAgentRuntime(
	workflow config.Workflow,
	staticBackends map[string]AgentBackend,
	factory AgentBackendFactory,
) (agentRuntime, error) {
	backendConfigs := effectiveAgentBackendConfigs(workflow.Config)
	backends := make(map[string]AgentBackend, len(backendConfigs))
	configsByID := make(map[string]config.AgentBackend, len(backendConfigs))
	for _, backendConfig := range backendConfigs {
		if strings.TrimSpace(backendConfig.ID) == "" {
			continue
		}
		configsByID[backendConfig.ID] = backendConfig
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
		backends:       backends,
		backendConfigs: configsByID,
		router:         router,
	}, nil
}

func effectiveAgentBackendConfigs(cfg config.Config) []config.AgentBackend {
	configs := cfg.AgentBackendConfigs()
	for index, backend := range configs {
		if backend.Kind != config.AgentBackendCodex {
			continue
		}
		effectiveCodex := backend.CodexConfig(cfg.Codex)
		effective := config.CodexAgentBackend(effectiveCodex)
		effective.ID = backend.ID
		effective.Kind = backend.Kind
		effective.Protocol = backend.Protocol
		configs[index] = effective
	}
	return configs
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

func (r agentRuntime) selectBackend(issue connector.Issue, ctx selector.Context) (RouteSelection, AgentBackend, config.AgentBackend, error) {
	return r.selectBackendForRole(issue, ctx, RoleCode)
}

func (r agentRuntime) selectBackendForRole(issue connector.Issue, ctx selector.Context, role string) (RouteSelection, AgentBackend, config.AgentBackend, error) {
	selection, err := r.router.RouteForRole(issue, ctx, role)
	if err != nil {
		return RouteSelection{}, nil, config.AgentBackend{}, err
	}
	backend, ok := r.backends[selection.BackendID]
	if !ok {
		return RouteSelection{}, nil, config.AgentBackend{}, fmt.Errorf("%w: %s", ErrMissingAgentBackend, selection.BackendID)
	}
	backendConfig, ok := r.backendConfigs[selection.BackendID]
	if !ok {
		return RouteSelection{}, nil, config.AgentBackend{}, fmt.Errorf("agent backend config not found: %s", selection.BackendID)
	}
	return selection, backend, backendConfig, nil
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
	default:
		return RunModeImplement
	}
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, agentRuntime := r.runtimeSnapshot()

	workspaceIssue := workspaceIssue(r.projectID, req.Issue)
	info, err := r.workspace.Create(ctx, workspaceIssue)
	if err != nil {
		return RunResult{}, fmt.Errorf("create workspace: %w", err)
	}

	if err := r.workspace.BeforeRun(ctx, info, workspaceIssue); err != nil {
		return RunResult{}, fmt.Errorf("workspace before_run: %w", err)
	}

	afterRunPending := true
	defer func() {
		if afterRunPending {
			r.afterRun(info, workspaceIssue)
		}
	}()

	availableSkills, err := r.availableSkills(workflow, info.Path)
	if err != nil {
		return RunResult{}, err
	}

	attempt := req.Attempt
	prompt, err := BuildPrompt(workflow, req.Issue, PromptOptions{
		Attempt:         &attempt,
		PlanOnly:        normalizeRunMode(req.Mode) == RunModePlan,
		WorkspacePath:   info.Path,
		Branch:          info.Branch,
		AvailableSkills: availableSkills,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("build prompt: %w", err)
	}
	selection, backend, backendConfig, err := agentRuntime.selectBackend(req.Issue, selectorContext(req.SelectorContext, workflow))
	if err != nil {
		return RunResult{}, err
	}

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	runStartedAt := r.now()
	model := selection.Model
	sessionID, sessionStarted, err := r.startSession(ctx, req.Issue, startedAt, model)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{FinalState: FinalStateCompleted}
	progress := newAgentRunProgress()
	turnResult, turnErr := backend.RunTurn(ctx, AgentTurnRequest{
		Workspace:         info.Path,
		Prompt:            prompt,
		ApprovalPolicy:    stringOrMapValue(backendConfig.Options.ApprovalPolicy),
		ThreadSandbox:     backendConfig.Options.ThreadSandbox,
		TurnSandboxPolicy: backendConfig.Options.TurnSandboxPolicy,
		Model:             model,
	}, func(update AgentUpdate) error {
		applyAgentUpdate(&result, update)
		eventAt := r.now()
		progress.apply(update, eventAt)
		if err := r.publishRunUpdate(ctx, req, info, workspaceIssue, progress, result, eventAt, runStartedAt); err != nil {
			return err
		}
		return nil
	})
	_ = turnResult
	result.Output = progress.outputText()

	r.afterRun(info, workspaceIssue)
	afterRunPending = false

	if turnErr != nil {
		result.FinalState = FinalStateFailed
		finishedAt := r.now().UTC()
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
		return result, errors.Join(
			fmt.Errorf("run agent turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, model, 1),
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
			if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, model, 1); err != nil {
				return result, err
			}
			return result, nil
		}
		result.FinalState = FinalStateFailed
		finishedAt := r.now().UTC()
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
		return result, errors.Join(
			fmt.Errorf("workspace diff stat: %w", err),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, model, 1),
		)
	}

	result.DiffStats = diffStatsFromWorkspace(diffStat)
	finishedAt := r.now().UTC()
	result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
	if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, model, 1); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Runner) Validate(ctx context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow, agentRuntime := r.runtimeSnapshot()

	workspaceIssue := workspaceIssue(r.projectID, req.Issue)
	info, err := r.workspace.Create(ctx, workspaceIssue)
	if err != nil {
		return gate.ValidatorResult{}, fmt.Errorf("create workspace: %w", err)
	}

	if err := r.workspace.BeforeRun(ctx, info, workspaceIssue); err != nil {
		return gate.ValidatorResult{}, fmt.Errorf("workspace before_run: %w", err)
	}

	afterRunPending := true
	defer func() {
		if afterRunPending {
			r.afterRun(info, workspaceIssue)
		}
	}()

	prompt := BuildValidatorPrompt(workflow, req.Issue, ValidatorPromptOptions{
		WorkspacePath: info.Path,
		Branch:        info.Branch,
	})
	selection, backend, backendConfig, err := agentRuntime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), RoleValidator)
	if err != nil {
		return gate.ValidatorResult{}, err
	}

	model := selection.Model
	if override := strings.TrimSpace(workflow.Config.Gate.Validator.Model); override != "" {
		model = override
	}

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	runStartedAt := r.now()
	sessionID, sessionStarted, err := r.startSession(ctx, req.Issue, startedAt, model)
	if err != nil {
		return gate.ValidatorResult{}, err
	}

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
		Workspace:         info.Path,
		Prompt:            prompt,
		ApprovalPolicy:    stringOrMapValue(backendConfig.Options.ApprovalPolicy),
		ThreadSandbox:     backendConfig.Options.ThreadSandbox,
		TurnSandboxPolicy: backendConfig.Options.TurnSandboxPolicy,
		Model:             model,
	}, func(update AgentUpdate) error {
		if update.Type == AgentUpdateMessageDelta {
			output.WriteString(update.Delta)
		}
		applyAgentUpdate(&runResult, update)
		if err := r.publishRunUpdate(ctx, runReq, info, workspaceIssue, progress, runResult, update, runStartedAt); err != nil {
			return err
		}
		return nil
	})
	_ = turnResult

	r.afterRun(info, workspaceIssue)
	afterRunPending = false

	finishedAt := r.now().UTC()
	runResult.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
	if turnErr != nil {
		runResult.FinalState = FinalStateFailed
		return gate.ValidatorResult{}, errors.Join(
			fmt.Errorf("run validator turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, model, 1),
		)
	}

	validation, err := parseValidatorResult(output.String())
	if err != nil {
		runResult.FinalState = FinalStateFailed
		return gate.ValidatorResult{}, errors.Join(
			fmt.Errorf("parse validator result: %w", err),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, model, 1),
		)
	}
	if err := r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, runResult, model, 1); err != nil {
		return gate.ValidatorResult{}, err
	}
	return validation, nil
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

func (r *Runner) runtimeSnapshot() (config.Workflow, agentRuntime) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.workflow, r.agentRuntime
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

func (r *Runner) startSession(
	ctx context.Context,
	issue connector.Issue,
	startedAt time.Time,
	model string,
) (int64, bool, error) {
	if r.store == nil {
		return 0, false, nil
	}

	sessionID, err := r.store.StartSession(ctx, store.SessionStart{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
		StartedAt:  startedAt,
		Model:      model,
	})
	if err != nil {
		return 0, false, fmt.Errorf("start codex session: %w", err)
	}
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
	turns int64,
) error {
	if !started {
		return nil
	}
	if result.FinalState == "" {
		result.FinalState = FinalStateCompleted
	}

	if err := r.store.FinishSession(ctx, sessionID, store.SessionFinish{
		CompletedAt:    finishedAt,
		Turns:          turns,
		InputTokens:    result.Tokens.InputTokens,
		OutputTokens:   result.Tokens.OutputTokens,
		TotalTokens:    result.Tokens.TotalTokens,
		RuntimeSeconds: int64(math.Round(result.Tokens.RuntimeSeconds)),
		FinalState:     result.FinalState,
		Model:          model,
	}); err != nil {
		return fmt.Errorf("finish codex session: %w", err)
	}
	if _, err := r.store.RecordUsageEvent(ctx, store.UsageEvent{
		ProjectID:      r.projectID,
		SessionID:      sessionID,
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		PRNumber:       pullRequestNumber(issue),
		Model:          model,
		InputTokens:    result.Tokens.InputTokens,
		OutputTokens:   result.Tokens.OutputTokens,
		TotalTokens:    result.Tokens.TotalTokens,
		CostUSD:        r.usageCostUSD(model, result.Tokens.InputTokens, result.Tokens.OutputTokens),
		RuntimeSeconds: int64(math.Round(result.Tokens.RuntimeSeconds)),
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
		Outcome:        result.FinalState,
	}); err != nil {
		return fmt.Errorf("record usage event: %w", err)
	}
	return nil
}

func (r *Runner) usageCostUSD(model string, inputTokens int64, outputTokens int64) float64 {
	cost, ok := budget.UsageCostUSD(r.pricing, model, inputTokens, outputTokens)
	if !ok {
		r.logger.Warn("usage event model pricing not found", "model", strings.TrimSpace(model))
		return 0
	}
	return cost
}

func workspaceIssue(projectID string, issue connector.Issue) workspace.Issue {
	return workspace.Issue{
		ProjectID:  projectID,
		ID:         issue.ID,
		Identifier: issue.Identifier,
		BranchName: issue.BranchName,
	}
}

func applyAgentUpdate(result *RunResult, update AgentUpdate) {
	switch update.Type {
	case AgentUpdateTokenUsage:
		result.Tokens.InputTokens = update.Tokens.InputTokens
		result.Tokens.OutputTokens = update.Tokens.OutputTokens
		result.Tokens.TotalTokens = update.Tokens.TotalTokens
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

func stringOrMapValue(value config.StringOrMap) any {
	if value.IsMap {
		return cloneMap(value.Map)
	}
	if value.IsString {
		return value.String
	}
	return nil
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return value
	}
	return cloned
}

func runtimeSeconds(startedAt, completedAt time.Time) float64 {
	if startedAt.IsZero() || completedAt.IsZero() || completedAt.Before(startedAt) {
		return 0
	}
	return completedAt.Sub(startedAt).Seconds()
}
