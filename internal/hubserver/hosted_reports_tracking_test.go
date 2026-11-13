package hubserver

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsTrackingIssues(t *testing.T) {
	t.Parallel()
	failures := []nativeFailure{
		{WorkItem: "wi_a", Example: "hub request timed out"},
		{WorkItem: "wi_b", Example: "landing gate failed"},
		{WorkItem: "wi_c", Example: "something nobody filed"},
	}
	for _, tt := range []struct {
		name     string
		body     func() string
		archived bool
		linked   bool
	}{
		{"signature digest", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "worker", Fingerprint: issueorigin.Fingerprint(normalizeFailureSignature("hub request timed out"))})
		}, false, true},
		{"class name", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})
		}, false, true},
		{"other fingerprint", func() string {
			return issueorigin.Stamp("Unrelated", issueorigin.Origin{Kind: "worker", Fingerprint: "unrelated"})
		}, false, false},
		{"unstamped issue", func() string { return "Hub request timed out again" }, false, false},
		{"archived issue", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})
		}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objects := &memoryRunDetails{}
			f := newNativeFixture(t, newPostgresNativeService(t, Config{RunDetails: objects}), "", "tracking")
			issue := f.create(t, "Tracking")
			body := tt.body() + "\n\n" + issueContractTestSections
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "tracking-body"}, ExpectedRevision: issue.Revision, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if tt.archived {
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/archive", f.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "tracking-archive"}, ExpectedRevision: issue.Revision})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
			classes := reportFailureClasses(failures)
			if err := f.service.reportsTrackingIssues(t.Context(), scope, classes, failures); err != nil {
				t.Fatal(err)
			}

			for _, class := range classes {
				want := ""
				if tt.linked && class.Class == "Hub request timed out" {
					want = string(issue.WorkItemID)
				}
				if class.TrackingWorkItemID != want {
					t.Fatalf("%s tracking=%q want %q", class.Class, class.TrackingWorkItemID, want)
				}
			}
			if tt.linked {
				for index, fingerprint := range []string{tt.body(), issueorigin.Stamp("Later class tracker", issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})} {
					later := f.create(t, "Later tracking "+string(rune('a'+index)))
					laterBody := fingerprint + "\n\n" + issueContractTestSections
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(later.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "later-tracking-body"}, ExpectedRevision: later.Revision, Body: &laterBody})
					requireNativeStatus(t, response, http.StatusOK)
				}
				earliest := reportFailureClasses(failures)
				if err := f.service.reportsTrackingIssues(t.Context(), scope, earliest, failures); err != nil {
					t.Fatal(err)
				}
				for _, class := range earliest {
					if class.Class == "Hub request timed out" && class.TrackingWorkItemID != string(issue.WorkItemID) {
						t.Fatalf("later matching fingerprint displaced earliest issue: %#v", class)
					}
				}
			}
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "tracking-other-project")
			foreign := newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "tracking-other-organization")
			for _, isolated := range []nativeFixture{other, foreign} {
				match := isolated.create(t, "Isolated tracking")
				body := issueorigin.Stamp(issueContractTestSections, issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})
				response := performHubAPIRequest(t, f.service, http.MethodPatch, isolated.base+"/work-items/"+string(match.WorkItemID), isolated.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "isolated-tracking-body"}, ExpectedRevision: match.Revision, Body: &body})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &match)
				credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), isolated.project.OrganizationID, apikey.HashToken(isolated.token), "", "")
				if err != nil {
					t.Fatal(err)
				}
				isolatedClasses := reportFailureClasses(failures)
				if err := f.service.reportsTrackingIssues(t.Context(), hubscope.NativeScope{Organization: isolated.project.OrganizationID, Project: isolated.project.ID, Credential: credential}, isolatedClasses, failures); err != nil {
					t.Fatal(err)
				}
				for _, class := range isolatedClasses {
					want := ""
					if class.Class == "Hub request timed out" {
						want = string(match.WorkItemID)
					}
					if class.TrackingWorkItemID != want {
						t.Fatalf("isolated project %s class=%s tracking=%q want=%q", isolated.project.ID, class.Class, class.TrackingWorkItemID, want)
					}
				}
			}
			isolatedClasses := reportFailureClasses(failures)
			if err := f.service.reportsTrackingIssues(t.Context(), scope, isolatedClasses, failures); err != nil {
				t.Fatal(err)
			}
			for _, class := range isolatedClasses {
				want := ""
				if tt.linked && class.Class == "Hub request timed out" {
					want = string(issue.WorkItemID)
				}
				if class.TrackingWorkItemID != want {
					t.Fatalf("tracking escaped tenant/project: class=%s tracking=%q want=%q", class.Class, class.TrackingWorkItemID, want)
				}
			}
			deniedScope := scope
			deniedScope.Project = other.project.ID
			if err := f.service.reportsTrackingIssues(t.Context(), deniedScope, reportFailureClasses(failures), failures); err == nil {
				t.Fatal("tracking lookup bypassed project authorization")
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			failed := f.create(t, "Example reported failure")
			worker := f.worker(t, "Example failure runner")
			workerCredential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
			if err != nil {
				t.Fatal(err)
			}
			lease := claimNativeAttempt(t, f, worker, string(workerCredential.Runner.MachineID), "tracking_session", failed.WorkItemID)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(failed.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "tracking-failed", 2, "failed"
			event.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "hub request timed out", ErrorClass: "protocol", ObservedAt: f.service.config.Clock()}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			reportService := &Service{config: f.service.config}
			reportService.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(scope.Organization)}
			now := f.service.config.Clock()
			window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
			report, err := reportService.readPostgresHostedReport(t.Context(), credential, []string{string(scope.Project)}, window, reportsFilter{}, true)
			wantTracking := ""
			if tt.linked {
				wantTracking = string(issue.WorkItemID)
			}
			if err != nil || len(report.Failures) != 1 || report.Failures[0].Class != "Hub request timed out" || report.Failures[0].TrackingWorkItemID != wantTracking || report.Failures[0].Count != 1 || report.PriorFailed == nil || *report.PriorFailed != 0 || slices.Contains(report.Unavailable, "failure_tracking_partial") {
				t.Fatalf("live report tracking=%+v prior=%v unavailable=%v error=%v", report.Failures, report.PriorFailed, report.Unavailable, err)
			}
			var sourceEvent eventstore.Event
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				sourceEvent, err = r.(hubrecords.NativeRunEventReader).RunEvent(t.Context(), scope.Project, failed.WorkItemID, nativeAttemptHistoryID(event.Type, event.Data))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			saved, present := objects.data[sourceEvent.PayloadObjectKey]
			objects.mu.Unlock()
			if !present {
				t.Fatal("tracking event object was not persisted")
			}
			t.Cleanup(func() { objects.mu.Lock(); objects.data[sourceEvent.PayloadObjectKey] = saved; objects.mu.Unlock() })
			objects.mu.Lock()
			delete(objects.data, sourceEvent.PayloadObjectKey)
			objects.mu.Unlock()
			missing, err := reportService.readPostgresHostedReport(t.Context(), credential, []string{string(scope.Project)}, window, reportsFilter{}, true)
			objects.mu.Lock()
			objects.data[sourceEvent.PayloadObjectKey] = saved
			objects.mu.Unlock()
			if err != nil || !slices.Contains(missing.Unavailable, "failure_tracking_partial") || len(missing.Failures) != 1 || missing.Failures[0].TrackingWorkItemID != "" || missing.Failures[0].Count != 1 {
				t.Fatalf("missing immutable failure evidence became complete tracking: failures=%+v unavailable=%v error=%v", missing.Failures, missing.Unavailable, err)
			}
			if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, scope.Organization, scope.Project, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if !tt.archived {
				if err := f.service.reportsTrackingIssues(t.Context(), scope, reportFailureClasses(failures), failures); !errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
					t.Fatalf("incomplete identities became a no-match: %v", err)
				}
			}
			if !tt.archived {
				pending, err := reportService.readPostgresHostedReport(t.Context(), credential, []string{string(scope.Project)}, window, reportsFilter{}, true)
				if err != nil || !slices.Contains(pending.Unavailable, "failure_tracking_partial") || len(pending.Failures) != 1 || pending.Failures[0].TrackingWorkItemID != "" {
					t.Fatalf("pending identity coverage became complete no-match: %+v error=%v", pending, err)
				}
			}
		})
	}
}
