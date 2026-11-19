package runnerfleet_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerAdmissionPublishesLatestDecisionPerIssue(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.Enroll(t)
	issue := f.Create(t, "Repeated delivery")
	now := f.Hub.Now().UTC()
	predicates := []string{"duplicate_pull_request_work", "current_head_ci_wait", "project_capacity_full"}
	var admissions []runnerauth.DiagnosticAdmission
	for i, predicate := range predicates {
		admissions = append(admissions, runnerauth.DiagnosticAdmission{IssueID: string(issue.WorkItemID), WorkItemRevision: int64(issue.Revision),
			ObservedAt: now.Add(time.Duration(i-len(predicates)) * time.Second), Result: "skipped", Predicate: predicate})
	}
	view := runnerauth.ProjectConfiguration{ProjectID: string(f.Project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now,
		Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{}, Admissions: admissions},
	}
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{
		"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.Redemption.BackendIsolation, "project_configuration": view,
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	decision := readLatestSchedulerDecision(t, f, issue)
	if want := predicates[len(predicates)-1]; decision == nil || decision.Reason != want {
		t.Fatalf("decision=%+v, want latest %q", decision, want)
	}
}

func readLatestSchedulerDecision(t *testing.T, f hubfixture.Native, issue tracker.NativeIssue) *tracker.NativeSchedulerDecision {
	t.Helper()
	var row eventstore.Event
	err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.(eventstore.WorkflowTransitionReader).LatestSchedulerDecision(t.Context(), f.Project.ID, issue.WorkItemID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(f.Hub.Config.RunDetails).Event(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	return event.Data.Decision
}

func TestRunnerAdmissionDecisionsYieldToNewerItemDecisions(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{Clock: clock.Now})
	reporter := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	reporter.Enroll(t)
	claimer := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	claimer.Enroll(t)
	claimer.ReportCapacity(t)
	claimed := f.Create(t, "Claimed by another runner")
	vetoed := f.Create(t, "Skip history never vetoes a claim")
	claim := func(issue tracker.NativeIssue, session string) {
		t.Helper()
		request := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: issue.WorkItemID, MachineID: claimer.Binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", claimer.Redemption.Credential, request), http.StatusOK)
	}
	report := func(issue tracker.NativeIssue, observed time.Time, predicate string) {
		t.Helper()
		now := clock.Advance(time.Second)
		if next := observed.Add(time.Second); now.Before(next) {
			clock.Set(next)
			now = next
		}
		view := runnerauth.ProjectConfiguration{ProjectID: string(f.Project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{
			ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{},
			Admissions: []runnerauth.DiagnosticAdmission{{IssueID: string(issue.WorkItemID), WorkItemRevision: int64(issue.Revision), ObservedAt: observed, Result: "skipped", Predicate: predicate}},
		}}
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(reporter.Binding.MachineID)+"/heartbeat", reporter.Redemption.Credential, map[string]any{
			"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": reporter.Redemption.BackendIsolation, "project_configuration": view,
		})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
			observation, _, err := reader.RunnerProjectObservation(t.Context(), reporter.Binding.RunnerID, f.Project.ID, string(issue.WorkItemID))
			if err != nil {
				return err
			}
			if observation.Diagnostics == nil || !slices.ContainsFunc(observation.Diagnostics.Admissions, func(admission runnerauth.DiagnosticAdmission) bool {
				return admission.IssueID == string(issue.WorkItemID) && admission.Predicate == predicate
			}) {
				t.Errorf("recorded observation lost admission %q for %s", predicate, issue.WorkItemID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	latest := func(issue tracker.NativeIssue, runner, outcome, reason string) {
		t.Helper()
		decision := readLatestSchedulerDecision(t, f, issue)
		if decision == nil || decision.RunnerID != runner || decision.Outcome != outcome || reason != "" && decision.Reason != reason {
			t.Fatalf("latest decision = %+v, want runner=%s outcome=%s reason=%q", decision, runner, outcome, reason)
		}
	}
	claimedAt := clock.Now()
	claim(claimed, "other-runner-claim")
	latest(claimed, claimer.Binding.RunnerID, "claimed", "")
	report(claimed, claimedAt.Add(-time.Second), "older_than_claim")
	latest(claimed, claimer.Binding.RunnerID, "claimed", "")
	report(claimed, claimedAt, "same_time_as_claim")
	latest(claimed, claimer.Binding.RunnerID, "claimed", "")
	report(claimed, claimedAt.Add(5*time.Second), "newer_than_claim")
	latest(claimed, reporter.Binding.RunnerID, "skipped", "newer_than_claim")
	report(claimed, claimedAt.Add(3*time.Second), "out_of_order")
	latest(claimed, reporter.Binding.RunnerID, "skipped", "newer_than_claim")
	report(claimed, claimedAt.Add(7*time.Second), "latest_in_order")
	latest(claimed, reporter.Binding.RunnerID, "skipped", "latest_in_order")
	report(vetoed, clock.Now(), "project_capacity_full")
	latest(vetoed, reporter.Binding.RunnerID, "skipped", "project_capacity_full")
	claim(vetoed, "after-skip-history")
	latest(vetoed, claimer.Binding.RunnerID, "claimed", "")
}
