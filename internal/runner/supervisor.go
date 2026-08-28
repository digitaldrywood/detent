package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultSupervisorMaxRetryBackoff       = 5 * time.Minute
	defaultSupervisorFailureRetryBaseDelay = 10 * time.Second
	defaultSupervisorOverloadRetryDelay    = 45 * time.Second
	defaultCompletionDeliveryGrace         = 250 * time.Millisecond
)

var ErrMissingRunner = errors.New("runner backend is required")

type SupervisorConfig struct {
	MaxRetryBackoff       time.Duration
	FailureRetryBaseDelay time.Duration
	OverloadRetryDelay    time.Duration
	Now                   func() time.Time
	Logger                *slog.Logger
	DispatchPacer         DispatchPacer
}

type Supervisor struct {
	mu                    sync.RWMutex
	backend               Backend
	maxRetryBackoff       time.Duration
	failureRetryBaseDelay time.Duration
	overloadRetryDelay    time.Duration
	now                   func() time.Time
	logger                *slog.Logger
	dispatchPacer         DispatchPacer
}

type Completion struct {
	IssueID      string
	Request      RunRequest
	Result       RunResult
	Err          error
	CompletedAt  time.Time
	Retryable    bool
	RetryAttempt int
	RetryDelay   time.Duration
}

func NewSupervisor(backend Backend, cfg SupervisorConfig) (*Supervisor, error) {
	if backend == nil {
		return nil, ErrMissingRunner
	}
	if cfg.MaxRetryBackoff <= 0 {
		cfg.MaxRetryBackoff = defaultSupervisorMaxRetryBackoff
	}
	if cfg.FailureRetryBaseDelay <= 0 {
		cfg.FailureRetryBaseDelay = defaultSupervisorFailureRetryBaseDelay
	}
	if cfg.OverloadRetryDelay <= 0 {
		cfg.OverloadRetryDelay = defaultSupervisorOverloadRetryDelay
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Supervisor{
		backend:               backend,
		maxRetryBackoff:       cfg.MaxRetryBackoff,
		failureRetryBaseDelay: cfg.FailureRetryBaseDelay,
		overloadRetryDelay:    cfg.OverloadRetryDelay,
		now:                   cfg.Now,
		logger:                cfg.Logger,
		dispatchPacer:         cfg.DispatchPacer,
	}, nil
}

func (s *Supervisor) UpdateConfig(cfg SupervisorConfig) {
	if cfg.MaxRetryBackoff <= 0 {
		cfg.MaxRetryBackoff = defaultSupervisorMaxRetryBackoff
	}
	if cfg.FailureRetryBaseDelay <= 0 {
		cfg.FailureRetryBaseDelay = defaultSupervisorFailureRetryBaseDelay
	}
	if cfg.OverloadRetryDelay <= 0 {
		cfg.OverloadRetryDelay = defaultSupervisorOverloadRetryDelay
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.maxRetryBackoff = cfg.MaxRetryBackoff
	s.failureRetryBaseDelay = cfg.FailureRetryBaseDelay
	s.overloadRetryDelay = cfg.OverloadRetryDelay
}

func (s *Supervisor) Dispatch(ctx context.Context, request RunRequest, completions chan<- Completion) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		completion := s.Run(ctx, request)
		close(done)
		if completions == nil {
			return
		}
		select {
		case completions <- completion:
			return
		default:
		}
		if ctx == nil {
			completions <- completion
			return
		}
		select {
		case completions <- completion:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(defaultCompletionDeliveryGrace)
		defer timer.Stop()
		select {
		case completions <- completion:
		case <-timer.C:
			s.logger.Warn(
				"runner completion delivery timed out after context cancellation",
				slog.String("issue_id", request.Issue.ID),
				slog.String("issue_identifier", request.Issue.Identifier),
				slog.Any("error", completion.Err),
			)
		}
	}()
	return done
}

func (s *Supervisor) Run(ctx context.Context, request RunRequest) (completion Completion) {
	completion = Completion{
		IssueID:     request.Issue.ID,
		Request:     request,
		CompletedAt: s.now(),
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			completion.Err = fmt.Errorf("runner panic: %v", recovered)
			s.logger.Error(
				"runner panic recovered",
				slog.String("issue_id", request.Issue.ID),
				slog.String("issue_identifier", request.Issue.Identifier),
				slog.Any("panic", recovered),
			)
		}
		if IsTransientOverload(completion.Err) || errors.Is(completion.Err, ErrWorkspaceBranchHeld) {
			completion.Retryable = true
			completion.RetryAttempt = request.Attempt
			completion.RetryDelay = s.OverloadRetryDelay()
		} else if completion.Err != nil && !IsCapacityError(completion.Err) && !cooperativeStopError(completion.Err) {
			completion.Retryable = true
			completion.RetryAttempt = nextFailureAttempt(request.Attempt)
			completion.RetryDelay = s.RetryDelay(completion.RetryAttempt)
		}
		attrs := []any{
			"issue_state", request.Issue.State,
			"attempt", request.Attempt,
			"worker_host", request.WorkerHost,
			"mode", request.Mode,
			"outcome", workerRunOutcome(completion.Err, completion.Result.FinalState),
			"retryable", completion.Retryable,
			"retry_attempt", completion.RetryAttempt,
			"retry_delay_seconds", int64(completion.RetryDelay / time.Second),
			"error", completion.Err,
		}
		attrs = append(attrs, backendErrorAttrs(completion.Err)...)
		level := slog.LevelDebug
		if IsTransientOverload(completion.Err) {
			attrs = append(attrs, "reason", "transient_overload")
			level = slog.LevelInfo
		} else if errors.Is(completion.Err, ErrWorkspaceBranchHeld) {
			attrs = append(attrs, "reason", "workspace_branch_held")
			level = slog.LevelInfo
		} else if completion.Err != nil {
			level = slog.LevelWarn
		}
		telemetry.LogLifecycle(s.logger, level, telemetry.LifecycleWorkAttempt, "worker_attempt_finished", telemetry.LifecycleCorrelation{
			ProjectID:       request.ProjectID,
			IssueID:         request.Issue.ID,
			IssueIdentifier: request.Issue.Identifier,
			WorkAttemptID:   request.WorkAttemptID,
		}, attrs...)
	}()
	if s.dispatchPacer != nil {
		if err := s.dispatchPacer.Wait(ctx); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
			completion.CompletedAt = s.now()
			completion.Err = err
			return completion
		}
	}
	var startup startupObservation
	if census, ok := s.dispatchPacer.(startupCensus); ok {
		startup = census.BeginStartup(request.WorkerHost)
		defer startup.Finish()
		usageHandler := request.OnUsageUpdate
		if usageHandler != nil {
			request.OnUsageUpdate = func(update UsageUpdate) error {
				if update.TurnCount > 0 || update.LastEvent == string(AgentUpdateTurnStarted) {
					startup.Ready()
				}
				return usageHandler(update)
			}
		}
	}

	result, err := s.backend.Run(ctx, request)
	if cause := context.Cause(ctx); errors.Is(cause, ErrMergeWorkerStartupTimeout) ||
		errors.Is(cause, ErrMergeWorkerDurationExceeded) ||
		errors.Is(cause, ErrCIUnavailable) ||
		errors.Is(cause, ErrLaneRevoked) {
		err = errors.Join(cause, err)
		if errors.Is(cause, ErrMergeWorkerDurationExceeded) {
			result.FinalState = FinalStateMergeDurationExceeded
		}
		if errors.Is(cause, ErrLaneRevoked) {
			result.FinalState = FinalStateLaneRevoked
		}
	}
	if startup != nil {
		snapshot := startup.Snapshot()
		backendcapacity.SetStartupHostSnapshot(err, request.WorkerHost, snapshot.concurrentStartups, snapshot.activeWorkers)
	}
	completion.CompletedAt = s.now()
	completion.Result = result
	completion.Err = err
	return completion
}

