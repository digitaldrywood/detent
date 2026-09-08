package runner

import (
	"context"
	"errors"
	"fmt"
)

type CancellationCause struct {
	Reason string `json:"reason"`
	Source string `json:"source"`
	err    error
}

func (c *CancellationCause) Error() string {
	return fmt.Sprintf("worker cancellation: %s (source: %s)", c.Reason, c.Source)
}

func (c *CancellationCause) Unwrap() error {
	return c.err
}

func NewCancellationCause(cause error, source string) *CancellationCause {
	var first *CancellationCause
	if errors.As(cause, &first) {
		return first
	}
	reason := "unclassified_cancellation"
	if cause == nil || errors.Is(cause, context.Canceled) {
		reason = "context_cancelled"
	}
	for _, candidate := range []struct {
		err    error
		reason string
	}{
		{ErrOperatorStopped, "operator_stopped"},
		{ErrLaneRevoked, "lane_revoked"},
		{ErrMergeRevoked, "merge_revoked"},
		{ErrCIUnavailable, "ci_unavailable"},
		{ErrMergeWorkerStartupTimeout, "merge_worker_startup_timeout"},
		{ErrMergeWorkerDurationExceeded, "merge_worker_duration_exceeded"},
		{ErrMergeFallbackBudgetExceeded, "merge_fallback_budget_exceeded"},
		{ErrSessionDurationExceeded, "session_duration_exceeded"},
		{ErrTurnDurationExceeded, "turn_duration_exceeded"},
		{ErrSessionMemoryCeilingExceeded, "session_memory_ceiling_exceeded"},
		{ErrSessionNoProgress, "session_no_progress"},
		{context.DeadlineExceeded, "deadline_exceeded"},
	} {
		if errors.Is(cause, candidate.err) {
			reason = candidate.reason
			break
		}
	}
	if cause == nil {
		cause = context.Canceled
	}
	return &CancellationCause{Reason: reason, Source: source, err: cause}
}

func preserveCancellation(ctx context.Context, err error, source string) error {
	if err == nil {
		return nil
	}
	var first *CancellationCause
	if errors.As(err, &first) {
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(NewCancellationCause(cause, source), err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(NewCancellationCause(err, source), err)
	}
	return err
}

func cancellationAttrs(err error) []any {
	var cause *CancellationCause
	if !errors.As(err, &cause) {
		return nil
	}
	return []any{"cancellation_reason", cause.Reason, "cancellation_source", cause.Source}
}

func firstCancellationCause(err error, cause error, source string) *CancellationCause {
	var first *CancellationCause
	if errors.As(err, &first) {
		return first
	}
	return NewCancellationCause(cause, source)
}
