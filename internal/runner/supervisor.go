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
	defaultCompletionDeliveryGrace         = 250 * time.Millisecond
)

var ErrMissingRunner = errors.New("runner backend is required")

type SupervisorConfig struct {
	MaxRetryBackoff       time.Duration
	FailureRetryBaseDelay time.Duration
	Now                   func() time.Time
	Logger                *slog.Logger
}

type Supervisor struct {
	mu                    sync.RWMutex
	backend               Backend
	maxRetryBackoff       time.Duration
	failureRetryBaseDelay time.Duration
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

	s.mu.Lock()
	defer s.mu.Unlock()

	s.maxRetryBackoff = cfg.MaxRetryBackoff
	s.failureRetryBaseDelay = cfg.FailureRetryBaseDelay
}

func (s *Supervisor) Dispatch(ctx context.Context, request RunRequest, completions chan<- Completion) {
	go func() {
		completion := s.Run(ctx, request)
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
		if completion.Err != nil {
			completion.Retryable = true
			completion.RetryAttempt = nextFailureAttempt(request.Attempt)
			completion.RetryDelay = s.RetryDelay(completion.RetryAttempt)
		}
		s.logger.Debug(
			"worker_attempt_finished",
			slog.String("event", "worker_attempt_finished"),
			slog.String("issue_id", request.Issue.ID),
			slog.String("issue_identifier", request.Issue.Identifier),
			slog.String("issue_state", request.Issue.State),
			slog.Int("attempt", request.Attempt),
			slog.String("worker_host", request.WorkerHost),
			slog.String("mode", request.Mode),
			slog.String("outcome", workerRunOutcome(completion.Err, completion.Result.FinalState)),
			slog.Bool("retryable", completion.Retryable),
			slog.Int("retry_attempt", completion.RetryAttempt),
			slog.Int64("retry_delay_seconds", int64(completion.RetryDelay/time.Second)),
			slog.Any("error", completion.Err),
		)
	}()

	result, err := s.backend.Run(ctx, request)
	completion.CompletedAt = s.now()
	completion.Result = result
	completion.Err = err
	return completion
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
