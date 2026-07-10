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

type capacityClassifierBackend struct{}

func (capacityClassifierBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	return AgentTurnResult{}, nil
}

func (capacityClassifierBackend) ClassifyCapacityError(error, *telemetry.RateLimits, time.Time) (backendcapacity.Details, bool) {
	return backendcapacity.Details{Kind: "usageLimitExceeded", Reason: "provider usage limit reached"}, true
}
