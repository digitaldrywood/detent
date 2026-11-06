package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runner"
)

type fixtureLifecycle interface {
	Execution
	runner.WorkerLifetimeExecution
	runner.HostDiagnosticsExecution
	runner.PipelineExecution
	runner.DiffExecution
	runner.RepositoryExecution
	runner.PublicationSourceExecution
	runner.CompletionExecution
	runner.LandingExecution
	CompletionState() json.RawMessage
}

func fixtureCompletion(t testing.TB, execution runner.Execution) checkpoint.State {
	t.Helper()
	var state checkpoint.State
	if err := json.Unmarshal(execution.(interface{ CompletionState() json.RawMessage }).CompletionState(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func fixtureRecoverExecution(t testing.TB, scheduler *Scheduler, item string, edit func(*checkpoint.State)) fixtureLifecycle {
	t.Helper()
	original := scheduler.RunExecution(item)
	state := fixtureCompletion(t, original)
	edit(&state)
	view, _ := scheduler.ExecutionClaim(item)
	execution, err := nativechange.NewRecoveredExecution(scheduler, t.Context(), view, state)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.mu.Lock()
	claim := scheduler.nativeClaims[item]
	claim.execution = execution
	scheduler.nativeClaims[item] = claim
	scheduler.mu.Unlock()
	return execution.(fixtureLifecycle)
}

type landedFixture struct{ Execution }

func (e landedFixture) ClaimState(ctx context.Context) (ExecutionState, error) {
	state, err := e.Execution.ClaimState(ctx)
	state.Landed = true
	return state, err
}
func (e landedFixture) LandedStatus() (bool, bool) { return true, true }
