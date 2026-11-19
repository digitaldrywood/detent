package runnerfleet_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	if want := predicates[len(predicates)-1]; event.Data.Decision == nil || event.Data.Decision.Reason != want {
		t.Fatalf("decision=%+v, want latest %q", event.Data.Decision, want)
	}
}
