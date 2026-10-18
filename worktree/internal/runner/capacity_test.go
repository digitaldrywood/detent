package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestClassifyAgentCapacityErrorSkipsLocalProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "hosted provider", provider: "openai", want: true},
		{name: "local ollama provider", provider: "local_ollama"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := classifyAgentCapacityError(
				capacityClassifierBackend{},
				RouteSelection{BackendID: "codex"},
				config.AgentBackend{ID: "codex", Kind: config.AgentBackendCodex, Provider: tt.provider},
				agentidentity.Identity{},
				errors.New("usage limit reached"),
				nil,
				time.Now(),
			)
			if got := IsCapacityError(err); got != tt.want {
				t.Fatalf("IsCapacityError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyAgentCapacityErrorUsesExhaustedProviderStatus(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 8, 20, 15, 27, 0, 0, time.UTC)
	tests := []struct {
		name       string
		runErr     error
		status     CapacityStatus
		wantErr    bool
		wantReason string
	}{
		{
			name: "clean completion with exhausted subscription window",
			status: CapacityStatus{
				Exhausted: true,
				Detail:    "live provider status reports an exhausted subscription window",
				Details: backendcapacity.Details{
					Type:    backendcapacity.ErrorTypeUsageLimit,
					Kind:    "subscription_window_exhausted",
					Reason:  "subscription window exhausted",
					ResetAt: &resetAt,
				},
			},
			wantErr:    true,
			wantReason: "subscription window exhausted",
		},
		{
			name:   "missing result with exhausted subscription window",
			runErr: errors.New("agent result event missing"),
			status: CapacityStatus{
				Exhausted: true,
				Details: backendcapacity.Details{
					ResetAt: &resetAt,
				},
			},
			wantErr:    true,
			wantReason: "subscription window exhausted",
		},
		{
			name:   "ordinary failure without exact exhaustion",
			runErr: errors.New("agent result event missing"),
			status: CapacityStatus{Detail: "live provider status reports 4% capacity remaining"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyAgentCapacityError(
				capacityStatusBackend{status: tt.status},
				RouteSelection{BackendID: "codex"},
				config.AgentBackend{ID: "codex", Kind: config.AgentBackendCodex, Provider: "openai"},
				agentidentity.Identity{},
				tt.runErr,
				&telemetry.RateLimits{},
				time.Now(),
			)
			capacityErr, ok := backendcapacity.As(got)
			if ok != tt.wantErr {
				t.Fatalf("backendcapacity.As() ok = %v, want %v; error = %v", ok, tt.wantErr, got)
			}
			if !tt.wantErr {
				if !errors.Is(got, tt.runErr) {
					t.Fatalf("error = %v, want original %v", got, tt.runErr)
				}
				return
			}
			if capacityErr.Details.Reason != tt.wantReason || capacityErr.Details.ResetAt == nil || !capacityErr.Details.ResetAt.Equal(resetAt) {
				t.Fatalf("capacity error = %#v, want reason %q reset %s", capacityErr, tt.wantReason, resetAt)
			}
		})
	}
}

type capacityClassifierBackend struct{}

func (capacityClassifierBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, nil
}

func (capacityClassifierBackend) ClassifyCapacityError(error, *telemetry.RateLimits, time.Time) (backendcapacity.Details, bool) {
	return backendcapacity.Details{Kind: "usageLimitExceeded", Reason: "provider usage limit reached"}, true
}

type capacityStatusBackend struct {
	status CapacityStatus
}

func (capacityStatusBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, nil
}

func (b capacityStatusBackend) CapacityStatus(*telemetry.RateLimits) (CapacityStatus, bool) {
	return b.status, true
}
