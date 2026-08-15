package claudecode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAgentBackendClassifyCapacityError(t *testing.T) {
	t.Parallel()

	backend := &AgentBackend{}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "anthropic rate limit without reset", err: errors.New("claude turn failed: rate_limit_error")},
		{name: "vertex quota without reset", err: errors.New("claude turn failed: RESOURCE_EXHAUSTED")},
		{name: "anthropic overload", err: errors.New("claude turn failed: overloaded_error"), want: true},
		{name: "http overload", err: errors.New("claude turn failed: HTTP 529"), want: true},
		{name: "max turns", err: errors.New("claude turn failed: error_max_turns")},
		{name: "result message", err: finalTurnError(context.Background(), turnState{sawResult: true, resultIsError: true, resultSubtype: "error_during_execution", resultText: "You've hit your limit. Try again at 9:39 PM"}, nil, ""), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ok := backend.ClassifyCapacityError(tt.err, nil, time.Now())
			if ok != tt.want {
				t.Fatalf("ClassifyCapacityError() ok = %v, want %v", ok, tt.want)
			}
		})
	}
}

func TestClassifyCapacityErrorProductionSessionLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 15, 20, 47, 49, 0, time.UTC)
	details, ok := ClassifyCapacityError(
		errors.New("You've hit your session limit · resets 4:10pm (America/Chicago)"),
		nil,
		now,
	)
	if !ok {
		t.Fatal("ClassifyCapacityError() ok = false, want true")
	}
	want := time.Date(2026, 8, 15, 21, 10, 0, 0, time.UTC)
	if details.Type != backendcapacity.ErrorTypeUsageLimit || details.ResetAt == nil || !details.ResetAt.Equal(want) {
		t.Fatalf("ClassifyCapacityError() = %#v, want usage limit reset at %s", details, want)
	}
}

func TestClaudeCapacityResetAtUsesReachedWindow(t *testing.T) {
	t.Parallel()

	primaryReset := time.Date(2026, 8, 15, 21, 10, 0, 0, time.UTC)
	secondaryReset := time.Date(2026, 8, 22, 21, 10, 0, 0, time.UTC)
	tests := []struct {
		name            string
		reachedType     string
		primaryStatus   string
		secondaryStatus string
		want            time.Time
	}{
		{
			name:            "five-hour window",
			reachedType:     "five_hour",
			primaryStatus:   telemetry.RateLimitStatusExhausted,
			secondaryStatus: telemetry.RateLimitStatusUnknown,
			want:            primaryReset,
		},
		{
			name:            "weekly window",
			reachedType:     "seven_day",
			primaryStatus:   telemetry.RateLimitStatusUnknown,
			secondaryStatus: telemetry.RateLimitStatusExhausted,
			want:            secondaryReset,
		},
		{name: "missing reached type", want: primaryReset},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := claudeCapacityResetAt(&telemetry.RateLimits{
				ReachedType: tt.reachedType,
				Primary: &telemetry.RateLimitBucket{
					Status:  tt.primaryStatus,
					ResetAt: &primaryReset,
				},
				Secondary: &telemetry.RateLimitBucket{
					Status:  tt.secondaryStatus,
					ResetAt: &secondaryReset,
				},
			})
			if got == nil || !got.Equal(tt.want) {
				t.Fatalf("claudeCapacityResetAt() = %v, want %s", got, tt.want)
			}
		})
	}
}
