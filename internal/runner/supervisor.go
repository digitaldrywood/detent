package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
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
}

type Supervisor struct {
	mu                    sync.RWMutex
	backend               Backend
	maxRetryBackoff       time.Duration
	failureRetryBaseDelay time.Duration
	overloadRetryDelay    time.Duration
	now                   func() time.Time
	logger                *slog.Logger
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
		if IsTransientOverload(completion.Err) {
			completion.Retryable = true
			completion.RetryAttempt = request.Attempt
			completion.RetryDelay = s.OverloadRetryDelay()
		} else if completion.Err != nil && !IsCapacityError(completion.Err) && !cooperativeStopError(completion.Err) {
			completion.Retryable = true
			completion.RetryAttempt = nextFailureAttempt(request.Attempt)
			completion.RetryDelay = s.RetryDelay(completion.RetryAttempt)
		}
		attrs := []any{
			"event", "worker_attempt_finished",
			"issue_id", request.Issue.ID,
			"issue_identifier", request.Issue.Identifier,
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
		if IsTransientOverload(completion.Err) {
			attrs = append(attrs, "reason", "transient_overload")
			s.logger.Info("worker_attempt_finished", attrs...)
		} else if completion.Err != nil {
			s.logger.Warn("worker_attempt_finished", attrs...)
		} else {
			s.logger.Debug("worker_attempt_finished", attrs...)
		}
	}()

	result, err := s.backend.Run(ctx, request)
	if cause := context.Cause(ctx); errors.Is(cause, ErrMergeWorkerDurationExceeded) {
		err = errors.Join(cause, err)
		result.FinalState = FinalStateMergeDurationExceeded
	}
	completion.CompletedAt = s.now()
	completion.Result = result
	completion.Err = err
	return completion
}

func cooperativeStopError(err error) bool {
	return errors.Is(err, ErrOperatorStopped) ||
		errors.Is(err, ErrMergeRevoked) ||
		errors.Is(err, ErrMergeWorkerDurationExceeded)
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
