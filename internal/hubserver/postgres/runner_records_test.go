package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProviderAccountLockScopesCurrentReports(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := historyAt()
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		count := 1
		if organization == "org_first" {
			count = 2
		}
		for index := 1; index <= count; index++ {
			runnerID := fmt.Sprintf("runner_%d", index)
			machineID := fmt.Sprintf("machine_%d", index)
			seedRunnerIdentity(t, repository, organization, runnerID, machineID, now, true)
			maxConcurrent := 6
			if organization == "org_second" {
				maxConcurrent = 2
			}
			if index == 1 && organization == "org_first" {
				maxConcurrent = 4
			}
			if index == 2 && organization == "org_first" {
				maxConcurrent = 0
			}
			report := providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: maxConcurrent, Availability: "available", ObservedAt: now, Models: []string{"example-model"}}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerProviderReports(t.Context(), runnerID, []providercapacity.Report{report})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, check := range []struct {
		organization tracker.OrganizationID
		key          runnerstore.ProviderAccountKey
		wantSelected bool
		wantCapacity int
		wantState    string
	}{
		{organization: "org_first", key: runnerstore.ProviderAccountKey{RunnerID: "runner_2", Provider: "example-provider", Backend: "example", AccountAlias: "account_shared", SharedAccountAlias: "shared_account"}, wantSelected: true, wantCapacity: 4, wantState: "available"},
		{organization: "org_second", key: runnerstore.ProviderAccountKey{RunnerID: "runner_1", Provider: "example-provider", Backend: "example", AccountAlias: "account_shared", SharedAccountAlias: "shared_account"}, wantSelected: true, wantCapacity: 2, wantState: "available"},
		{organization: "org_first", key: runnerstore.ProviderAccountKey{RunnerID: "runner_2", Provider: "example-provider", Backend: "example", AccountAlias: "account_shared"}, wantSelected: true, wantCapacity: 0, wantState: "available"},
		{organization: "org_first", key: runnerstore.ProviderAccountKey{RunnerID: "runner_2", Provider: "example-provider", Backend: "other", AccountAlias: "account_shared", SharedAccountAlias: "shared_account"}, wantSelected: false, wantCapacity: 4, wantState: "available"},
	} {
		err := repository.WriteTenant(t.Context(), check.organization, corestore.Serving, func(w tenantstore.Writer) error {
			capacity, err := w.LockRunnerProviderAccount(t.Context(), check.key, now)
			if err != nil {
				return err
			}
			if capacity.SelectedReport != check.wantSelected || capacity.MaxConcurrent != check.wantCapacity || capacity.State != check.wantState {
				t.Errorf("provider account capacity=%+v, want selected=%t max_concurrent=%d state=%q", capacity, check.wantSelected, check.wantCapacity, check.wantState)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		name      string
		mutate    func() providercapacity.Report
		wantState string
	}{
		{name: "current exhausted dominates available", mutate: func() providercapacity.Report {
			return providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: 4, Availability: "exhausted", ObservedAt: now, Models: []string{"example-model"}}
		}, wantState: "exhausted"},
		{name: "stale exhausted is unknown", mutate: func() providercapacity.Report {
			return providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: 4, Availability: "exhausted", ObservedAt: now.Add(-3 * time.Minute), Models: []string{"example-model"}}
		}, wantState: "unknown"},
		{name: "future report is unknown", mutate: func() providercapacity.Report {
			return providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: 4, Availability: "available", ObservedAt: now.Add(time.Minute), Models: []string{"example-model"}}
		}, wantState: "unknown"},
		{name: "reset report is unknown", mutate: func() providercapacity.Report {
			return providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: 4, Availability: "available", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(-time.Second), Models: []string{"example-model"}}
		}, wantState: "unknown"},
	} {
		t.Run(check.name, func(t *testing.T) {
			err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.ReplaceRunnerProviderReports(t.Context(), "runner_1", []providercapacity.Report{check.mutate()}); err != nil {
					return err
				}
				capacity, err := w.LockRunnerProviderAccount(t.Context(), runnerstore.ProviderAccountKey{RunnerID: "runner_2", Provider: "example-provider", Backend: "example", AccountAlias: "account_shared", SharedAccountAlias: "shared_account"}, now)
				if err != nil {
					return err
				}
				if !capacity.SelectedReport || capacity.MaxConcurrent != 4 || capacity.State != check.wantState {
					t.Errorf("provider account capacity=%+v, want selected=true max_concurrent=4 state=%q", capacity, check.wantState)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerProjectUsageCountsLiveRunnerAndProjectLeases(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	const organization tracker.OrganizationID = "org_usage"
	seedLeaseFamily(t, repository, organization)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 2, Title: "Example"})
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ID: 4, ProjectID: "prj_1", NativeID: "wi_4", Number: 3, Title: "Example"})
		if err != nil {
			return err
		}
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_3", Hostname: "example", Capacity: 1, Version: "example"})
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	leases := []leasestore.Lease{
		claimLease(t, repository, organization, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "machine_1", SessionID: "usage_1", LeaseID: "lease_usage_1", TTL: time.Hour}),
		claimLease(t, repository, organization, leasestore.Claim{IssueID: 3, ExpectedRevision: 1, MachineID: "machine_2", SessionID: "usage_2", LeaseID: "lease_usage_2", TTL: time.Hour}),
		claimLease(t, repository, organization, leasestore.Claim{IssueID: 4, ExpectedRevision: 1, MachineID: "machine_3", SessionID: "usage_3", LeaseID: "lease_usage_3", TTL: time.Hour}),
	}
	for index, lease := range leases {
		runnerID := fmt.Sprintf("runner_%d", index+1)
		seedRunnerIdentity(t, repository, organization, runnerID, string(lease.MachineID), now, false)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		runner  string
		project tracker.ProjectID
		now     time.Time
		wantRun int64
		wantPrj int64
	}{
		{runner: "runner_2", project: "prj_1", now: now, wantRun: 1, wantPrj: 1},
		{runner: "runner_2", project: "prj_1", now: now.Add(2 * time.Hour), wantRun: 0, wantPrj: 0},
	} {
		err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			runnerUsed, projectUsed, err := r.RunnerProjectUsage(t.Context(), check.runner, check.project, check.now)
			if err != nil {
				return err
			}
			if runnerUsed != check.wantRun || projectUsed != check.wantPrj {
				t.Errorf("usage runner=%d project=%d, want runner=%d project=%d", runnerUsed, projectUsed, check.wantRun, check.wantPrj)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerHasActiveLeaseIsBoundedAndTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	first := tracker.OrganizationID("org_active_lease_first")
	second := tracker.OrganizationID("org_active_lease_second")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, organization := range []tracker.OrganizationID{first, second} {
		seedTenantAccess(t, repository, organization)
		seedLeaseFamily(t, repository, organization)
	}
	for _, runner := range []struct {
		organization tracker.OrganizationID
		runner       string
		machine      string
	}{
		{organization: first, runner: "runner_active", machine: "machine_1"},
		{organization: first, runner: "runner_released", machine: "machine_2"},
		{organization: second, runner: "runner_cross", machine: "machine_1"},
		{organization: second, runner: "runner_expired", machine: "machine_2"},
	} {
		seedRunnerIdentity(t, repository, runner.organization, runner.runner, runner.machine, now, false)
	}
	assign := func(organization tracker.OrganizationID, lease leasestore.Lease, runner string) {
		t.Helper()
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: lease.AcquiredAt})
		}); err != nil {
			t.Fatal(err)
		}
	}
	released := claimLease(t, repository, first, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "machine_2", SessionID: "released_session", LeaseID: "lease_released", TTL: time.Hour})
	assign(first, released, "runner_released")
	if err := repository.WriteLeases(t.Context(), first, corestore.Serving, func(w leasestore.Writer) error {
		_, err := w.Release(t.Context(), leaseFence(released), "example release")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	activeFirst := claimLease(t, repository, first, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "machine_1", SessionID: "active_session_1", LeaseID: "lease_active_1", TTL: time.Hour})
	activeSecond := claimLease(t, repository, first, leasestore.Claim{IssueID: 2, ExpectedRevision: 1, MachineID: "machine_1", SessionID: "active_session_2", LeaseID: "lease_active_2", TTL: time.Hour})
	assign(first, activeFirst, "runner_active")
	assign(first, activeSecond, "runner_active")
	crossTenant := claimLease(t, repository, second, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "machine_1", SessionID: "cross_session", LeaseID: "lease_cross", TTL: time.Hour})
	assign(second, crossTenant, "runner_cross")
	expired := claimLease(t, repository, second, leasestore.Claim{IssueID: 2, ExpectedRevision: 1, MachineID: "machine_2", SessionID: "expired_session", LeaseID: "lease_expired", TTL: time.Hour})
	assign(second, expired, "runner_expired")
	expireLeaseFixture(t, admin, second, expired.LeaseID)
	for _, check := range []struct {
		name         string
		organization tracker.OrganizationID
		runner       string
		want         bool
	}{
		{name: "multiple live leases", organization: first, runner: "runner_active", want: true},
		{name: "released only", organization: first, runner: "runner_released", want: false},
		{name: "expired only", organization: second, runner: "runner_expired", want: false},
		{name: "same runner ID in other tenant", organization: first, runner: "runner_cross", want: false},
		{name: "live lease in owning tenant", organization: second, runner: "runner_cross", want: true},
	} {
		t.Run(check.name, func(t *testing.T) {
			if err := repository.ReadTenant(t.Context(), check.organization, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.RunnerHasActiveLease(t.Context(), check.runner, now)
				if err != nil {
					return err
				}
				if got != check.want {
					t.Errorf("RunnerHasActiveLease(%q) = %t, want %t", check.runner, got, check.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerSharedProviderAccountSerializesFirstReportJoin(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	const organization tracker.OrganizationID = "org_shared_account"
	now := time.Now().UTC()
	seedRunnerIdentity(t, repository, organization, "runner_first", "machine_first", now, true)
	seedRunnerIdentity(t, repository, organization, "runner_second", "machine_second", now, true)
	report := providercapacity.Report{Backend: "example", Provider: "example-provider", AccountAlias: "account_shared", SharedAccountAlias: "shared_account", Billing: "api_key", MaxConcurrent: 4, Availability: "available", ObservedAt: now, Models: []string{"example-model"}}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerProviderReports(t.Context(), "runner_first", []providercapacity.Report{report})
	}); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	lookup := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.LockRunnerProviderAccount(ctx, runnerstore.ProviderAccountKey{RunnerID: "runner_second", Provider: report.Provider, Backend: report.Backend, AccountAlias: report.AccountAlias, SharedAccountAlias: report.SharedAccountAlias}, now); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	waitRepositorySignal(t, locked)
	update := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ReplaceRunnerProviderReports(ctx, "runner_second", []providercapacity.Report{report})
		})
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-update.done:
			t.Fatalf("shared account report update completed before acquiring the claim's account lock: %v", update.err)
		default:
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting == 0 {
			return fmt.Errorf("shared account report update is not waiting on the claim's account lock")
		}
		return nil
	})
	close(release)
	if err := lookup.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := update.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		capacity, err := w.LockRunnerProviderAccount(t.Context(), runnerstore.ProviderAccountKey{RunnerID: "runner_second", Provider: report.Provider, Backend: report.Backend, AccountAlias: report.AccountAlias, SharedAccountAlias: report.SharedAccountAlias}, now)
		if err != nil {
			return err
		}
		if !capacity.SelectedReport || capacity.MaxConcurrent != 4 || capacity.State != "available" {
			t.Fatalf("shared account capacity=%+v, want selected report with max concurrency 4 available", capacity)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func seedRunnerIdentity(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID, runnerID, machineID string, now time.Time, registerMachine bool) {
	t.Helper()
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if registerMachine {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: runnerID, Name: runnerID, Hash: apikey.HashToken("credential_" + string(organization) + "_" + runnerID), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: timePointer(now.Add(time.Hour)), ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + runnerID, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("enrollment_" + string(organization) + "_" + runnerID), Scope: "organization", Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, runnerID, tracker.MachineID(machineID), now); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: runnerID, EnrollmentID: enrollment.ID, Scope: "organization", Operations: enrollment.Operations, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetRunnerUrgentUpdateRejectsNonzeroRevisionWithoutCurrentRow(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organization := tracker.OrganizationID("org_urgent_cas")
	now := historyAt()
	seedTenantAccess(t, repository, organization)
	seedRunnerIdentity(t, repository, organization, "runner_urgent_cas", "machine_urgent_cas", now, true)
	request := runnerauth.UpdateRequest{ID: "urgent_request", RequestedAt: now, Service: "detent", Version: "1.2.3", Release: true, Urgent: true}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: organization, Request: request})
	}); err != nil {
		t.Fatal(err)
	}
	noncanonical := request
	noncanonical.ID = "urgent_submicro"
	noncanonical.RequestedAt = now.Add(time.Nanosecond)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: organization, Request: noncanonical})
	}); !errors.Is(err, runnerstore.ErrInvalidRecord) {
		t.Fatalf("create update request with sub-microsecond time: %v", err)
	}
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRunnerUrgentUpdate(t.Context(), 1, request.ID, now)
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("set missing urgent revision with expected revision 1: %v", err)
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.RunnerUrgentUpdate(t.Context())
		if err != nil {
			return err
		}
		if current.Revision != 0 || current.Request != nil {
			t.Fatalf("missing-row conflict created urgent state: %+v", current)
		}
		if _, err := r.RunnerUpdateRequest(t.Context(), request.ID); err != nil {
			return err
		}
		if _, err := r.RunnerUpdateRequest(t.Context(), noncanonical.ID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("sub-microsecond request was stored: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerIdentityEnrollmentRevocationIsTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	now := historyAt()
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: organization, ID: 1, ProjectID: "project_shared", NativeID: "wi_shared", Number: 1, Title: "Example"}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: "runner_shared", Name: "runner_shared", Hash: apikey.HashToken("credential_" + string(organization)), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: timePointer(now.Add(24 * time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: "runner_shared", ProjectID: "project_shared"}); err != nil {
				return err
			}
			return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", DisplayName: "Example", Capacity: 1, Version: "example"})
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("enroll runner: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_shared", RunnerID: "runner_shared", MachineID: "machine_shared", TokenHash: apikey.HashToken("enrollment-token-" + string(organization)), Scope: "projects", Operations: []string{"read", "claim"}, ProjectIDs: []tracker.ProjectID{"project_shared"}, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), CreatedBy: "admin"})
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("create redeemed identity: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			enrollment, err := w.RunnerEnrollmentByTokenHash(ctx, apikey.HashToken("enrollment-token-"+string(organization)))
			if err != nil {
				return fmt.Errorf("read enrollment: %w", err)
			}
			if enrollment.ID != "enrollment_shared" || len(enrollment.ProjectIDs) != 1 || enrollment.ProjectIDs[0] != "project_shared" {
				t.Fatalf("enrollment=%+v", enrollment)
			}
			if err := w.RedeemRunnerEnrollment(ctx, enrollment.ID, "runner_shared", "machine_shared", now); err != nil {
				return fmt.Errorf("redeem enrollment: %w", err)
			}
			if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: organization, RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: enrollment.ID, Scope: enrollment.Scope, Operations: enrollment.Operations, DisplayName: "Example", CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), LastHeartbeatAt: now}); err != nil {
				return fmt.Errorf("create runner identity: %w", err)
			}
			if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: "runner_shared", ActorID: "admin", Kind: "enrolled", OccurredAt: now}); err != nil {
				return fmt.Errorf("record identity event: %w", err)
			}
			return nil
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("redeem runner: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
	}

	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: "org_first", RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: "enrollment_shared", Scope: "projects", Operations: []string{"read"}, DisplayName: "duplicate", CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now})
	}); !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("duplicate runner identity error=%v", err)
	}

	for _, organization := range organizations {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			identity, err := r.RunnerIdentityByCredential(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if identity.OrganizationID != organization || identity.RunnerID != "runner_shared" {
				t.Fatalf("identity escaped organization: %+v", identity)
			}
			projects, err := r.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
			if err != nil {
				return err
			}
			if len(projects) != 1 || projects[0] != "project_shared" {
				t.Fatalf("projects=%v", projects)
			}
			projectionReader, ok := r.(runnerstore.UpdateProjectionPageReader)
			if !ok {
				t.Fatal("tenant reader does not expose the typed update projection")
			}
			projection, err := projectionReader.RunnerUpdateProjectionPage(ctx, runnerstore.UpdateProjectionCursor{}, 1)
			if err != nil || len(projection) != 1 || projection[0].RunnerID != "runner_shared" || projection[0].DisplayName != "Example" || projection[0].Version != "example" || !projection[0].LastHeartbeat.Equal(now) {
				t.Fatalf("projection=%+v error=%v", projection, err)
			}
			projection, err = projectionReader.RunnerUpdateProjectionPage(ctx, runnerstore.UpdateProjectionCursor{DisplayName: "Example", RunnerID: "runner_shared"}, 1)
			if err != nil || len(projection) != 0 {
				t.Fatalf("projection after cursor=%+v error=%v", projection, err)
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, "project_shared", now.Add(time.Minute))
			if err != nil || !allowed {
				t.Fatalf("authorized=%t error=%v", allowed, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.RevokeAccessCredential(ctx, "runner_shared", now.Add(time.Minute)); err != nil {
			return err
		}
		return w.RemoveRunnerIdentity(ctx, "runner_shared", now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		organization tracker.OrganizationID
		want         bool
	}{{"org_first", false}, {"org_second", true}} {
		if err := repository.ReadTenant(ctx, check.organization, corestore.Serving, func(r tenantstore.Reader) error {
			allowed, err := r.RunnerProjectAuthorized(ctx, "runner_shared", "project_shared", now.Add(2*time.Minute))
			if err != nil {
				return err
			}
			if allowed != check.want {
				t.Fatalf("organization %s authorization=%t want=%t", check.organization, allowed, check.want)
			}
			if check.organization == "org_first" {
				page, err := r.(runnerstore.UpdateProjectionPageReader).RunnerUpdateProjectionPage(ctx, runnerstore.UpdateProjectionCursor{}, 1)
				if err != nil || len(page) != 0 {
					t.Fatalf("revoked runner projection=%+v error=%v", page, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestRunnerProjectObservationIsTenantScopedAndRejectsStaleSnapshots(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	ctx := t.Context()
	now := historyAt()
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), Local: true, CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: "runner_shared", Name: "runner_shared", Hash: apikey.HashToken("credential_" + string(organization)), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: timePointer(now.Add(time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_shared", RunnerID: "runner_shared", MachineID: "machine_shared", TokenHash: apikey.HashToken("enrollment_" + string(organization)), Scope: "projects", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: organization, RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: "enrollment_shared", Scope: "projects", Operations: []string{"read"}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			identity, err := w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			identity.State = "draining"
			identity.Tags = []string{"linux", "example"}
			identity.MachineUse = "dedicated"
			identity.IsolationTier = "native-trusted"
			identity.HostServices = []string{"git"}
			identity.Availability = runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 2}, HardDeadline: "30m"}
			identity.ProjectCapacityLimits = map[tracker.ProjectID]int{"project_shared": 3}
			identity.ProjectRanks = map[tracker.ProjectID]int{"project_shared": 7}
			if err := w.UpdateRunnerRouting(ctx, identity.Revision, identity); err != nil {
				return err
			}
			localAttempt := int64(5)
			if organization == "org_second" {
				localAttempt = 6
			}
			observedAt, diagnosticsAt, runtimeAt := now.Add(2*time.Minute), now.Add(time.Minute), now.Add(90*time.Second)
			record := runnerauth.DiagnosticAttempt{Key: fmt.Sprintf("local:%d", localAttempt), IssueID: "wi_shared", LocalAttemptID: localAttempt, NativeAttemptID: "attempt_shared", Generation: 3, Fence: 4, Stage: "implementing", Membership: []string{"running", "native_claim", "durable_active"}, ProviderCompletedAt: now, HostOperation: "workspace.finalize_native_work", OperationPending: true, OperationStartedAt: now.Add(-time.Second), OperationObservedAt: now, LatestErrorCode: "invalid_request", LatestError: "context deadline exceeded [private error text omitted]", DeferredAt: now.Add(-2 * time.Second), RetryAt: now.Add(time.Second), RetryAttempt: 2, RecoveryOwner: "orchestrator.deferred_completion", LeaseRenewedAt: runtimeAt, LeaseExpiresAt: now.Add(time.Minute), Unavailable: map[string]string{"native_attempt_id": "unrecorded"}}
			diagnostics := &runnerauth.ProjectDiagnostics{RuntimeObservedAt: runtimeAt, ObservedAt: diagnosticsAt, Source: "runner_runtime_and_durable_attempt_owners", RunningBuild: &runnerauth.BuildEvidence{Version: "v0.177.4", Commit: "abcdef1234567", Source: "development", OS: "darwin", Architecture: "arm64", ObservedAt: now}, Records: []runnerauth.DiagnosticAttempt{record}, Admissions: []runnerauth.DiagnosticAdmission{{IssueID: "wi_shared", WorkItemRevision: 7, ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full", RepositoryReservations: []string{"repository_example"}, HostLandingOccupancy: 3, ProviderRequirement: &providercapacity.Requirement{Role: "implementer", Backend: "codex", Model: "example-model"}, Unavailable: map[string]string{"local_attempt_id": "unrecorded"}}}, Counts: map[string]int{"running": 1, "native_claim": 1}, Truncated: true, Unavailable: map[string]string{"running_build": "unavailable"}}
			attempt := runnerstore.ProjectDiagnosticAttempt{Position: 0, RecordKey: record.Key, IssueID: record.IssueID, LocalAttemptID: record.LocalAttemptID, NativeAttemptID: record.NativeAttemptID, Generation: int64(record.Generation), Fence: record.Fence, Stage: record.Stage, Running: true, NativeClaim: true, ProviderCompletedAt: &record.ProviderCompletedAt, HostOperation: record.HostOperation, OperationPending: true, OperationStartedAt: &record.OperationStartedAt, OperationObservedAt: &record.OperationObservedAt, LatestErrorCode: record.LatestErrorCode, LatestError: record.LatestError, DeferredAt: &record.DeferredAt, RetryAt: &record.RetryAt, RetryAttempt: record.RetryAttempt, RecoveryOwner: record.RecoveryOwner, LeaseRenewedAt: &record.LeaseRenewedAt, LeaseExpiresAt: &record.LeaseExpiresAt, Membership: record.Membership, Unavailable: record.Unavailable}
			return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: organization, RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: observedAt, DiagnosticsAt: &diagnosticsAt, RuntimeObservedAt: &runtimeAt, SnapshotDigest: strings.Repeat("a", 64), Diagnostics: diagnostics}, []runnerstore.ProjectDiagnosticAttempt{attempt})
		}); err != nil {
			t.Fatalf("seed observation for %s: %v", organization, err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_race", Name: "Race", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
			return err
		}); err != nil {
			t.Fatalf("seed race project for %s: %v", organization, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			observation, attempts, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_shared", "wi_shared")
			if err != nil {
				return err
			}
			wantLocalAttempt := int64(5)
			if organization == "org_second" {
				wantLocalAttempt = 6
			}
			if observation.OrganizationID != organization || observation.SnapshotDigest != strings.Repeat("a", 64) || observation.RuntimeObservedAt == nil || !observation.RuntimeObservedAt.Equal(now.Add(90*time.Second)) || len(attempts) != 1 || attempts[0].LocalAttemptID != wantLocalAttempt || attempts[0].NativeAttemptID != "attempt_shared" || attempts[0].Fence != 4 || !attempts[0].Running || !attempts[0].OperationPending {
				t.Fatalf("organization %s observation=%+v attempts=%+v", organization, observation, attempts)
			}
			if observation.Diagnostics == nil || observation.Diagnostics.Source != "runner_runtime_and_durable_attempt_owners" || !observation.Diagnostics.Truncated || observation.Diagnostics.Counts["running"] != 1 || observation.Diagnostics.Unavailable["running_build"] != "unavailable" || observation.Diagnostics.RunningBuild == nil || observation.Diagnostics.RunningBuild.Commit != "abcdef1234567" || len(observation.Diagnostics.Admissions) != 1 || observation.Diagnostics.Admissions[0].ProviderRequirement == nil || observation.Diagnostics.Admissions[0].ProviderRequirement.Model != "example-model" {
				t.Fatalf("organization %s diagnostics metadata=%+v", organization, observation.Diagnostics)
			}
			record := observation.Diagnostics.Records[0]
			if !slices.Equal(record.Membership, []string{"running", "native_claim", "durable_active"}) || record.OperationStartedAt.IsZero() || record.OperationObservedAt.IsZero() || record.LatestErrorCode != "invalid_request" || record.LatestError != "context deadline exceeded [private error text omitted]" || record.DeferredAt.IsZero() || record.RetryAt.IsZero() || record.RetryAttempt != 2 || record.RecoveryOwner != "orchestrator.deferred_completion" || record.LeaseExpiresAt.IsZero() || record.Unavailable["native_attempt_id"] != "unrecorded" || observation.Diagnostics.Admissions[0].Unavailable["local_attempt_id"] != "unrecorded" {
				t.Fatalf("organization %s diagnostic details=%+v admissions=%+v", organization, record, observation.Diagnostics.Admissions)
			}
			identity, err := r.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if identity.State != "draining" || identity.Revision != 2 || len(identity.Tags) != 2 || identity.MachineUse != "dedicated" || identity.IsolationTier != "native-trusted" || len(identity.HostServices) != 1 || identity.Availability.Timezone != "UTC" || len(identity.Availability.Windows) != 1 || identity.Availability.WindowSlots[identity.Availability.Windows[0]] != 2 || identity.ProjectCapacityLimits["project_shared"] != 3 || identity.ProjectRanks["project_shared"] != 7 {
				t.Fatalf("organization %s routing identity=%+v", organization, identity)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(ctx, "runner_shared")
		if err != nil {
			return err
		}
		identity.Revision--
		return w.UpdateRunnerRouting(ctx, identity.Revision, identity)
	}); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale runner routing revision error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(ctx, "runner_shared")
		if err != nil {
			return err
		}
		identity.ProjectRanks = make(map[tracker.ProjectID]int, runnerstore.MaxRunnerProjectRanks+1)
		for i := range runnerstore.MaxRunnerProjectRanks + 1 {
			identity.ProjectRanks[tracker.ProjectID(fmt.Sprintf("project_%03d", i))] = i
		}
		return w.UpdateRunnerRouting(ctx, identity.Revision, identity)
	}); !errors.Is(err, runnerstore.ErrInvalidRecord) {
		t.Fatalf("oversized runner project ranks error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(time.Minute), SnapshotDigest: strings.Repeat("b", 64)}, nil)
	}); !errors.Is(err, runnerstore.ErrStaleObservation) {
		t.Fatalf("stale project observation error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		staleDiagnosticsAt := now
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(3 * time.Minute), DiagnosticsAt: &staleDiagnosticsAt, SnapshotDigest: strings.Repeat("b", 64)}, nil)
	}); err != nil {
		t.Fatalf("later receipt with stale diagnostics error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		newerDiagnosticsAt, staleRuntimeAt := now.Add(2*time.Minute), now.Add(time.Minute)
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(4 * time.Minute), DiagnosticsAt: &newerDiagnosticsAt, RuntimeObservedAt: &staleRuntimeAt, SnapshotDigest: strings.Repeat("c", 64)}, nil)
	}); err != nil {
		t.Fatalf("later diagnostics with stale runtime error=%v", err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		observation, attempts, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_shared", "wi_shared")
		if err != nil {
			return err
		}
		if !observation.ObservedAt.Equal(now.Add(2*time.Minute)) || observation.DiagnosticsAt == nil || !observation.DiagnosticsAt.Equal(now.Add(time.Minute)) || observation.RuntimeObservedAt == nil || !observation.RuntimeObservedAt.Equal(now.Add(90*time.Second)) || observation.SnapshotDigest != strings.Repeat("a", 64) || len(attempts) != 1 || attempts[0].LocalAttemptID != 5 {
			t.Fatalf("stale write replaced current project observation: %+v attempts=%+v", observation, attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.ExecContext(ctx, `CREATE FUNCTION p12_runner_observation_write_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(EXTRACT(EPOCH FROM NEW.observed_at)::bigint); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE TRIGGER p12_runner_observation_write_barrier BEFORE INSERT ON public.runner_project_observations FOR EACH ROW EXECUTE FUNCTION p12_runner_observation_write_barrier()`); err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(2)
	lockConnection, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockConnection.Close() })
	olderAt, newerAt := now.Add(4*time.Minute), now.Add(5*time.Minute)
	for _, at := range []time.Time{olderAt, newerAt} {
		if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, at.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	diagnosticsAt := now.Add(3 * time.Minute)
	writeObservation := func(at time.Time) func(context.Context) error {
		return func(writeCtx context.Context) error {
			return repository.WriteTenant(writeCtx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerProjectObservation(writeCtx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_race", ObservedAt: at, DiagnosticsAt: &diagnosticsAt, SnapshotDigest: strings.Repeat("c", 64)}, nil)
			})
		}
	}
	olderTask := startRepositoryTask(t, writeObservation(olderAt))
	newerTask := startRepositoryTask(t, writeObservation(newerAt))
	testenv.WaitUntil(t, ctx, func() error {
		var waiting int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO runner_project_observations%'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting != 2 {
			return fmt.Errorf("observation insert waiters = %d, want 2", waiting)
		}
		return nil
	})
	if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, newerAt.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := newerTask.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, olderAt.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := olderTask.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		observation, _, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_race", "")
		if err != nil {
			return err
		}
		if !observation.ObservedAt.Equal(newerAt) || observation.SnapshotDigest != strings.Repeat("c", 64) {
			t.Fatalf("concurrent first-insert result = %+v, want newer observation", observation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `UPDATE runner_project_observations SET diagnostics_source='' WHERE organization_id='org_second' AND runner_id='runner_shared' AND project_id='project_shared'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		observation, attempts, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_shared", "wi_shared")
		if err != nil {
			return err
		}
		if observation.Diagnostics != nil || len(attempts) != 0 {
			t.Fatalf("legacy sparse diagnostics were promoted to a complete snapshot: observation=%+v attempts=%+v", observation, attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerHeartbeatObservationsAreTenantScoped(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	ctx := t.Context()
	now := historyAt()
	hour := now.UTC().Truncate(time.Hour)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRunnerIdentity(t, repository, organization, "runner_shared", "machine_shared", now, true)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
				return err
			}
			cpuBusy := float64(len(organization))
			metric := hostmetrics.Summary{Hour: hour, SegmentID: now.Add(-time.Minute), SampleCount: 1, LogicalCores: 2, CPUSampleCount: 1, CPUBusySumPercent: cpuBusy, CPUBusyMaxPercent: cpuBusy}
			if _, err := w.AppendRunnerHostMetricSegments(ctx, "runner_shared", []hostmetrics.Summary{metric}, now); err != nil {
				return err
			}
			return w.ReplaceRunnerAdmissionObservation(ctx, "runner_shared", "project_shared", tracker.NativeAdmissionObservation{
				RunnerRevision: 1,
				ReceivedAt:     now,
				Context: tracker.NativeAdmissionContext{
					ObservedAt: now, PolicyID: "policy_" + string(organization), LastRefreshAt: now,
					RefreshDuration: time.Second, WorkflowStates: []string{"Todo"},
				},
			})
		}); err != nil {
			t.Fatalf("write observations for %s: %v", organization, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			metrics, err := r.RunnerHostMetrics(ctx, "runner_shared", hour, hour.Add(time.Hour))
			if err != nil {
				return err
			}
			admission, err := r.RunnerAdmissionObservation(ctx, "runner_shared", "project_shared")
			if err != nil {
				return err
			}
			if len(metrics) != 1 || metrics[0].CPUBusySumPercent != float64(len(organization)) || admission.Context.PolicyID != "policy_"+string(organization) || len(admission.Context.WorkflowStates) != 1 || admission.Context.WorkflowStates[0] != "Todo" {
				t.Errorf("organization %s metrics=%+v admission=%+v", organization, metrics, admission)
			}
			return nil
		}); err != nil {
			t.Fatalf("read observations for %s: %v", organization, err)
		}
	}
}

func TestRunnerHostMetricRetentionUsesBoundedTenantBatch(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	now := historyAt()
	firstHour := now.UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRunnerIdentity(t, repository, organization, "runner_shared", "machine_shared", now, true)
		segments := make([]hostmetrics.Summary, 0, 3)
		for offset := range 3 {
			hour := firstHour.Add(time.Duration(offset) * time.Hour)
			segments = append(segments, hostmetrics.Summary{Hour: hour, SegmentID: hour.Add(-time.Minute), SampleCount: 1, LogicalCores: 2})
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.AppendRunnerHostMetricSegments(ctx, "runner_shared", segments, now)
			return err
		}); err != nil {
			t.Fatalf("seed host metrics for %s: %v", organization, err)
		}
	}
	connection, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `SET enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	plan, err := connection.QueryContext(ctx, `EXPLAIN (COSTS OFF) SELECT ctid FROM runner_host_metric_segments WHERE organization_id=$1 AND hour <= $2 ORDER BY hour,runner_id,segment_id LIMIT $3`, "org_first", firstHour.Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	var planLines []string
	for plan.Next() {
		var line string
		if err := plan.Scan(&line); err != nil {
			plan.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := errors.Join(plan.Err(), plan.Close()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(planLines, "\n"), "runner_host_metric_segments_retention_idx") {
		t.Fatalf("bounded retention query plan does not use its indexed cursor: %v", planLines)
	}
	cutoff := firstHour.Add(time.Hour)
	if err := repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error {
		return w.PruneRunnerHostMetrics(ctx, cutoff, 1)
	}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		organization tracker.OrganizationID
		want         int
	}{
		{organization: "org_first", want: 2},
		{organization: "org_second", want: 3},
	} {
		if err := repository.ReadTenant(ctx, check.organization, corestore.Serving, func(r tenantstore.Reader) error {
			metrics, err := r.RunnerHostMetrics(ctx, "runner_shared", firstHour.Add(-time.Hour), firstHour.Add(4*time.Hour))
			if err != nil {
				return err
			}
			if len(metrics) != check.want {
				t.Errorf("organization %s retained %d segments, want %d", check.organization, len(metrics), check.want)
			}
			return nil
		}); err != nil {
			t.Fatalf("read retained host metrics for %s: %v", check.organization, err)
		}
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error {
		return w.PruneRunnerHostMetrics(ctx, cutoff, runnerstore.MaxRunnerHostPruneBatch+1)
	}); !errors.Is(err, corestore.ErrInvalidPage) {
		t.Fatalf("oversized host metric prune batch error=%v", err)
	}
}
