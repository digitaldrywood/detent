package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
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
	ErrMissingWorkspace = errors.New("runner workspace backend is required")
	ErrMissingCodex     = errors.New("runner codex client is required")
)

type CodexClient interface {
	RunTurn(context.Context, codex.RunTurnRequest, codex.UpdateHandler) (codex.RunTurnResult, error)
}

type SessionStore interface {
	StartSession(context.Context, store.SessionStart) (int64, error)
	FinishSession(context.Context, int64, store.SessionFinish) error
	RecordUsageEvent(context.Context, store.UsageEvent) (int64, error)
}

type Dependencies struct {
	ProjectID       string
	Workflow        config.Workflow
	Workspace       workspace.Backend
	Codex           CodexClient
	Store           SessionStore
	Pricing         budget.PricingTable
	Now             func() time.Time
	Logger          *slog.Logger
	AfterRunTimeout time.Duration
}

type Runner struct {
	mu              sync.RWMutex
	projectID       string
	workflow        config.Workflow
	workspace       workspace.Backend
	codex           CodexClient
	store           SessionStore
	pricing         budget.PricingTable
	now             func() time.Time
	logger          *slog.Logger
	afterRunTimeout time.Duration
}

func NewRunner(deps Dependencies) (*Runner, error) {
	if deps.Workspace == nil {
		return nil, ErrMissingWorkspace
	}
	if deps.Codex == nil {
		return nil, ErrMissingCodex
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

	return &Runner{
		projectID:       projectID,
		workflow:        deps.Workflow,
		workspace:       deps.Workspace,
		codex:           deps.Codex,
		store:           deps.Store,
		pricing:         deps.Pricing,
		now:             deps.Now,
		logger:          deps.Logger,
		afterRunTimeout: deps.AfterRunTimeout,
	}, nil
}

func (r *Runner) UpdateWorkflow(workflow config.Workflow) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.workflow = workflow
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflow := r.workflowSnapshot()

	workspaceIssue := workspaceIssue(req.Issue)
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
		WorkspacePath:   info.Path,
		AvailableSkills: availableSkills,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("build prompt: %w", err)
	}

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now().UTC()
	}
	runStartedAt := r.now()
	model := strings.TrimSpace(req.Issue.ModelOverride)
	sessionID, sessionStarted, err := r.startSession(ctx, req.Issue, startedAt, model)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{FinalState: FinalStateCompleted}
	progress := newCodexRunProgress()
	turnResult, turnErr := r.codex.RunTurn(ctx, codex.RunTurnRequest{
		Workspace:         info.Path,
		Prompt:            prompt,
		ApprovalPolicy:    stringOrMapValue(workflow.Config.Codex.ApprovalPolicy),
		ThreadSandbox:     workflow.Config.Codex.ThreadSandbox,
		TurnSandboxPolicy: workflow.Config.Codex.TurnSandboxPolicy,
		Model:             model,
	}, func(update codex.Update) error {
		applyCodexUpdate(&result, update)
		if err := r.publishRunUpdate(ctx, req, info, workspaceIssue, progress, result, update, runStartedAt); err != nil {
			return err
		}
		return nil
	})
	_ = turnResult

	r.afterRun(info, workspaceIssue)
	afterRunPending = false

	if turnErr != nil {
		result.FinalState = FinalStateFailed
		finishedAt := r.now().UTC()
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, finishedAt)
		return result, errors.Join(
			fmt.Errorf("run codex turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, req.Issue, startedAt, finishedAt, result, model, 1),
		)
	}

	diffStat, err := r.workspace.DiffStat(ctx, info, workspaceIssue)
	if err != nil {
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

func (r *Runner) workflowSnapshot() config.Workflow {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.workflow
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

func workspaceIssue(issue connector.Issue) workspace.Issue {
	return workspace.Issue{
		ID:         issue.ID,
		Identifier: issue.Identifier,
		BranchName: issue.BranchName,
	}
}

func applyCodexUpdate(result *RunResult, update codex.Update) {
	switch update.Type {
	case codex.UpdateTokenUsage:
		result.Tokens.InputTokens = update.Tokens.InputTokens
		result.Tokens.OutputTokens = update.Tokens.OutputTokens
		result.Tokens.TotalTokens = update.Tokens.TotalTokens
	case codex.UpdateRateLimits:
		result.RateLimits = rateLimitsFromCodex(update.RateLimits)
	}
}

type codexRunProgress struct {
	sessionID          string
	processIdentity    string
	turnIDs            map[string]struct{}
	messages           map[string]string
	lastEventAt        time.Time
	lastEvent          string
	lastMessage        string
	recentEvents       []telemetry.ActivityEvent
	diffStats          DiffStats
	diffStatsCollected bool
	diffStatsCheckedAt time.Time
}

func newCodexRunProgress() *codexRunProgress {
	return &codexRunProgress{
		turnIDs:  map[string]struct{}{},
		messages: map[string]string{},
	}
}

func (p *codexRunProgress) apply(update codex.Update, eventAt time.Time) {
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
	case codex.UpdateAgentMessageDelta:
		key := update.ItemID
		if key == "" {
			key = update.TurnID
		}
		p.messages[key] += update.Delta
		p.lastMessage = strings.TrimSpace(p.messages[key])
		eventMessage = p.lastMessage
	case codex.UpdateTurnStarted:
		p.lastMessage = "turn started"
		eventMessage = p.lastMessage
	case codex.UpdateTurnCompleted:
		status := update.Status
		if status == "" {
			status = "completed"
		}
		p.lastMessage = "turn " + status
		eventMessage = p.lastMessage
	case codex.UpdateTokenUsage:
		eventMessage = tokenUsageActivityMessage(update.Tokens)
	case codex.UpdateRateLimits:
		eventMessage = rateLimitsActivityMessage(update.RateLimits)
	case codex.UpdateProcessStarted:
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

func tokenUsageActivityMessage(tokens codex.TokenUsage) string {
	if tokens.TotalTokens > 0 && (tokens.InputTokens > 0 || tokens.OutputTokens > 0) {
		return fmt.Sprintf("%d total tokens (%d in, %d out)", tokens.TotalTokens, tokens.InputTokens, tokens.OutputTokens)
	}
	if tokens.TotalTokens > 0 {
		return fmt.Sprintf("%d total tokens", tokens.TotalTokens)
	}
	return "tokens updated"
}

func rateLimitsActivityMessage(snapshot *codex.RateLimitSnapshot) string {
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

func (p *codexRunProgress) turnCount() int {
	return len(p.turnIDs)
}

func (p *codexRunProgress) addRecentEvent(event telemetry.ActivityEvent) {
	if event.Event == "" && event.Message == "" {
		return
	}
	p.recentEvents = append(p.recentEvents, event)
	if len(p.recentEvents) > recentActivityLimit {
		p.recentEvents = p.recentEvents[len(p.recentEvents)-recentActivityLimit:]
	}
}

func (p *codexRunProgress) recentActivity() []telemetry.ActivityEvent {
	if len(p.recentEvents) == 0 {
		return nil
	}
	out := make([]telemetry.ActivityEvent, len(p.recentEvents))
	copy(out, p.recentEvents)
	return out
}

func (r *Runner) publishRunUpdate(
	ctx context.Context,
	req RunRequest,
	info workspace.Info,
	issue workspace.Issue,
	progress *codexRunProgress,
	result RunResult,
	update codex.Update,
	runStartedAt time.Time,
) error {
	if req.OnUsageUpdate == nil {
		return nil
	}

	eventAt := r.now()
	progress.apply(update, eventAt)
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
	progress *codexRunProgress,
	eventAt time.Time,
) (DiffStats, bool) {
	if !progress.shouldRefreshDiffStats(eventAt) {
		return progress.cachedDiffStats()
	}

	progress.diffStatsCheckedAt = eventAt
	stat, err := r.workspace.DiffStat(ctx, info, issue)
	if err != nil {
		r.logger.Warn(
			"workspace live diff stat failed",
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
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

func (p *codexRunProgress) shouldRefreshDiffStats(eventAt time.Time) bool {
	if p.diffStatsCheckedAt.IsZero() {
		return true
	}
	return eventAt.Sub(p.diffStatsCheckedAt) >= liveDiffStatsInterval
}

func (p *codexRunProgress) cachedDiffStats() (DiffStats, bool) {
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

func rateLimitsFromCodex(snapshot *codex.RateLimitSnapshot) *telemetry.RateLimits {
	if snapshot == nil {
		return nil
	}
	return &telemetry.RateLimits{
		LimitID:   snapshot.LimitID,
		LimitName: snapshot.LimitName,
		Primary:   rateLimitBucketFromCodex(snapshot.Primary),
		Secondary: rateLimitBucketFromCodex(snapshot.Secondary),
		Credits:   creditsBucketFromCodex(snapshot.Credits),
	}
}

func rateLimitBucketFromCodex(window *codex.RateLimitWindow) *telemetry.RateLimitBucket {
	if window == nil {
		return nil
	}

	used := int64(math.Round(window.UsedPercent))
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}

	bucket := &telemetry.RateLimitBucket{
		Limit:     100,
		Used:      used,
		Remaining: 100 - used,
	}
	if window.ResetsAt != nil {
		resetAt := time.Unix(*window.ResetsAt, 0).UTC()
		bucket.ResetAt = &resetAt
	}
	return bucket
}

func creditsBucketFromCodex(credits *codex.CreditsSnapshot) *telemetry.RateLimitBucket {
	if credits == nil {
		return nil
	}
	return &telemetry.RateLimitBucket{
		HasCredits: credits.HasCredits,
		Unlimited:  credits.Unlimited,
		Balance:    credits.Balance,
	}
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
