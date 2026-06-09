package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestSupervisorConvertsPanicToRetryableCompletion(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 5, 31, 14, 0, 0, 0, time.UTC)
	supervisor, err := NewSupervisor(panicBackend{}, SupervisorConfig{
		FailureRetryBaseDelay: 10 * time.Second,
		MaxRetryBackoff:       time.Minute,
		Now:                   func() time.Time { return completedAt },
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	completion := supervisor.Run(context.Background(), RunRequest{
		Issue: connector.Issue{ID: "issue-22", Identifier: "digitaldrywood/detent#22"},
	})

	if completion.IssueID != "issue-22" {
		t.Fatalf("IssueID = %q, want issue-22", completion.IssueID)
	}
	if completion.Err == nil || !strings.Contains(completion.Err.Error(), "runner panic: boom") {
		t.Fatalf("Err = %v, want runner panic", completion.Err)
	}
	if !completion.Retryable {
		t.Fatal("Retryable = false, want true")
	}
	if completion.RetryAttempt != 1 {
		t.Fatalf("RetryAttempt = %d, want 1", completion.RetryAttempt)
	}
	if completion.RetryDelay != 10*time.Second {
		t.Fatalf("RetryDelay = %s, want 10s", completion.RetryDelay)
	}
	if !completion.CompletedAt.Equal(completedAt) {
		t.Fatalf("CompletedAt = %v, want %v", completion.CompletedAt, completedAt)
	}
}

func TestSupervisorAppliesCappedBackoffForRunnerErrors(t *testing.T) {
	t.Parallel()

	supervisor, err := NewSupervisor(errorBackend{}, SupervisorConfig{
		FailureRetryBaseDelay: 10 * time.Second,
		MaxRetryBackoff:       25 * time.Second,
		Now:                   func() time.Time { return time.Date(2026, 5, 31, 14, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	completion := supervisor.Run(context.Background(), RunRequest{
		Issue:   connector.Issue{ID: "issue-22", Identifier: "digitaldrywood/detent#22"},
		Attempt: 2,
	})

	if completion.Err == nil || !strings.Contains(completion.Err.Error(), "runner failed") {
		t.Fatalf("Err = %v, want runner failed", completion.Err)
	}
	if completion.RetryAttempt != 3 {
		t.Fatalf("RetryAttempt = %d, want 3", completion.RetryAttempt)
	}
	if completion.RetryDelay != 25*time.Second {
		t.Fatalf("RetryDelay = %s, want capped 25s", completion.RetryDelay)
	}
}

func TestSupervisorRetryDelayNeverOverflows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
		attempt   int
		want      time.Duration
	}{
		{
			name:      "first attempt uses base delay",
			baseDelay: 10 * time.Second,
			maxDelay:  5 * time.Minute,
			attempt:   1,
			want:      10 * time.Second,
		},
		{
			name:      "negative attempt uses first attempt",
			baseDelay: 10 * time.Second,
			maxDelay:  5 * time.Minute,
			attempt:   -10,
			want:      10 * time.Second,
		},
		{
			name:      "caps before default backoff can overflow",
			baseDelay: 10 * time.Second,
			maxDelay:  5 * time.Minute,
			attempt:   31,
			want:      5 * time.Minute,
		},
		{
			name:      "large attempt stays capped",
			baseDelay: time.Nanosecond,
			maxDelay:  time.Duration(1<<63 - 1),
			attempt:   int(^uint(0) >> 1),
			want:      time.Duration(1<<63 - 1),
		},
		{
			name:      "near max duration multiplication caps",
			baseDelay: time.Duration(1<<62 + 1),
			maxDelay:  time.Duration(1<<63 - 1),
			attempt:   2,
			want:      time.Duration(1<<63 - 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			supervisor, err := NewSupervisor(errorBackend{}, SupervisorConfig{
				FailureRetryBaseDelay: tt.baseDelay,
				MaxRetryBackoff:       tt.maxDelay,
			})
			if err != nil {
				t.Fatalf("NewSupervisor() error = %v", err)
			}

			got := supervisor.RetryDelay(tt.attempt)
			if got != tt.want {
				t.Fatalf("RetryDelay(%d) = %s, want %s", tt.attempt, got, tt.want)
			}
			if got < 0 {
				t.Fatalf("RetryDelay(%d) = %s, want non-negative duration", tt.attempt, got)
			}
		})
	}
}

func TestSupervisorUpdateConfigChangesRetryDelay(t *testing.T) {
	t.Parallel()

	supervisor, err := NewSupervisor(errorBackend{}, SupervisorConfig{
		FailureRetryBaseDelay: 10 * time.Second,
		MaxRetryBackoff:       25 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	supervisor.UpdateConfig(SupervisorConfig{
		FailureRetryBaseDelay: time.Second,
		MaxRetryBackoff:       2 * time.Second,
	})

	if got := supervisor.RetryDelay(4); got != 2*time.Second {
		t.Fatalf("RetryDelay(4) = %s, want 2s", got)
	}
}

func TestSupervisorDispatchSendsCompletion(t *testing.T) {
	t.Parallel()

	supervisor, err := NewSupervisor(staticBackend{
		result: RunResult{FinalState: FinalStateCompleted},
	}, SupervisorConfig{
		FailureRetryBaseDelay: time.Second,
		MaxRetryBackoff:       time.Minute,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	completions := make(chan Completion, 1)
	supervisor.Dispatch(context.Background(), RunRequest{
		Issue: connector.Issue{ID: "issue-22", Identifier: "digitaldrywood/detent#22"},
	}, completions)

	select {
	case completion := <-completions:
		if completion.Err != nil {
			t.Fatalf("Completion.Err = %v, want nil", completion.Err)
		}
		if completion.Retryable {
			t.Fatal("Retryable = true, want false")
		}
		if completion.Result.FinalState != FinalStateCompleted {
			t.Fatalf("FinalState = %q, want %q", completion.Result.FinalState, FinalStateCompleted)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for completion")
	}
}

type panicBackend struct{}

func (panicBackend) Run(context.Context, RunRequest) (RunResult, error) {
	panic("boom")
}

type errorBackend struct{}

func (errorBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, errors.New("runner failed")
}

type staticBackend struct {
	result RunResult
}

func (b staticBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return b.result, nil
}
