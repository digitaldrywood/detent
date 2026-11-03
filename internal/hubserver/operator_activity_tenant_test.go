package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresActivityReadsLiveOutcomesAndIssueClocksWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := now.Add(-time.Hour)
	observed := now
	details := &memoryRunDetails{}
	provider := newHostedSecurityProvider()
	member := auth.Membership{ID: "member", UserID: "user", OrganizationID: "provider_org_first", Status: "active"}
	member.Role.Slug = "owner"
	provider.members["user"] = member
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{}, config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return observed }, Hosted: &HostedConfig{OrganizationID: "org_first", Provider: provider}}}
	service.registerPostgresAppRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "owner", Hash: apikey.HashToken(string(org) + "_owner"), Name: "Example owner", Scope: "operator", NativeOnly: true, UserID: "user", MembershipID: "member", KeyScope: string(apikey.ScopeRead), ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return fmt.Errorf("CreateAccessCredential: %w", err)
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "user", Email: "example@example.test", MembershipID: "member", Role: "owner", Active: true, PrincipalID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for projectNumber := 1; projectNumber <= 2; projectNumber++ {
				project := tracker.ProjectID(fmt.Sprintf("project_%d", projectNumber))
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "user", ProjectID: project, CanWrite: true}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "owner", ProjectID: project}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: string(project), Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
					return err
				}
				runner := string(project)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: runner, Name: "Example runner " + runner, Hash: apikey.HashToken(string(org) + runner), Kind: "service", Scope: "worker", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return fmt.Errorf("CreateAccessCredential: %w", err)
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: runner, RunnerID: runner, MachineID: tracker.MachineID(project), TokenHash: apikey.HashToken("enroll_" + string(org) + runner), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "owner"}); err != nil {
					return fmt.Errorf("CreateRunnerEnrollment: %w", err)
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: runner, MachineID: tracker.MachineID(project), TokenID: runner, EnrollmentID: runner, Scope: "organization", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
					return fmt.Errorf("CreateRunnerIdentity: %w", err)
				}
				for number := 1; number <= 2; number++ {
					id := tracker.NativeWorkItemID(fmt.Sprintf("%s_item_%d", project, number))
					state := "In Progress"
					if number == 2 {
						state = "Done"
					}
					actor := "owner"
					if projectNumber == 2 {
						actor = "other"
					}
					issue, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(number), Title: string(org) + " " + string(id), WorkflowStateName: &state, ActorPrincipalID: &actor, NativeCreatedAt: base, NativeUpdatedAt: now})
					if err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: issue.ID, ExpectedRevision: 1, MachineID: string(project), SessionID: string(id), LeaseID: string(id), TTL: time.Hour})
					if err != nil {
						return err
					}
					if lease.RenewedAt.After(observed) {
						observed = lease.RenewedAt
					}
					if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: now}); err != nil {
						return fmt.Errorf("AssignRunnerLease: %w", err)
					}
					status := "running"
					if number == 2 {
						status = "succeeded"
					}
					attempt, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: string(id), Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: string(id), Sequence: 1, Status: status, StartedAt: base.Add(20 * time.Minute), PayloadObjectKey: "example/run"})
					if err != nil {
						return err
					}
					if attempt.UpdatedAt.After(observed) {
						observed = attempt.UpdatedAt
					}
					phases := []tracker.NativePhase{{Name: "implement", StartedAt: base.Add(20 * time.Minute)}}
					if number == 2 {
						phases[0].FinishedAt = base.Add(40 * time.Minute)
					}
					run := tracker.NativeRunData{AttemptID: string(id), RunID: string(id), LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), SessionID: string(id), Runtime: &tracker.NativeRuntimeObservation{Phase: "implement", Phases: phases, Identity: agentidentity.Configured("example", "example", "", "code", "example", "example", "", "", base)}}
					values := []tracker.CollaborationEvent{{ID: "created_" + string(id), Type: "issue.created", RecordedAt: base}, {ID: "active_" + string(id), Type: "workflow.transitioned", RecordedAt: base.Add(10 * time.Minute), Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}, {ID: "run_" + string(id), Type: "run.updated", RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}}
					if number == 2 {
						values = append(values, tracker.CollaborationEvent{ID: "finished_" + string(id), Type: "workflow.transitioned", RecordedAt: base.Add(50 * time.Minute), Data: tracker.CollaborationData{FromState: "In Progress", ToState: "Done"}})
					}
					for _, value := range values {
						value.OrganizationID = org
						value.ProjectID = project
						value.AggregateID = id
						value.AggregateType = "issue"
						value.SchemaVersion = 1
						value.Actor = tracker.Actor{Kind: "human", PrincipalID: actor}
						raw, err := json.Marshal(value)
						if err != nil {
							return err
						}
						record, err := eventstore.NewContent(details).PrepareEvent(t.Context(), org, raw)
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), record); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	observed = observed.Add(time.Second)
	credential := apiCredential{HostedRole: "owner", Hosted: &auth.HostedIdentity{Subject: "user"}}
	request := operatortool.ActivityRequest{Limit: 100}
	window := operatortool.AnalyticsWindow{From: base.Add(-time.Hour), To: observed, Bucket: time.Hour}
	report, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Running) != 2 || len(report.Finished) != 2 || len(report.RunningIssues) != 2 || len(report.FinishedIssues) != 2 {
		t.Fatalf("activity omitted current or recorded work: %+v", report)
	}
	for _, issue := range append(report.RunningIssues, report.FinishedIssues...) {
		if strings.Contains(issue.Title, "org_second") || issue.ClockStartedAt.IsZero() || issue.TotalSeconds <= 0 || issue.WorkedSeconds <= 0 || len(issue.WorkedByRole) != 1 {
			t.Errorf("incomplete or cross-tenant issue activity: %+v", issue)
		}
	}
	for _, attempt := range report.Running {
		if attempt.Stage != "code" || attempt.Phase != "implement" || attempt.StageElapsedSeconds == nil {
			t.Errorf("live run phase facts missing: %+v", attempt)
		}
	}
	response := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/org_first/activity", "org_first_owner", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var delivered operatortool.ActivityReport
	decodeHubResponse(t, response, &delivered)
	if len(delivered.RunningIssues) != 2 || len(delivered.FinishedIssues) != 2 {
		t.Fatalf("Activity API omitted issue clocks: %+v", delivered)
	}
	if report.Partial || report.IssuesPartial {
		t.Fatalf("complete fixture marked partial: %+v", report)
	}
	request.Limit = 1
	clipped, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(clipped.Running) != 1 || len(clipped.Finished) != 1 || !clipped.Partial {
		t.Fatalf("bounded activity slice omitted partial: %+v", clipped)
	}
	request.Limit = 100
	request.RunnerID = "project_2"
	selectedRunner, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectedRunner.Running) != 1 || len(selectedRunner.FinishedIssues) != 1 || selectedRunner.Running[0].RunnerID != "project_2" {
		t.Fatalf("runner filter ignored: %+v", selectedRunner)
	}
	request.RunnerID = ""
	request.ProjectID = "project_1"
	scoped, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Running) != 1 || len(scoped.FinishedIssues) != 1 {
		t.Fatalf("project filter ignored: %+v", scoped)
	}
	credential.HostedRole = "reporter"
	credential.HostedPrincipal = "owner"
	request.ProjectID = ""
	own, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Running) != 1 || len(own.FinishedIssues) != 1 {
		t.Fatalf("reporter ownership not applied: %+v", own)
	}
	credential.HostedRole = "owner"
	credential.HostedPrincipal = ""
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessProjectGrant(t.Context(), "user", "project_2") }); err != nil {
		t.Fatal(err)
	}
	granted, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted.Running) != 1 || len(granted.FinishedIssues) != 1 {
		t.Fatalf("revoked project grant ignored: %+v", granted)
	}
	service.config.RunDetails = &memoryRunDetails{}
	missing, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if !missing.Partial || len(missing.Running) != 1 || len(missing.RunningIssues) != 1 {
		t.Fatalf("missing run object erased typed activity facts: %+v", missing)
	}
	service.config.RunDetails = details
	observed = observed.Add(2 * time.Hour)
	window.To = observed
	expired, err := service.readActivityReport(t.Context(), credential, request, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired.Running) != 0 || len(expired.Finished) != 2 {
		t.Fatalf("expired leases still displayed running: %+v", expired)
	}
	interrupted := false
	for _, attempt := range expired.Finished {
		interrupted = interrupted || attempt.Outcome == "interrupted"
	}
	if !interrupted {
		t.Fatal("expired running attempt did not report interruption")
	}

}
