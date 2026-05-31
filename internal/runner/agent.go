package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/codex"
	"github.com/digitaldrywood/symphony-go/internal/config"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/skills"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
	"github.com/digitaldrywood/symphony-go/internal/workspace"
)

const defaultAfterRunTimeout = time.Minute

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
}

type Dependencies struct {
	Workflow        config.Workflow
	Workspace       workspace.Backend
	Codex           CodexClient
	Store           SessionStore
	Now             func() time.Time
	Logger          *slog.Logger
	AfterRunTimeout time.Duration
}

type Runner struct {
	mu              sync.RWMutex
	workflow        config.Workflow
	workspace       workspace.Backend
	codex           CodexClient
	store           SessionStore
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

	return &Runner{
		workflow:        deps.Workflow,
		workspace:       deps.Workspace,
		codex:           deps.Codex,
		store:           deps.Store,
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
	turnResult, turnErr := r.codex.RunTurn(ctx, codex.RunTurnRequest{
		Workspace:         info.Path,
		Prompt:            prompt,
		ApprovalPolicy:    stringOrMapValue(workflow.Config.Codex.ApprovalPolicy),
		ThreadSandbox:     workflow.Config.Codex.ThreadSandbox,
		TurnSandboxPolicy: workflow.Config.Codex.TurnSandboxPolicy,
		Model:             model,
	}, func(update codex.Update) error {
		applyCodexUpdate(&result, update)
		return nil
	})
	_ = turnResult

	r.afterRun(info, workspaceIssue)
	afterRunPending = false

	if turnErr != nil {
		result.FinalState = FinalStateFailed
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, r.now())
		return result, errors.Join(
			fmt.Errorf("run codex turn: %w", turnErr),
			r.finishSession(ctx, sessionID, sessionStarted, result, model, 1),
		)
	}

	diffStat, err := r.workspace.DiffStat(ctx, info, workspaceIssue)
	if err != nil {
		result.FinalState = FinalStateFailed
		result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, r.now())
		return result, errors.Join(
			fmt.Errorf("workspace diff stat: %w", err),
			r.finishSession(ctx, sessionID, sessionStarted, result, model, 1),
		)
	}

	result.DiffStats = diffStatsFromWorkspace(diffStat)
	result.Tokens.RuntimeSeconds = runtimeSeconds(runStartedAt, r.now())
	if err := r.finishSession(ctx, sessionID, sessionStarted, result, model, 1); err != nil {
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
		CompletedAt:    r.now().UTC(),
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
	return nil
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

func rateLimitsFromCodex(snapshot *codex.RateLimitSnapshot) *telemetry.RateLimits {
	if snapshot == nil {
		return nil
	}
	return &telemetry.RateLimits{
		LimitID:   snapshot.LimitID,
		LimitName: snapshot.LimitName,
		Primary:   rateLimitBucketFromCodex(snapshot.Primary),
		Secondary: rateLimitBucketFromCodex(snapshot.Secondary),
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
