package codex

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAgentBackendClassifyCapacityError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	tests := []struct {
		name   string
		err    error
		limits *telemetry.RateLimits
		want   bool
	}{
		{
			name: "usage limit payload",
			err:  &TurnFailedError{Status: "failed", Body: `{"error":{"type":"usageLimitExceeded"}}`},
			limits: &telemetry.RateLimits{
				Primary: &telemetry.RateLimitBucket{ResetAt: &resetAt},
			},
			want: true,
		},
		{
			name: "google resource exhausted payload",
			err:  &TurnFailedError{Status: "failed", Body: `{"error":{"status":"RESOURCE_EXHAUSTED"}}`},
			want: true,
		},
		{
			name: "invalid request",
			err:  &TurnFailedError{Status: "failed", Body: `{"error":{"type":"invalid_request_error"}}`},
		},
	}

	backend := &AgentBackend{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			details, ok := backend.ClassifyCapacityError(tt.err, tt.limits, now)
			if ok != tt.want {
				t.Fatalf("ClassifyCapacityError() ok = %v, want %v", ok, tt.want)
			}
			if tt.limits != nil && (details.ResetAt == nil || !details.ResetAt.Equal(resetAt)) {
				t.Fatalf("ClassifyCapacityError() ResetAt = %v, want %v", details.ResetAt, resetAt)
			}
		})
	}
}

func TestAgentBackendClassifyCapacityErrorUsesReachedWindowReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	primaryReset := now.Add(44 * time.Minute)
	secondaryReset := now.Add(24 * time.Hour)
	limits := &telemetry.RateLimits{
		ReachedType: "secondary",
		Primary: &telemetry.RateLimitBucket{
			Status:  telemetry.RateLimitStatusUnknown,
			ResetAt: &primaryReset,
		},
		Secondary: &telemetry.RateLimitBucket{
			Status:  telemetry.RateLimitStatusExhausted,
			ResetAt: &secondaryReset,
		},
	}

	details, ok := (&AgentBackend{}).ClassifyCapacityError(
		&TurnFailedError{Status: "failed", Body: `{"error":{"type":"usageLimitExceeded"}}`},
		limits,
		now,
	)
	if !ok {
		t.Fatal("ClassifyCapacityError() ok = false, want true")
	}
	if details.ResetAt == nil || !details.ResetAt.Equal(secondaryReset) {
		t.Fatalf("ResetAt = %v, want secondary reset %s", details.ResetAt, secondaryReset)
	}
}

func TestRateLimitsFromCodexPreservesReachedType(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC).Unix()
	limits := rateLimitsFromCodex(&RateLimitSnapshot{
		RateLimitReachedType: "primary",
		Primary:              &RateLimitWindow{UsedPercent: 100, ResetsAt: &resetAt},
	})
	if limits == nil || limits.ReachedType != "primary" {
		t.Fatalf("RateLimits = %#v, want reached type", limits)
	}
	if limits.Primary == nil || limits.Primary.Status != telemetry.RateLimitStatusExhausted {
		t.Fatalf("Primary = %#v, want exhausted", limits.Primary)
	}
}
