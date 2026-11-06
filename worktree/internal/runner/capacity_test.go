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
		name      string
		provider  string
		want      bool
		errorType backendcapacity.ErrorType
	}{
		{name: "hosted provider outage", provider: "openai", want: true, errorType: backendcapacity.ErrorTypeProviderOutage},
		{name: "local ollama provider", provider: "local_ollama", errorType: backendcapacity.ErrorTypeProviderOutage},
		{name: "quota response is not a capacity constraint", provider: "openai", errorType: backendcapacity.ErrorTypeUsageLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := classifyAgentCapacityError(
				capacityClassifierBackend{errorType: tt.errorType},
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

func TestClassifyAgentCapacityErrorIgnoresProviderUsage(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 8, 20, 15, 27, 0, 0, time.UTC)
	tests := []struct {
		name   string
		runErr error
		status CapacityStatus
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
			if IsCapacityError(got) || !errors.Is(got, tt.runErr) {
				t.Fatalf("error = %v, want original non-capacity result %v", got, tt.runErr)
			}
		})
	}
}

type capacityClassifierBackend struct {
	errorType backendcapacity.ErrorType
}

func (capacityClassifierBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, nil
}

func (b capacityClassifierBackend) ClassifyCapacityError(error, *telemetry.RateLimits, time.Time) (backendcapacity.Details, bool) {
	return backendcapacity.Details{Type: b.errorType}, true
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
