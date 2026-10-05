package hubclient

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectSetupFailurePrecedesNativeClaim(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	issue, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "setup failure must not claim this", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	failing := true
	h.scheduler.prepareProject = func(_ context.Context, project string) error {
		if project == "local" && failing {
			return errors.New("setup exited 7")
		}
		return nil
	}
	h.scheduler.localChecks = map[string]runnerauth.LocalChecks{
		"local": {Checkout: "passed", Doctor: "passed", Provider: "passed"},
		"other": {Checkout: "passed", Doctor: "passed", Provider: "passed"},
	}
	request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}}
	for range 2 {
		candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
		if !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(candidates) != 0 || len(h.scheduler.nativeClaims) != 0 {
			t.Fatalf("failed setup candidates = %v, error = %v, claims = %v", candidates, err, h.scheduler.nativeClaims)
		}
		current, err := h.admin.Issue(t.Context(), issue.WorkItemID)
		if err != nil || current.State != "Todo" || current.Revision != issue.Revision {
			t.Fatalf("issue changed by instance failure: %+v, error = %v", current, err)
		}
	}
	if h.scheduler.localChecks["local"].Setup != "failed" {
		t.Fatal("failed project setup was not reported")
	}
	if err := h.scheduler.PrepareProject(t.Context(), "other"); err != nil || !h.scheduler.localChecks["other"].Passed() {
		t.Fatalf("other project was blocked: %v", err)
	}
	failing = false
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].ID != string(issue.WorkItemID) || h.scheduler.localChecks["local"].Setup != "passed" {
		t.Fatalf("repaired setup candidates = %v, error = %v", candidates, err)
	}
}
