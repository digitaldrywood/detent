package orchestrator

import (
	"testing"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func newTestSupervisor(t *testing.T, backend Runner, cfg Config) *runpkg.Supervisor {
	t.Helper()

	supervisor, err := runpkg.NewSupervisor(backend, runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	return supervisor
}
