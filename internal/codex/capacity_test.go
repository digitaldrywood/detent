package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAgentBackendClassifyCapacityError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	tests := []struct {
		name     string
		err      error
		limits   *telemetry.RateLimits
		want     bool
		wantType backendcapacity.ErrorType
		wantKind string
	}{
		{
			name: "usage limit payload",
			err:  &TurnFailedError{Status: "failed", Body: `{"error":{"type":"usageLimitExceeded"}}`},
			limits: &telemetry.RateLimits{
				Primary: &telemetry.RateLimitBucket{ResetAt: &resetAt},
			},
			want:     true,
			wantType: backendcapacity.ErrorTypeUsageLimit,
		},
		{
			name: "recorded subscription exhaustion payload",
			err: &TurnFailedError{
				Status: "failed",
				Body:   `{"message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 20th, 2026 10:27 AM.","codexErrorInfo":"usageLimitExceeded","additionalDetails":null}`,
			},
			want:     true,
			wantType: backendcapacity.ErrorTypeUsageLimit,
		},
		{
			name: "google resource exhausted without reset",
			err:  &TurnFailedError{Status: "failed", Body: `{"error":{"status":"RESOURCE_EXHAUSTED"}}`},
		},
		{
			name:     "server overloaded payload",
			err:      &TurnFailedError{Status: "failed", Body: `{"message":"Selected model is at capacity","codexErrorInfo":"serverOverloaded"}`},
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
		},
		{
			name:     "provider http 529",
			err:      &TurnFailedError{Status: "failed", Body: `{"status_code":529,"message":"retry later"}`},
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
		},
		{
			name:     "provider http 503",
			err:      &TurnFailedError{Status: "failed", Body: `{"status":503,"message":"retry later"}`},
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
		},
		{
			name:     "thread start handshake timeout",
			err:      fmt.Errorf("run agent turn: wait for thread/start response: %w", context.DeadlineExceeded),
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
			wantKind: backendcapacity.StartupTimeoutKind,
		},
		{
			name:     "initialize process exit",
			err:      fmt.Errorf("run agent turn: wait for initialize response: %w: codex app-server process exited (exit status 23): stderr: broken environment", io.EOF),
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
			wantKind: backendcapacity.StartupFailureKind,
		},
		{
			name:     "transport spawn failure",
			err:      errors.New("run agent turn: start codex app-server transport: start command: executable file not found"),
			want:     true,
			wantType: backendcapacity.ErrorTypeTransientOverload,
			wantKind: backendcapacity.StartupFailureKind,
		},
		{
			name: "canceled transport spawn",
			err:  fmt.Errorf("run agent turn: start codex app-server transport: start command: %w", context.Canceled),
		},
		{
			name: "unrelated deadline",
			err:  fmt.Errorf("run agent turn: wait for output: %w", context.DeadlineExceeded),
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
			if tt.want && details.Type != tt.wantType {
				t.Fatalf("ClassifyCapacityError() Type = %q, want %q", details.Type, tt.wantType)
			}
			if tt.wantKind != "" && details.Kind != tt.wantKind {
				t.Fatalf("ClassifyCapacityError() Kind = %q, want %q", details.Kind, tt.wantKind)
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

func TestAgentBackendClassifyCapacityErrorKeepsOverloadTransientWithRateLimitTelemetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 20, 34, 0, 0, time.UTC)
	resetAt := now.Add(12 * time.Hour)
	details, ok := (&AgentBackend{}).ClassifyCapacityError(
		&TurnFailedError{Status: "failed", Body: `{"message":"Selected model is at capacity","codexErrorInfo":"serverOverloaded"}`},
		&telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{
			Status:  telemetry.RateLimitStatusExhausted,
			ResetAt: &resetAt,
		}},
		now,
	)
	if !ok || details.Type != backendcapacity.ErrorTypeTransientOverload || details.ResetAt != nil {
		t.Fatalf("ClassifyCapacityError() = %#v, %v, want reset-free transient overload", details, ok)
	}
}

func TestCapacityStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		limits        *telemetry.RateLimits
		wantReported  bool
		wantAvailable bool
		wantExhausted bool
	}{
		{name: "missing status"},
		{
			name: "both windows recovered",
			limits: &telemetry.RateLimits{
				Primary:   &telemetry.RateLimitBucket{Limit: 100, Remaining: 20},
				Secondary: &telemetry.RateLimitBucket{Limit: 100, Remaining: 50},
			},
			wantReported:  true,
			wantAvailable: true,
		},
		{
			name: "rolling window below threshold",
			limits: &telemetry.RateLimits{
				Primary:   &telemetry.RateLimitBucket{Limit: 100, Remaining: 4},
				Secondary: &telemetry.RateLimitBucket{Limit: 100, Remaining: 50},
			},
			wantReported: true,
		},
		{
			name: "provider still reports reached window",
			limits: &telemetry.RateLimits{
				ReachedType: "primary",
				Primary:     &telemetry.RateLimitBucket{Limit: 100, Remaining: 20},
			},
			wantReported:  true,
			wantExhausted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, reported := CapacityStatus(tt.limits)
			if reported != tt.wantReported || status.Available != tt.wantAvailable || status.Exhausted != tt.wantExhausted {
				t.Fatalf("CapacityStatus() = %#v, %v, want available %v exhausted %v reported %v", status, reported, tt.wantAvailable, tt.wantExhausted, tt.wantReported)
			}
		})
	}
}

func TestAgentBackendClassifyCapacityErrorParsesResetFromBackendBody(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	details, ok := (&AgentBackend{}).ClassifyCapacityError(
		&TurnFailedError{
			Status: "failed",
			Body:   `{"error":{"type":"usageLimitExceeded"},"resetAt":1783651140}`,
		},
		nil,
		now,
	)
	if !ok {
		t.Fatal("ClassifyCapacityError() ok = false, want true")
	}
	if details.ResetAt == nil || !details.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %v, want %s", details.ResetAt, resetAt)
	}
}

func TestCodexCapacityErrorTextDoesNotDuplicateCarrierBody(t *testing.T) {
	t.Parallel()

	body := `{"error":{"type":"usageLimitExceeded"},"resetAt":1783651140}`
	text := codexCapacityErrorText(&TurnFailedError{Status: "failed", Body: body})
	if count := strings.Count(text, body); count != 1 {
		t.Fatalf("backend body appears %d times in %q, want once", count, text)
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
	if limits.Primary.ObservedAt == nil {
		t.Fatalf("Primary.ObservedAt = nil, want observation timestamp")
	}
}