func cooperativeStopError(err error) bool {
	return errors.Is(err, ErrOperatorStopped) ||
		errors.Is(err, ErrMergeRevoked) ||
		errors.Is(err, ErrLaneRevoked) ||
		errors.Is(err, ErrCIUnavailable) ||
		errors.Is(err, ErrModelPermitUnavailable) ||
		errors.Is(err, ErrMergeWorkerStartupTimeout) ||
		errors.Is(err, ErrMergeWorkerDurationExceeded) ||
		errors.Is(err, ErrMergeFallbackBudgetExceeded) ||
		errors.Is(err, ErrSessionBudgetProjectionExceeded) ||
		errors.Is(err, ErrSessionDurationExceeded) ||
		errors.Is(err, ErrSessionMemoryCeilingExceeded) ||
		errors.Is(err, ErrSessionTurnLimitExceeded) ||
		errors.Is(err, ErrSessionNoProgress) ||
		errors.Is(err, ErrWorkerGitHubBudgetMonitor) ||
		IsDeliverableConfigurationError(err)
}

func (s *Supervisor) OverloadRetryDelay() time.Duration {
	s.mu.RLock()
	delay := s.overloadRetryDelay
	s.mu.RUnlock()
	return delay
}

func (s *Supervisor) RetryDelay(attempt int) time.Duration {
	s.mu.RLock()
	maxRetryBackoff := s.maxRetryBackoff
	failureRetryBaseDelay := s.failureRetryBaseDelay
	s.mu.RUnlock()

	if attempt < 1 {
		attempt = 1
	}

	delay := failureRetryBaseDelay
	for range attempt - 1 {
		if delay >= maxRetryBackoff || delay > maxRetryBackoff/2 {
			return maxRetryBackoff
		}
		delay *= 2
	}
	if delay > maxRetryBackoff {
		return maxRetryBackoff
	}
	return delay
}

func nextFailureAttempt(current int) int {
	if current < 0 {
		return 1
	}
	return current + 1
}
