package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerDiagnosticsUsesTenantOwnerWithoutLegacyAlias(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, now: func() time.Time { return now }}}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: organization, Name: "Example", Slug: string(organization)})
			err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_example", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: organization, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
			page, err := service.readRunnerProjectDiagnostics(t.Context(), scope, operatortool.LocalProjectArguments{ProjectID: "project_example", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if page.Unavailable["runner"] != "not_authorized_or_unreported" {
				t.Fatalf("unavailable=%v", page.Unavailable)
			}
		})
	}
}

func TestRunnerRemovalUsesTenantAuthorityAndActiveLeases(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: "Example", Slug: string(org)})
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project_example", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			for _, id := range []string{"idle", "busy"} {
				expires := now.Add(time.Hour)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: apikey.HashToken(string(org) + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: id, Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken(string(org) + id + "enrollment"), Scope: "projects", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: id, Scope: "projects", State: "active", Operations: []string{runnerauth.Read}, CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project_example", NativeID: "item_example", Number: 1, Title: "Example"})
			if err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: "busy", SessionID: "example", LeaseID: "lease_example", TTL: time.Hour})
			if err != nil {
				return err
			}
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: "busy", LeaseID: lease.LeaseID, AssignedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			scope := nativeScope{organization: org, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
			for _, id := range []string{"busy", "idle"} {
				_, err := service.revokeRunnerIdentityCommand(t.Context(), scope, id)
				if (id == "busy") != (err != nil) {
					t.Fatalf("remove %s: %v", id, err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					identity, err := r.RunnerIdentity(t.Context(), id)
					if err != nil {
						return err
					}
					credential, err := r.AccessCredential(t.Context(), id)
					if err != nil {
						return err
					}
					if (identity.RemovedAt != nil) != (id == "idle") || (credential.RevokedAt != nil) != (id == "idle") {
						t.Fatalf("runner %s removed=%v credential revoked=%v", id, identity.RemovedAt, credential.RevokedAt)
					}
					if id == "idle" && identity.Revision != 2 {
						t.Fatalf("removed revision=%d", identity.Revision)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.revokeRunnerIdentityCommand(t.Context(), scope, "idle"); err != nil {
				t.Fatalf("replayed removal: %v", err)
			}
		})
	}
}
