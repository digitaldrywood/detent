package runner

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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

func TestSupervisorDispatchDeliversCompletionAfterContextCancellation(t *testing.T) {
	backend := &cancelCompletionBackend{
		returning: make(chan struct{}),
		release:   make(chan struct{}),
	}
	supervisor, err := NewSupervisor(backend, SupervisorConfig{
		FailureRetryBaseDelay: time.Second,
		MaxRetryBackoff:       time.Minute,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	completions := make(chan Completion)
	supervisor.Dispatch(ctx, RunRequest{
		Issue: connector.Issue{ID: "issue-22", Identifier: "digitaldrywood/detent#22"},
	}, completions)

	cancel()
	select {
	case <-backend.returning:
	case <-time.After(time.Second):
		t.Fatal("backend did not observe cancellation")
	}
	close(backend.release)

	select {
	case completion := <-completions:
		if !errors.Is(completion.Err, context.Canceled) {
			t.Fatalf("Completion.Err = %v, want context.Canceled", completion.Err)
		}
		if !completion.Retryable {
			t.Fatal("Retryable = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled completion")
	}
}

func TestSupervisorDispatchStopsBlockedSendAfterCancellation(t *testing.T) {
	t.Parallel()

	returned := make(chan struct{})
	logs := newSignalLog("runner completion delivery timed out after context cancellation")
	supervisor, err := NewSupervisor(staticBackend{
		result:   RunResult{FinalState: FinalStateCompleted},
		returned: returned,
	}, SupervisorConfig{
		FailureRetryBaseDelay: time.Second,
		MaxRetryBackoff:       time.Minute,
		Logger:                slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	supervisor.Dispatch(ctx, RunRequest{
		Issue: connector.Issue{ID: "issue-22", Identifier: "digitaldrywood/detent#22"},
	}, make(chan Completion))

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("backend did not return")
	}
	cancel()

	select {
	case <-logs.signal:
	case <-time.After(time.Second):
		t.Fatalf("logs %q missing blocked completion timeout", logs.String())
	}
}

type signalLog struct {
	mu       sync.Mutex
	body     strings.Builder
	fragment string
	signal   chan struct{}
	once     sync.Once
}

func newSignalLog(fragment string) *signalLog {
	return &signalLog{
		fragment: fragment,
		signal:   make(chan struct{}),
	}
}

func (l *signalLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, _ = l.body.Write(p)
	if strings.Contains(l.body.String(), l.fragment) {
		l.once.Do(func() {
			close(l.signal)
		})
	}
	return len(p), nil
}

func (l *signalLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.body.String()
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
	result   RunResult
	returned chan struct{}
}

func (b staticBackend) Run(context.Context, RunRequest) (RunResult, error) {
	if b.returned != nil {
		close(b.returned)
	}
	return b.result, nil
}

type cancelCompletionBackend struct {
	returning chan struct{}
	release   chan struct{}
}

func (b *cancelCompletionBackend) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	<-ctx.Done()
	close(b.returning)
	<-b.release
	return RunResult{}, ctx.Err()
}
