package planner_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	for _, historical := range []bool{false, true} {
		for _, occupied := range []providercapacity.Requirement{code, plan} {
			name := occupied.Role + " account busy"
			if historical {
				name += " after changing omitted human direction"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				reports := func() ([]providercapacity.Report, error) {
					now := time.Now().UTC()
					return []providercapacity.Report{
						{Provider: "openai", Backend: code.Backend, AccountAlias: "code", SharedAccountAlias: "code", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
						{Provider: "openai", Backend: plan.Backend, AccountAlias: "plan", SharedAccountAlias: "plan", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
					}, nil
				}
				descriptor, local, other, create, admin := scenariotest.OpenPlannerAdmission(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, reports)
				resolve := func(requirement providercapacity.Requirement) func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					return func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
						return requirement, nil
					}
				}
				request := orchestrator.SchedulingRequest{Policy: descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}, ProviderRequirement: resolve(occupied)}
				var target string
				if historical {
					target = create("Replan after implementation")
					seed := request
					seed.ProviderRequirement = resolve(code)
					seedPlanWithImplementation(t, local, admin, seed, target, occupied == plan)
				}
				blocker := create("Hold provider capacity")
				request.CandidateReady = func(_ context.Context, issue connector.Issue) bool { return issue.ID == blocker }
				claimed, err := other.FetchCandidateIssues(t.Context(), request)
				if err != nil || len(claimed) != 1 || claimed[0].ID != blocker {
					t.Fatalf("competing worker did not reserve %s capacity: %v %v", occupied.Role, claimed, err)
				}
				t.Cleanup(func() {
					if err := other.ReleaseClaim(context.WithoutCancel(t.Context()), blocker, "failed"); err != nil {
						t.Error(err)
					}
				})
				if target == "" {
					target = create("Plan before implementation")
				}
				request.CandidateReady = func(_ context.Context, issue connector.Issue) bool { return issue.ID == target }
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
				if historical {
					continuation, err := local.RunExecution(target).(runner.PhaseReceiptExecution).PhaseReceipts(t.Context(), "")
					if err != nil || continuation.Plan == nil || !continuation.Plan.NeedsProvider() || continuation.NextPhase != "plan" {
						t.Fatalf("execution did not resume at planning: %+v, %v", continuation, err)
					}
				}
				if err := local.ReleaseClaim(t.Context(), target, "failed"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func seedPlanWithImplementation(t *testing.T, local *scenariotest.Scheduler, admin *api.NativeClient, request orchestrator.SchedulingRequest, target string, omitDiscussion bool) {
	t.Helper()
	ctx, item := t.Context(), tracker.NativeWorkItemID(target)
	body, projected := "Keep the original behavior.", 32
	if omitDiscussion {
		body, projected = strings.Repeat("Keep the original behavior. ", 1300), 0
	}
	var oldest tracker.NativeComment
	for i := range 33 {
		comment, err := admin.CreateComment(ctx, item, tracker.CreateComment{Mutation: api.NativeMutationKey(), Body: body})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = comment
		}
	}
	candidates, err := local.FetchCandidateIssues(ctx, request)
	if err != nil || len(candidates) != 1 || candidates[0].ID != target {
		t.Fatalf("cannot seed claimed plan: %v, %v", candidates, err)
	}
	execution := local.RunExecution(target)
	if execution == nil {
		t.Fatal("missing claim for plan")
	}
	if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "coder", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	recovery := execution.Recovery()
	if len(recovery.Discussion) != projected || projected != 0 && recovery.Discussion[0].ID == oldest.ID {
		t.Fatalf("test did not omit the old direction: projected=%d", len(recovery.Discussion))
	}
	head, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", HeadSHA: head, WorkspaceDigest: tree, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	step := tracker.NativeStepReceipt{Step: "plan", Source: tracker.NativeCheckpoint{HeadSHA: head, WorkspaceDigest: tree}, CompletedAt: time.Now().UTC(), CompletionBody: "Keep the original behavior.", PlanApproval: "automated:APPROVED"}
	if err := execution.(interface {
		RecordStep(context.Context, tracker.NativeStepReceipt) error
	}).RecordStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	phases := execution.(runner.PhaseReceiptExecution)
	if err := phases.RecordPhaseReceipt(ctx, "plan", "item-branch", head, tree, ""); err != nil {
		t.Fatal(err)
	}
	if err := phases.RecordPhaseReceipt(ctx, "implement", "item-branch", strings.Repeat("c", 40), strings.Repeat("d", 40), ""); err != nil {
		t.Fatal(err)
	}
	continuation, err := phases.PhaseReceipts(ctx, "")
	if err != nil || continuation.Plan == nil || !continuation.Plan.Accepted || !slices.ContainsFunc(continuation.Receipts, func(receipt tracker.NativePhaseReceipt) bool { return receipt.Phase == "implement" }) {
		t.Fatalf("valid plan and code were not reusable before the edit: %+v, %v", continuation, err)
	}
	if err := execution.Finish(ctx, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := local.ReleaseClaim(ctx, target, "released"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.UpdateComment(ctx, item, oldest.ID, tracker.UpdateComment{Mutation: api.NativeMutationKey(), ExpectedRevision: oldest.Revision, Body: "Change the original behavior."}); err != nil {
		t.Fatal(err)
	}
	current, err := admin.Recovery(ctx, item)
	if err != nil || len(current.Discussion) != projected || projected != 0 && current.Discussion[0].ID == oldest.ID {
		t.Fatalf("edited direction was not omitted: projected=%d, %v", len(current.Discussion), err)
	}
	receipts, err := admin.ItemPhaseReceipts(ctx, item)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("did not retain both receipts: %v, %v", receipts, err)
	}
	resolved, err := admin.ResolvePlan(ctx, current, request.Policy.ID, receipts[1:])
	if err != nil || resolved == nil || resolved.Accepted || !resolved.NeedsProvider() {
		t.Errorf("plan survived edit to a comment outside the projection: %+v, %v", resolved, err)
	}
}

func TestPhasedLiveReconnectChecksOccupiedDestination(t *testing.T) {
	code := providercapacity.Requirement{Role: runner.RoleCode, Backend: "coder", Model: "test"}
	plan := providercapacity.Requirement{Role: runner.RolePlan, Backend: "planner", Model: "test"}
	for _, initial := range []providercapacity.Requirement{{}, plan, code} {
		t.Run("permit "+initial.Role, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			reports := func() ([]providercapacity.Report, error) {
				now := time.Now().UTC()
				return []providercapacity.Report{
					{Provider: "openai", Backend: code.Backend, AccountAlias: "code", SharedAccountAlias: "code", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
					{Provider: "openai", Backend: plan.Backend, AccountAlias: "plan", SharedAccountAlias: "plan", Models: []string{"test"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now},
				}, nil
			}
			descriptor, local, other, create, _ := scenariotest.OpenPlannerAdmission(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, reports)
			reserve := func(s *scenariotest.Scheduler, target string, requirement providercapacity.Requirement) {
				t.Helper()
				request := orchestrator.SchedulingRequest{Policy: descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"},
					CandidateReady: func(_ context.Context, issue connector.Issue) bool { return issue.ID == target },
					ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
						return requirement, nil
					},
				}
				candidates, err := s.FetchCandidateIssues(ctx, request)
				if err != nil || len(candidates) != 1 || candidates[0].ID != target {
					t.Fatalf("could not reserve %s for %s: %v, %v", requirement.Role, target, candidates, err)
				}
			}
			type phased interface {
				StartPhased(context.Context, tracker.NativeExecutionIdentity) error
				SetPhaseProvider(context.Context, providercapacity.Requirement) error
			}
			target := create("Reconnect running item")
			first, destination := plan, code
			if initial == code {
				first, destination = code, plan
			}
			reserve(local, target, first)
			claim, ok := local.ExecutionClaim(target)
			if !ok {
				t.Fatal("missing live claim")
			}
			identity := tracker.NativeExecutionIdentity{Role: code.Role, Backend: code.Backend, Model: code.Model}
			execution := nativechange.NewExecution(local, claim)
			if err := execution.(phased).StartPhased(ctx, identity); err != nil {
				t.Fatal(err)
			}
			client := claim.Source.NativeClient()
			attempt := execution.CompletionData()
			if initial.Role == "" {
				lease, err := client.SetLeaseProviderPhase(ctx, claim.Lease, 90, tracker.NativeProviderPhaseMutation{Expected: first, Sequence: attempt.Sequence})
				if err != nil || lease.ProviderReservation != nil {
					t.Fatalf("could not simulate reconnect without permit: %+v, %v", lease, err)
				}
				claim.Lease = lease
			}
			blocker := create("Occupy destination during reconnect")
			reserve(other, blocker, destination)
			var err error
			claim.Recovery, err = client.Recovery(ctx, claim.Lease.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			reconnected := nativechange.NewExecution(local, claim)
			if err := reconnected.(phased).StartPhased(ctx, identity); err != nil {
				t.Fatal(err)
			}
			if data := reconnected.CompletionData(); data.AttemptID != attempt.AttemptID || data.Sequence <= attempt.Sequence {
				t.Fatalf("provider phase did not reconnect the live attempt: before=%s/%d after=%s/%d", attempt.AttemptID, attempt.Sequence, data.AttemptID, data.Sequence)
			}
			var refusal *api.APIError
			if err := reconnected.(phased).SetPhaseProvider(ctx, destination); !errors.As(err, &refusal) || refusal.Code != "provider_capacity" {
				t.Fatalf("reconnect accepted an occupied %s destination: %v", destination.Role, err)
			}
			lease, err := client.Renew(ctx, claim.Lease, 90)
			if err != nil || initial.Role == "" && lease.ProviderReservation != nil || initial.Role != "" && (lease.ProviderReservation == nil || lease.ProviderReservation.Requirement != initial) {
				t.Fatalf("refused swap changed the original reservation: %+v, %v", lease.ProviderReservation, err)
			}
			if err := other.ReleaseClaim(ctx, blocker, "released"); err != nil {
				t.Fatal(err)
			}
			if err := reconnected.(phased).SetPhaseProvider(ctx, destination); err != nil {
				t.Fatalf("reconnect refused free %s capacity: %v", destination.Role, err)
			}
			if permit := reconnected.(runner.ProviderCapacityExecution).ProviderCapacity(); permit == nil || permit.Requirement != destination {
				t.Fatalf("reconnect did not acquire destination capacity: %+v", permit)
			}
			if err := client.ReleaseAttempt(ctx, claim.Lease, "cancelled", true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
