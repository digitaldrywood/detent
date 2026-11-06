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
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresNativeProjectAuthorizationRejectsDeletedProjectsAndRemovedRunners(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	db := &database{postgres: repository, now: func() time.Time { return now }}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			project := tracker.ProjectID("project_" + organization)
			runner := "runner_" + string(organization)
			admin := "admin_" + string(organization)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "example-" + string(organization), CreatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				for _, credential := range []accessstore.Credential{
					{OrganizationID: organization, ID: admin, Name: "Example admin", Hash: apikey.HashToken(admin), Scope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now},
					{OrganizationID: organization, ID: runner, Name: "Example runner", Hash: apikey.HashToken(runner), Scope: "worker", Kind: "service", NativeOnly: true, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now},
				} {
					if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
						return err
					}
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: runner, Hostname: "example-host", Capacity: 1}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: runner, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenHash: apikey.HashToken(runner), Scope: "organization", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: admin}); err != nil {
					return err
				}
				return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenID: runner, EnrollmentID: runner, Scope: "organization", State: "active", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: organization, project: project, credential: apiCredential{ID: runner, Hash: apikey.HashToken(runner), Scope: apiScopeWorker, NativeOnly: true, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runner, MachineID: tracker.MachineID(runner)}, OrganizationID: organization}}}
			if err := db.authorizeNativeProject(t.Context(), scope); err != nil {
				t.Fatalf("organization runner without token grant: %v", err)
			}
			mismatched := scope
			mismatched.credential.Runner.OrganizationID = "org_other"
			if err := db.authorizeNativeProject(t.Context(), mismatched); !isNativeNotFound(err) {
				t.Fatalf("mismatched runner organization: %v", err)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RemoveRunnerIdentity(t.Context(), runner, now)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.authorizeNativeProject(t.Context(), scope); !isNativeNotFound(err) {
				t.Fatalf("removed runner: %v", err)
			}
			scope.credential = apiCredential{ID: admin, Hash: apikey.HashToken(admin), Scope: apiScopeAdmin}
			if err := db.authorizeNativeProject(t.Context(), scope); err != nil {
				t.Fatalf("live project administrator: %v", err)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				return w.RetireProject(t.Context(), project, record.Settings.Revision, now)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.authorizeNativeProject(t.Context(), scope); !isNativeNotFound(err) {
				t.Fatalf("deleted project administrator: %v", err)
			}
		})
	}
}
