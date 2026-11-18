package scenariotest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Constructors struct {
	New       executioncontract.ExecutionFactory
	Recovered func(executioncontract.ExecutionHost, context.Context, executioncontract.ExecutionClaim, checkpoint.State) (executioncontract.Execution, error)
}
type Checks struct {
	DirtyArtifacts func(*testing.T, runner.Execution) bool
}
type ownedExecution struct {
	Lifecycle
	claim nativeClaim
}
type nativeClaim struct {
	source   *NativeConnector
	lease    tracker.NativeLease
	recovery tracker.NativeRecovery
}
type nativeCompletionState = checkpoint.State

func wrap(actual executioncontract.Execution, view executioncontract.ExecutionClaim) *ownedExecution {
	return &ownedExecution{Lifecycle: actual.(Lifecycle), claim: nativeClaim{source: view.Source, lease: view.Lease, recovery: view.Recovery}}
}
func Underlying(execution runner.Execution) runner.Execution {
	if owner, ok := execution.(*ownedExecution); ok {
		return owner.Lifecycle
	}
	return execution
}
func (e *ownedExecution) Settled() bool {
	state := e.CompletionSnapshot()
	return state.Settled
}
func sameCompletionLease(current, original tracker.NativeLease) bool {
	return checkpoint.SameLease(current, original)
}
func replaceRecovery(t *testing.T, h *nativeChangeHub, execution runner.Execution, delivery *tracker.ReleaseDelivery, constructors Constructors) runner.Execution {
	t.Helper()
	item := string(execution.Recovery().Lease.WorkItemID)
	view, ok := h.scheduler.ExecutionClaim(item)
	if !ok {
		t.Fatal("fixture lost its claim")
	}
	view.Recovery.Issue.ReleaseDelivery = delivery
	saved, ok := schedulerFixtures.Load(h.scheduler)
	if !ok {
		t.Fatal("fixture lost scheduler configuration")
	}
	config := saved.(*schedulerFixture).config
	replacementHost, err := newTestScheduler(h.scheduler.Transport(), config)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler = replacementHost
	view.Source = h.scheduler.ExecutionSource("local")
	replacement := wrap(constructors.New(h.scheduler, view), view)
	if err := h.scheduler.RegisterExecution(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor}, connector.Issue{ID: item}, view, replacement); err != nil {
		t.Fatal(err)
	}
	return replacement
}

var _ dispatch.Execution = (*ownedExecution)(nil)
var _ interface{ CompletionState() json.RawMessage } = (*ownedExecution)(nil)

func replaceSourceRecovery(t *testing.T, h *nativeChangeHub, original *ownedExecution, recovery tracker.NativeRecovery, factories Constructors) *ownedExecution {
	t.Helper()
	item := string(original.claim.lease.WorkItemID)
	view, ok := h.scheduler.ExecutionClaim(item)
	if !ok {
		t.Fatal("fixture lost its claim")
	}
	view.Recovery = recovery
	saved, ok := schedulerFixtures.Load(h.scheduler)
	if !ok {
		t.Fatal("fixture lost scheduler configuration")
	}
	replacementHost, err := newTestScheduler(h.scheduler.Transport(), saved.(*schedulerFixture).config)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler = replacementHost
	view.Source = h.scheduler.ExecutionSource("local")
	actual, err := factories.Recovered(h.scheduler, t.Context(), view, original.CompletionSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	replacement := wrap(actual, view)
	if err := h.scheduler.RegisterExecution(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor}, connector.Issue{ID: item}, view, replacement); err != nil {
		t.Fatal(err)
	}
	return replacement
}
