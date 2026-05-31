package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"
)

const (
	defaultSupervisorMaxRetryBackoff       = 5 * time.Minute
	defaultSupervisorFailureRetryBaseDelay = 10 * time.Second
)

var ErrMissingRunner = errors.New("runner backend is required")

type SupervisorConfig struct {
	MaxRetryBackoff       time.Duration
	FailureRetryBaseDelay time.Duration
	Now                   func() time.Time
	Logger                *slog.Logger
}

type Supervisor struct {
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

func (s *Supervisor) Dispatch(ctx context.Context, request RunRequest, completions chan<- Completion) {
	go func() {
		completion := s.Run(ctx, request)
		select {
		case completions <- completion:
		case <-ctx.Done():
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
	}()

	result, err := s.backend.Run(ctx, request)
	completion.CompletedAt = s.now()
	completion.Result = result
	completion.Err = err
	return completion
}

func (s *Supervisor) RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 30 {
		exponent = 30
	}

	delay := s.failureRetryBaseDelay * time.Duration(math.Pow(2, float64(exponent)))
	if delay > s.maxRetryBackoff {
		return s.maxRetryBackoff
	}
	return delay
}

func nextFailureAttempt(current int) int {
	if current < 0 {
		return 1
	}
	return current + 1
}
