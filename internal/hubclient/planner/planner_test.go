package planner_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativePlannerAutomaticHandoff(t *testing.T) {
	scenariotest.RunPlannerAutomaticHandoff(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerDisposition(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "blocked native729 first unchanged Rework preserves dependency", FinalMessage: scenariotest.DependencyMessage, WantDisposition: scenariotest.DependencyDisposition(), Rework: true},
		{Name: "blocked native825 unchanged Rework preserves instance gate blocker", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), WantReworkCandidate: true, Rework: true},
		{Name: "blocked SSH Rework retains staged source", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), WantReworkCandidate: true, Rework: true, SSH: true, Staged: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
func TestPlanAdmissionContention(t *testing.T) {
	code := providercapacity.Requirement{Role: runner.RoleCode, Backend: "coder", Model: "test"}
	plan := providercapacity.Requirement{Role: runner.RolePlan, Backend: "planner", Model: "test"}
	for _, occupied := range []providercapacity.Requirement{code, plan} {
		t.Run(occupied.Role+" account busy", func(t *testing.T) {
			t.Parallel()
			reports := func() ([]providercapacity.Report, error) {
				now := time.Now().UTC()
				return []providercapacity.Report{
					{Provider: "openai", Backend: code.Backend, AccountAlias: "code", SharedAccountAlias: "code", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
					{Provider: "openai", Backend: plan.Backend, AccountAlias: "plan", SharedAccountAlias: "plan", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
				}, nil
			}
			descriptor, local, other, create := scenariotest.OpenPlannerAdmission(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, reports)
			resolve := func(requirement providercapacity.Requirement) func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
				return func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					return requirement, nil
				}
			}
			blocker := create("Hold provider capacity")
			request := orchestrator.SchedulingRequest{Policy: descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}, ProviderRequirement: resolve(occupied)}
			claimed, err := other.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(claimed) != 1 || claimed[0].ID != blocker {
				t.Fatalf("competing worker did not reserve %s capacity: %v %v", occupied.Role, claimed, err)
			}
			t.Cleanup(func() {
				if err := other.ReleaseClaim(context.WithoutCancel(t.Context()), blocker, "failed"); err != nil {
					t.Error(err)
				}
			})
			target := create("Plan before implementation")
			request.ProviderRequirement, request.PlanProviderRequirement = resolve(code), resolve(plan)
			capacityRefused := false
			request.CandidateClaimObserved = func(_ context.Context, issue connector.Issue, allowed bool, predicate string, requirement *providercapacity.Requirement) {
				capacityRefused = issue.ID == target && !allowed && predicate == "hub_claim.provider_capacity" && requirement != nil && *requirement == plan
			}
			claimed, err = local.FetchCandidateIssues(t.Context(), request)
			claim, found := local.ExecutionClaim(target)
			if occupied == plan {
				if len(claimed) != 0 || found || !capacityRefused {
					t.Fatalf("full plan account did not refuse admission: candidates=%v claim=%t refused=%t error=%v", claimed, found, capacityRefused, err)
				}
				return
			}
			if err != nil || len(claimed) != 1 || claimed[0].ID != target || !found || claim.Lease.ProviderReservation == nil || claim.Lease.ProviderReservation.Requirement != plan {
				t.Fatalf("free planning capacity was refused: candidates=%v permit=%+v error=%v", claimed, claim.Lease.ProviderReservation, err)
			}
			if err := local.ReleaseClaim(t.Context(), target, "failed"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
