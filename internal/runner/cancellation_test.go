package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestCancellationFirstCause(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		cause  error
		reason string
	}{
		{"operator", ErrOperatorStopped, "operator_stopped"},
		{"lane", ErrLaneRevoked, "lane_revoked"},
		{"merge", ErrMergeRevoked, "merge_revoked"},
		{"CI", ErrCIUnavailable, "ci_unavailable"},
		{"startup", ErrMergeWorkerStartupTimeout, "merge_worker_startup_timeout"},
		{"merge duration", ErrMergeWorkerDurationExceeded, "merge_worker_duration_exceeded"},
		{"fallback", ErrMergeFallbackBudgetExceeded, "merge_fallback_budget_exceeded"},
		{"session duration", ErrSessionDurationExceeded, "session_duration_exceeded"},
		{"turn duration", ErrTurnDurationExceeded, "turn_duration_exceeded"},
		{"memory", ErrSessionMemoryCeilingExceeded, "session_memory_ceiling_exceeded"},
		{"no progress", ErrSessionNoProgress, "session_no_progress"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"shutdown", context.Canceled, "context_cancelled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			first := NewCancellationCause(tt.cause, "test.initiator")
			cancel(first)
			cancel(NewCancellationCause(context.Canceled, "test.cleanup"))
			err := preserveCancellation(ctx, fmt.Errorf("stream turn: %w", context.Canceled), "test.backend")
			err = preserveCancellation(ctx, err, "test.completion")
			if got := firstCancellationCause(err, ErrOperatorStopped, "later.stop"); got != first {
				t.Fatalf("session finalization replaced first cause with %v", got)
			}
			var got *CancellationCause
			if !errors.As(err, &got) || got != first || got.Reason != tt.reason || !errors.Is(err, tt.cause) {
				t.Fatalf("cancellation = %v, want first cause %v", err, first)
			}
			if len(cancellationAttrs(err)) != 4 {
				t.Fatalf("missing cancellation log attributes: %v", cancellationAttrs(err))
			}
		})
	}
}

func TestCancellationDoesNotInventFailureOrExposeCause(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, false},
		{"ordinary error", errors.New("failure"), false},
		{"backend cancellation", context.Canceled, true},
		{"backend deadline", context.DeadlineExceeded, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := preserveCancellation(t.Context(), tt.err, "runner.agent_backend")
			var cause *CancellationCause
			if errors.As(err, &cause) != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("preserveCancellation() = %v", err)
			}
		})
	}
	cause := NewCancellationCause(fmt.Errorf("private token: %w", context.Canceled), "test.source")
	data, err := json.Marshal(cause)
	if err != nil || strings.Contains(string(data)+cause.Error(), "private token") {
		t.Fatalf("cancellation serialization exposed underlying details: %s, %v", data, err)
	}
	if !errors.Is(NewCancellationCause(nil, "test.source"), context.Canceled) {
		t.Fatal("nil cancellation must retain context.Canceled semantics")
	}
	if got := firstCancellationCause(nil, ErrOperatorStopped, "test.source"); got.Reason != "operator_stopped" {
		t.Fatalf("unattributed session cause = %v", got)
	}
}

func TestFinishSessionAfterCancellation(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrOperatorStopped, ErrLaneRevoked} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(cause)
			sessions := &cancellationSessionStore{}
			r := &Runner{store: sessions, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			err := r.finishSession(ctx, 5782, true, 4885, connector.Issue{ID: "issue"}, time.Now(), time.Now(), RunResult{FinalState: FinalStateFailed}, "model", "codex", 1, AgentTurnResult{ThreadID: "thread", SessionID: "session"}, 0)
			if err != nil {
				t.Fatalf("finishSession() = %v", err)
			}
			if !sessions.bounded || sessions.finishCalls != 1 {
				t.Fatalf("session finalization: bounded=%v, writes=%d", sessions.bounded, sessions.finishCalls)
			}
		})
	}
}

func TestCancellationProcessReapAttribution(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		cause error
		want  string
	}{
		{context.Canceled, "context_cancelled:test.source"},
		{context.DeadlineExceeded, "deadline_exceeded:test.source"},
		{ErrOperatorStopped, "operator_stopped:test.source"},
		{ErrLaneRevoked, "lane_revoked:test.source"},
		{ErrSessionDurationExceeded, "maximum_session_lifetime_exceeded"},
		{ErrTurnDurationExceeded, "maximum_turn_lifetime_exceeded"},
		{ErrSessionNoProgress, SessionBrakeReasonNoProgress},
	} {
		t.Run(tt.cause.Error(), func(t *testing.T) {
			t.Parallel()
			cause := NewCancellationCause(tt.cause, "test.source")
			if got := workerProcessReapReason(t.Context(), cause); got != tt.want {
				t.Fatalf("workerProcessReapReason() = %q, want %q", got, tt.want)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(NewCancellationCause(ErrOperatorStopped, "later.stop"))
			if got := workerProcessReapReason(ctx, cause); got != tt.want {
				t.Fatalf("later parent cancellation replaced first reap reason with %q", got)
			}
		})
	}
}

type cancellationSessionStore struct {
	fakeSessionStore
	bounded bool
}

func (s *cancellationSessionStore) FinishSession(ctx context.Context, id int64, attrs store.SessionFinish) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, s.bounded = ctx.Deadline()
	return s.fakeSessionStore.FinishSession(ctx, id, attrs)
}
