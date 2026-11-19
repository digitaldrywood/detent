package runnerfleet_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerAdmissionPublishesNativeDecision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := hubtest.NewClock(now)
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{Clock: clock.Now})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.Enroll(t)
	first, second := f.Create(t, "First delivery"), f.Create(t, "Second delivery")
	foreign := hubfixture.NewNative(t, f.Hub, f.Organization(), "Foreign admission")
	foreignIssue := foreign.Create(t, "Foreign delivery")
	other := hubfixture.NewNativeInOrganization(t, f.Hub)
	otherIssue := other.Create(t, "Other organization delivery")
	admission := func(issue tracker.NativeIssue, predicate string) runnerauth.DiagnosticAdmission {
		return runnerauth.DiagnosticAdmission{IssueID: string(issue.WorkItemID), WorkItemRevision: int64(issue.Revision), ObservedAt: now, Result: "skipped", Predicate: predicate}
	}
	configuration := func(project tracker.ProjectID, admissions ...runnerauth.DiagnosticAdmission) runnerauth.ProjectConfiguration {
		return runnerauth.ProjectConfiguration{
			ProjectID: string(project), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now,
			Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: clock.Now(), Source: "runner_runtime_and_durable_attempt_owners", Admissions: admissions},
		}
	}
	heartbeat := func(target hubfixture.Native, view runnerauth.ProjectConfiguration) *httptest.ResponseRecorder {
		t.Helper()
		clock.Advance(time.Second)
		return f.Hub.Request(t, http.MethodPost, target.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{
			"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.Redemption.BackendIsolation, "project_configuration": view,
		})
	}
	latestRecord := func(target hubfixture.Native, issue tracker.NativeIssue) (eventstore.Event, error) {
		t.Helper()
		var record eventstore.Event
		err := f.Hub.Repository.ReadTenant(t.Context(), target.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			record, err = reader.(eventstore.WorkflowTransitionReader).LatestSchedulerDecision(t.Context(), target.Project.ID, issue.WorkItemID)
			return err
		})
		return record, err
	}
	requireNoDecision := func(target hubfixture.Native, issue tracker.NativeIssue) {
		t.Helper()
		if record, err := latestRecord(target, issue); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("decision for %s = %s, err=%v, want not found", issue.WorkItemID, record.ID, err)
		}
	}
	observe := func() error {
		t.Helper()
		return f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
			_, _, err := reader.RunnerProjectObservation(t.Context(), r.Binding.RunnerID, f.Project.ID, "")
			return err
		})
	}
	view := configuration(f.Project.ID,
		admission(first, "duplicate_pull_request_work"),
		admission(second, "current_head_ci_wait"),
		admission(foreignIssue, "duplicate_pull_request_work"),
		admission(otherIssue, "duplicate_pull_request_work"),
	)
	if _, err := f.Hub.Admin.ExecContext(t.Context(), `
		CREATE FUNCTION fail_admission_observation() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'observation storage unavailable'; END $$;
		CREATE TRIGGER fail_admission_observation BEFORE INSERT OR UPDATE ON runner_project_observations
		FOR EACH ROW EXECUTE FUNCTION fail_admission_observation()
	`); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, heartbeat(f, view), http.StatusInternalServerError)
	requireNoDecision(f, first)
	requireNoDecision(f, second)
	if err := observe(); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("observation after failed heartbeat: %v, want not found", err)
	}
	if _, err := f.Hub.Admin.ExecContext(t.Context(), "DROP TRIGGER fail_admission_observation ON runner_project_observations"); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, heartbeat(f, view), http.StatusOK)
	if err := observe(); err != nil {
		t.Fatalf("observation after successful heartbeat: %v", err)
	}
	call := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(f.Organization()), f.Token)
	requireDecision := func(issue tracker.NativeIssue, reason string) eventstore.Event {
		t.Helper()
		record, err := latestRecord(f, issue)
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(f.Hub.Config.RunDetails).Event(t.Context(), record)
		if err != nil {
			t.Fatal(err)
		}
		if d := event.Data.Decision; d == nil || d.Reason != reason || d.Outcome != "skipped" || d.RunnerID != r.Binding.RunnerID || d.WorkItemRevision != issue.Revision || !d.At.Equal(now) {
			t.Fatalf("persisted decision = %+v", d)
		}
		reply := call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": string(f.Project.ID), "reference": strconv.Itoa(issue.Number)})
		payload := hubfixture.MCPData(t, reply, false)
		var explanation explain.IssueExplanation
		if err := json.Unmarshal(payload, &explanation); err != nil {
			t.Fatal(err)
		}
		if latest := explanation.Eligibility.Latest; latest == nil || latest.Reason != reason || latest.EvidenceID != record.ID || latest.Outcome != "skipped" {
			t.Fatalf("explain_item lost refusal: %s", payload)
		}
		return record
	}
	firstReason, secondReason := view.Diagnostics.Admissions[0].Predicate, view.Diagnostics.Admissions[1].Predicate
	firstDecision, secondDecision := requireDecision(first, firstReason), requireDecision(second, secondReason)
	hubfixture.RequireStatus(t, heartbeat(f, view), http.StatusOK)
	wrongRevision := admission(first, "project_capacity_full")
	wrongRevision.WorkItemRevision++
	unknownItem := admission(second, "project_capacity_full")
	unknownItem.IssueID = "wi_unknown"
	hubfixture.RequireStatus(t, heartbeat(f, configuration(f.Project.ID, wrongRevision, unknownItem)), http.StatusOK)
	stale := admission(first, "project_capacity_full")
	stale.ObservedAt = now.Add(-time.Second)
	hubfixture.RequireStatus(t, heartbeat(f, configuration(f.Project.ID, stale)), http.StatusOK)
	foreignView := configuration(other.Project.ID, admission(otherIssue, "duplicate_pull_request_work"))
	hubfixture.RequireStatus(t, heartbeat(f, foreignView), http.StatusUnprocessableEntity)
	hubfixture.RequireStatus(t, heartbeat(other, foreignView), http.StatusUnauthorized)
	if current := requireDecision(first, firstReason); current.ID != firstDecision.ID {
		t.Fatalf("first decision changed from %s to %s", firstDecision.ID, current.ID)
	}
	if current := requireDecision(second, secondReason); current.ID != secondDecision.ID {
		t.Fatalf("second decision changed from %s to %s", secondDecision.ID, current.ID)
	}
	requireNoDecision(foreign, foreignIssue)
	requireNoDecision(other, otherIssue)
}
