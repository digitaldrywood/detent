package scenariotest

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
)

func (h *nativeChangeHub) candidatesIn(t *testing.T, states ...string) []connector.Issue {
	return CandidatesIn(t, h.scheduler, h.descriptor, states...)
}
func CandidatesIn(t *testing.T, scheduler *Scheduler, descriptor policy.Descriptor, states ...string) []connector.Issue {
	t.Helper()
	scheduler.ExecutionSource("local").NativeClient().ApplyHeartbeatChanges(nil)
	capacity := 0
	if descriptor.Gates.GitHubPullRequest {
		capacity = 1
	}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{LandingCapacity: capacity, Policy: descriptor, ProjectID: "local", WorkflowStates: states})
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}
