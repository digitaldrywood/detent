package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresFleetReadsNamesCapacityLeasesAndHistoryWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	asOf := now
	historyHour := now.Truncate(time.Hour)
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }, Version: "0.177.4", Hosted: &HostedConfig{OrganizationID: "org_first"}}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return fmt.Errorf("CreateAccessOrganization: %w", err)
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "credential_owner", Name: "Example owner", Hash: apikey.HashToken(string(org) + "_owner"), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_example", MembershipID: "membership_example", ProjectAccess: "all", Kind: "personal"}); err != nil {
				return fmt.Errorf("create owner: %w", err)
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "user_example", Email: "example@example.test", MembershipID: "membership_example", Role: "owner", Active: true, PrincipalID: "credential_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
				return fmt.Errorf("CreateAccessMember: %w", err)
			}
			for index := 1; index <= 2; index++ {
				id := fmt.Sprintf("%d", index)
				project := tracker.ProjectID("project_" + id)
				runner := "runner_" + id
				machine := "machine_" + id
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + id, Slug: "example-" + id, Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return fmt.Errorf("CreateProjectRecord: %w", err)
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "user_example", ProjectID: project, CanWrite: true, ManageRunner: true}); err != nil {
					return fmt.Errorf("CreateAccessProjectGrant: %w", err)
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: runner, Name: runner, Hash: apikey.HashToken(string(org) + "_" + runner), Scope: "worker", NativeOnly: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
					return fmt.Errorf("Add: %w", err)
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: machine, Hostname: "example-" + string(org) + "-" + id, Capacity: 4, Version: "0.177.4"}); err != nil {
					return fmt.Errorf("RegisterMachine: %w", err)
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment_" + runner, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enroll_" + string(org) + runner), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), CreatedBy: "example-owner"}); err != nil {
					return fmt.Errorf("Add: %w", err)
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: runner, EnrollmentID: "enrollment_" + runner, Scope: "organization", State: "active", DisplayName: "Example " + id, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 3, ReportedCapacity: 2, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
					return fmt.Errorf("Add: %w", err)
				}
				if err := w.ReplaceRunnerProviderReports(t.Context(), runner, []providercapacity.Report{{Backend: "example", Provider: "example", AccountAlias: "account", Billing: "api_key", Models: []string{"example-model"}, Availability: "available", ObservedAt: now, MaxConcurrent: 2}}); err != nil {
					return fmt.Errorf("ReplaceRunnerProviderReports: %w", err)
				}
				if err := w.UpdateRunnerCapacityConfig(t.Context(), runner, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now}); err != nil {
					return fmt.Errorf("UpdateRunnerCapacityConfig: %w", err)
				}
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID("item_" + id), Number: 1, Title: "Example issue " + id})
				if err != nil {
					return fmt.Errorf("CreateWorkItem: %w", err)
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: machine, SessionID: "session_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
				if err != nil {
					return fmt.Errorf("Claim: %w", err)
				}
				if lease.RenewedAt.After(asOf) {
					asOf = lease.RenewedAt
				}
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: runner, LeaseID: lease.LeaseID, AssignedAt: now}); err != nil {
					return fmt.Errorf("AssignRunnerLease: %w", err)
				}
				report := providercapacity.Report{Backend: "example", Provider: "example", AccountAlias: "account", Billing: "api_key", Models: []string{"example-model"}, Availability: "available", ObservedAt: now, MaxConcurrent: 2}
				if err := w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: org, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "example", Model: "example-model"}, Report: report, Reason: "Example capacity"}}); err != nil {
					return err
				}
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_" + id, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run_" + id, Sequence: 1, Status: "running", PayloadObjectKey: "example/start", StartedAt: now}); err != nil {
					return err
				}
				value := tracker.CollaborationEvent{ID: "decision_" + id, OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item.NativeID, Type: "scheduler.decision", SchemaVersion: 1, RecordedAt: now, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-scheduler"}, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready", RunnerID: runner, At: now}}}
				raw, err := json.Marshal(value)
				if err != nil {
					return err
				}
				event, err := eventstore.NewContent(details).PrepareEvent(t.Context(), org, raw)
				if err != nil {
					return err
				}
				if _, err := w.AppendEvent(t.Context(), event); err != nil {
					return err
				}

				if _, err := w.AppendRunnerHostMetricSegments(t.Context(), runner, []hostmetrics.Summary{{Hour: historyHour, SegmentID: historyHour, SampleCount: 1, LogicalCores: 4, CPUSampleCount: 1, CPUBusySumPercent: 25, CPUBusyMaxPercent: 25}}, now); err != nil {
					return fmt.Errorf("Add: %w", err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	now = asOf.Add(time.Second)
	credential := apiCredential{HostedRole: "owner", Hosted: &auth.HostedIdentity{Subject: "user_example"}}
	names, err := service.readHostedRunnerNamesTenant(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names["runner_1"].Hostname != "example-org_first-1" || names["runner_2"].DisplayName != "Example 2" {
		t.Fatalf("tenant runner names = %+v", names)
	}
	fleet, err := service.readHostedFleet(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Runners) != 2 {
		t.Fatalf("fleet runners = %+v", fleet.Runners)
	}
	for _, runner := range fleet.Runners {
		if runner.Health != "online" || runner.HostCapacity != 4 || runner.HostUsed != 1 || runner.Capacity == nil || runner.Capacity.Effective == nil || *runner.Capacity.Effective != 2 || len(runner.Leases) != 1 || runner.ProviderCapacity[0].MaxConcurrent != 2 || runner.ProviderCapacity[0].Used != 1 {
			t.Errorf("runner live facts = %+v", runner)
		}
	}
	history, err := service.readHostedRunnerHostHistory(t.Context(), credential, "runner_1", historyHour, historyHour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].CPUBusyAveragePercent == nil || *history[0].CPUBusyAveragePercent != 25 {
		t.Fatalf("runner history = %+v", history)
	}
	report, err := service.readCloudDiagnostics(t.Context(), credential, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Capacity) != 2 || report.Capacity[1].Slots <= 0 || report.Claimed == nil || *report.Claimed != 2 || report.Ready == nil || *report.Ready != 2 || report.Coverage[1].Observed == nil || *report.Coverage[1].Observed != 2 {
		t.Fatalf("recorded diagnostics = %+v", report)
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		for index := range 17 {
			value := tracker.CollaborationEvent{ID: fmt.Sprintf("recent_decision_%02d", index), OrganizationID: "org_first", ProjectID: "project_1", AggregateType: "issue", AggregateID: "item_1", Type: "scheduler.decision", SchemaVersion: 1, RecordedAt: now.Add(-time.Millisecond), Actor: tracker.Actor{Kind: "system", PrincipalID: "example-scheduler"}, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready", At: now}}}
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			event, err := eventstore.NewContent(details).PrepareEvent(t.Context(), "org_first", raw)
			if err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readsBefore := details.reads
	clipped, err := service.readCloudDiagnostics(t.Context(), credential, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !clipped.Partial || clipped.Claimed == nil || *clipped.Claimed != 2 || clipped.Ready == nil || *clipped.Ready != 16 || details.reads-readsBefore != 16 {
		t.Fatalf("bounded diagnostic object reads = %d, report = %+v", details.reads-readsBefore, clipped)
	}

	details.beforeRead = func(context.Context) error { return artifact.ErrMissing }
	missing, err := service.readCloudDiagnostics(t.Context(), credential, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !missing.Partial || missing.Ready != nil || missing.Claimed == nil || *missing.Claimed != 2 {
		t.Fatalf("missing scheduler objects hide retained lease history: %+v", missing)
	}

	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_first", ID: "credential_limited", Name: "Example member", Hash: apikey.HashToken("example_member"), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_limited", MembershipID: "membership_limited", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_first", UserID: "user_limited", Email: "member@example.test", MembershipID: "membership_limited", Role: "member", Active: true, PrincipalID: "credential_limited", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_first", UserID: "user_limited", ProjectID: "project_1", ManageRunner: true}); err != nil {
			return err
		}
		for _, id := range []string{"runner_1", "runner_2"} {
			identity, err := w.RunnerIdentity(t.Context(), id)
			if err != nil {
				return err
			}
			identity.ProjectRanks = map[tracker.ProjectID]int{"project_1": 1, "project_2": 2}
			identity.ProjectCapacityLimits = map[tracker.ProjectID]int{"project_1": 2, "project_2": 1}
			if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"project_1", "project_2"} {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_first", TokenID: id, ProjectID: project}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	limited := apiCredential{HostedRole: "member", Hosted: &auth.HostedIdentity{Subject: "user_limited"}}
	scoped, err := service.readHostedFleet(t.Context(), limited)
	if err != nil {
		t.Fatal(err)
	}
	for _, runner := range scoped.Runners {
		if runner.Routing == nil || len(runner.Routing.ProjectIDs) != 1 || runner.Routing.ProjectIDs[0] != "project_1" || len(runner.Routing.ProjectRanks) != 1 || len(runner.Routing.ProjectCapacityLimits) != 1 || len(runner.Capacity.Projects) != 1 {
			t.Fatalf("member routing exposes inaccessible project facts: %+v", runner)
		}
		if runner.ID == "runner_2" && (len(runner.Leases) != 0 || runner.HostUsed != 0 || runner.ProviderCapacity[0].Used != 0) {
			t.Fatalf("member fleet exposes inaccessible activity: %+v", runner)
		}
	}

	e := echo.New()
	service.registerPostgresAppRoutes(e)
	for _, path := range []string{"/fleet", "/diagnostics", "/diagnostics/requests"} {
		found := false
		for _, route := range e.Routes() {
			if route.Method == http.MethodGet && route.Path == hostedOrganizationBase+path {
				found = true
			}
		}
		if !found {
			t.Errorf("PostgreSQL GET route missing: %s", path)
		}
	}
}

func BenchmarkTenantFleetProviderViewsTenThousandSharedRunners(b *testing.B) {
	now := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	report := providercapacity.Report{Provider: "example", Backend: "example", AccountAlias: "account", SharedAccountAlias: "shared", Models: []string{"example-model"}, MaxConcurrent: 10000, Availability: "available", ObservedAt: now}
	records := make([]runnerstore.FleetRecord, 10000)
	leases := make([]runnerstore.FleetLease, 10000)
	for index := range records {
		id := fmt.Sprintf("runner_%d", index)
		records[index] = runnerstore.FleetRecord{Identity: runnerstore.Identity{RunnerID: id}, CredentialCreatedAt: now.Add(-time.Hour), Reports: []providercapacity.Report{report}}
		leases[index] = runnerstore.FleetLease{RunnerID: id, Reservation: &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "example", Model: "example-model"}, Report: report}}
	}
	b.ReportAllocs()
	for b.Loop() {
		views := tenantFleetProviderViews(records, leases, now)
		if len(views) != len(records) || views["runner_0"][0].Used != 10000 {
			b.Fatal("shared runner capacity population is incomplete")
		}
	}
}
