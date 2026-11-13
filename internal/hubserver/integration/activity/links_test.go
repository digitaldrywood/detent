package activity_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestActivityTypedAndHistoricalAttemptLinks(t *testing.T) {
	const changeLegacyReadLimit = 100
	f := newActivityPostgresFixture(t, "org_activity_links")
	content := eventstore.NewContent(f.service.runDetailStore())
	store := func(issue corestore.WorkItem, id string, at time.Time, run tracker.NativeRunData, assigned bool) {
		t.Helper()
		f.fence++
		start, end := at.Add(-time.Minute), at
		lease := leasestore.Lease{OrganizationID: f.organization, LeaseID: "lease_" + id, FencingToken: f.fence, IssueID: issue.ID, MachineID: "machine_browser_preview", SessionID: "lease-session-" + id, WorkItemRevision: issue.Revision, AcquiredAt: start, RenewedAt: start, ExpiresAt: at.Add(time.Hour), ReleasedAt: &end}
		if err := f.repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: f.receipt(t, "core-lease", lease.LeaseID, lease)}); err != nil {
			t.Fatal(err)
		}
		if assigned {
			assignment := runnerstore.LeaseAssignment{OrganizationID: f.organization, LeaseID: lease.LeaseID, RunnerID: "runner_browser_preview", AssignedAt: start}
			if err := f.repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: f.receipt(t, "core-runner-lease-assignment", lease.LeaseID, assignment)}); err != nil {
				t.Fatal(err)
			}
		}
		run.AttemptID, run.RunID, run.LeaseID, run.FencingToken, run.Sequence, run.Outcome = id, id, tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken), 1, "succeeded"
		raw, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
		if err != nil {
			t.Fatal(err)
		}
		attempt := leasestore.Attempt{OrganizationID: f.organization, ID: id, IssueID: issue.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: id, Sequence: 1, Status: "succeeded", WorkItemRevision: issue.Revision, PayloadObjectKey: ref.Key, StartedAt: start, UpdatedAt: at}
		prepared := postgres.CoreAttemptImport{Attempt: attempt}
		if run.Handoff != nil {
			raw, err := json.Marshal(run.Handoff)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			prepared.Attempt.CheckpointObjectKey, prepared.CheckpointReference = &checkpoint.Key, checkpoint
		}
		prepared.Receipt = f.receipt(t, "core-attempt", id, prepared.Attempt)
		if err := f.repository.ImportCoreAttempt(t.Context(), prepared); err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: f.organization, ProjectID: issue.ProjectID, AggregateID: issue.NativeID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine_browser_preview"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &run}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), f.organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
	}
	for entity := 0; entity < 2; entity++ {
		project := f.createProject(t, fmt.Sprintf("Activity links %d", entity))
		issue := f.issue(t, project, fmt.Sprintf("Activity links %d", entity))
		changeID, versionID := fmt.Sprintf("activity_change_%d", entity), fmt.Sprintf("activity_version_%d", entity)
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			policy, err := w.ProjectPolicy(t.Context(), issue.ProjectID)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(tracker.ChangeRequest{ID: changeID, OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, Title: "Activity change", Revision: 1, CreatedAt: f.now.Add(-time.Hour), UpdatedAt: f.now.Add(-time.Hour)})
			if err != nil {
				return err
			}
			prepared, err := content.PrepareChange(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			change, err := w.CreateChange(t.Context(), prepared)
			if err != nil {
				return err
			}
			raw, err = json.Marshal(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, CreatedAt: f.now.Add(-time.Hour), Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: policy.PolicyID, Repository: "example-org/example-project", External: &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://example.test/pull/42"}}})
			if err != nil {
				return err
			}
			version, err := content.PrepareVersion(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			change.CurrentVersionID = &version.ID
			if _, err := w.UpdateChange(t.Context(), change); err != nil {
				return err
			}
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, ChangeID: changeID, CreatedAt: f.now.Add(-time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		for sourceIndex, source := range []string{"finalization", "checkpoint", "latest Change"} {
			for _, assigned := range []bool{false, true} {
				name := fmt.Sprintf("entity_%d/%s/assigned_%t", entity, source, assigned)
				t.Run(name, func(t *testing.T) {
					index := sourceIndex * 2
					if assigned {
						index++
					}
					at := f.now.Add(-30*time.Minute + time.Duration(index)*3*time.Minute)
					id := fmt.Sprintf("activity_links_%d_%d", entity, index)
					run := tracker.NativeRunData{RunnerID: "runner_browser_preview", SessionID: "recorded-session", Identity: &tracker.NativeExecutionIdentity{Role: "code"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", PhasesDropped: 2, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}}
					if assigned {
						run.RunnerID = "payload-runner"
						run.Runtime.Identity = agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", at)
					}
					wantURL := "https://example.test/pull/42"
					switch source {
					case "finalization":
						wantURL = "https://example.test/pull/99"
						if assigned {
							wantURL = "https://example.test/pull/77"
						}
						run.Finalization = &tracker.NativeFinalization{ChangeID: changeID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: wantURL}}}
					case "checkpoint":
						run.Handoff = &tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: changeID}}
					}
					store(issue, id, at, run, assigned)
					runnerFilter := ""
					if assigned {
						runnerFilter = "&runner_id=runner_browser_preview"
					}
					path := "/api/v2/organizations/" + string(f.organization) + "/activity?project_id=" + project + "&limit=5&from=" + at.Add(-2*time.Minute).Format(time.RFC3339Nano) + "&to=" + at.Add(time.Second).Format(time.RFC3339Nano) + runnerFilter
					response := f.page(t, "owner", path)
					browserHostedStatus(t, response, http.StatusOK)
					var report operatortool.ActivityReport
					usageDecode(t, response, &report)
					if len(report.Finished) != 1 {
						t.Fatalf("source=%s assigned=%t activity=%+v", source, assigned, report)
					}
					got := report.Finished[0]
					if got.ChangeID != changeID || got.PullRequestURL != wantURL || got.RunnerID != "runner_browser_preview" || got.SessionID != "recorded-session" || got.Phase != "implementation" || got.Stage != "code" || !got.Partial || got.StageDurationSeconds == nil || *got.StageDurationSeconds != 60 {
						t.Fatalf("source=%s assigned=%t lost rendered evidence: %+v", source, assigned, got)
					}
				})
			}
		}
		newerChangeID, newerVersionID := fmt.Sprintf("activity_newer_change_%d", entity), fmt.Sprintf("activity_newer_version_%d", entity)
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			policy, err := w.ProjectPolicy(t.Context(), issue.ProjectID)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(tracker.ChangeRequest{ID: newerChangeID, OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, Title: "Newer activity change", Revision: 1, CreatedAt: f.now.Add(-30 * time.Minute), UpdatedAt: f.now.Add(-30 * time.Minute)})
			if err != nil {
				return err
			}
			prepared, err := content.PrepareChange(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			change, err := w.CreateChange(t.Context(), prepared)
			if err != nil {
				return err
			}
			raw, err = json.Marshal(tracker.ChangeVersion{ID: newerVersionID, ChangeID: newerChangeID, Number: 1, CreatedAt: f.now.Add(-30 * time.Minute), Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: policy.PolicyID, Repository: "example-org/example-project", External: &tracker.ChangeExternalReference{Provider: "github", ID: "43", URL: "https://example.test/pull/43"}}})
			if err != nil {
				return err
			}
			version, err := content.PrepareVersion(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			change.CurrentVersionID = &version.ID
			if _, err := w.UpdateChange(t.Context(), change); err != nil {
				return err
			}
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: issue.NativeID, ChangeID: newerChangeID, CreatedAt: f.now.Add(-30 * time.Minute)})
		}); err != nil {
			t.Fatal(err)
		}
		for sourceIndex, source := range []string{"finalization", "checkpoint"} {
			t.Run(fmt.Sprintf("entity_%d/%s_before_newer_link", entity, source), func(t *testing.T) {
				at := f.now.Add(-9*time.Minute + time.Duration(sourceIndex)*3*time.Minute)
				id := fmt.Sprintf("activity_links_precedence_%d_%d", entity, sourceIndex)
				run := tracker.NativeRunData{RunnerID: "payload-runner", SessionID: "recorded-session", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", at), Phase: "implementation", PhasesDropped: 2, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}}
				wantURL := ""
				if source == "finalization" {
					wantURL = "https://example.test/pull/99"
					run.Finalization = &tracker.NativeFinalization{ChangeID: changeID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: wantURL}}}
				} else {
					run.Handoff = &tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: changeID}}
				}
				store(issue, id, at, run, true)
				path := "/api/v2/organizations/" + string(f.organization) + "/activity?project_id=" + project + "&limit=5&runner_id=runner_browser_preview&from=" + at.Add(-2*time.Minute).Format(time.RFC3339Nano) + "&to=" + at.Add(time.Second).Format(time.RFC3339Nano)
				response := f.page(t, "owner", path)
				browserHostedStatus(t, response, http.StatusOK)
				var report operatortool.ActivityReport
				usageDecode(t, response, &report)
				if len(report.Finished) != 1 {
					t.Fatalf("source=%s newer linked Change activity=%+v", source, report)
				}
				got := report.Finished[0]
				if got.ChangeID != changeID || got.PullRequestURL != wantURL || got.RunnerID != "runner_browser_preview" || got.SessionID != "recorded-session" || got.Phase != "implementation" || got.Stage != "code" || !got.Partial || got.StageDurationSeconds == nil || *got.StageDurationSeconds != 60 {
					t.Fatalf("source=%s replaced named Change or leaked unrelated URL: %+v", source, got)
				}
			})
		}
		for index := 0; index <= changeLegacyReadLimit; index++ {
			f.storeAttempt(t, issue, fmt.Sprintf("other_runner_%d_%d", entity, index), "succeeded", f.now.Add(-2*time.Minute), f.now.Add(-time.Minute), nil, false)
		}
		response := f.page(t, "owner", "/api/v2/organizations/"+string(f.organization)+"/activity?project_id="+project+"&limit=5&runner_id=missing-runner")
		browserHostedStatus(t, response, http.StatusOK)
		var report operatortool.ActivityReport
		usageDecode(t, response, &report)
		if len(report.Finished) != 0 || len(report.Running) != 0 {
			t.Fatalf("activity runner filter failed after overflow: %+v", report)
		}
	}
}
