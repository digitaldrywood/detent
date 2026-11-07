package testsupport

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeChangeIdentityProvider struct{ auth.HostedProvider }

func (p nativeChangeIdentityProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	if user != "user_example" || organization != "provider_example" {
		return nil, nil
	}
	member := auth.Membership{ID: "membership_example", UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = "owner"
	return []auth.Membership{member}, nil
}

func NativeChangeWrite(t *testing.T, cfg hubserver.Config, write func(tenantstore.Writer) error) {
	t.Helper()
	migrations, err := fs.Sub(nativeChangeSchema(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, migrations)
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), tracker.OrganizationID(cfg.Hosted.OrganizationID), corestore.Serving, write)
	if err := errors.Join(err, repository.Close()); err != nil {
		t.Fatal(err)
	}
}

func NativeChangeProject(t *testing.T, cfg hubserver.Config, states []tracker.NativeState, dependencies bool) tracker.NativeProject {
	t.Helper()
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	id := tracker.ProjectID("prj_" + uuid.NewString())
	var project projectstore.Record
	NativeChangeWrite(t, cfg, func(w tenantstore.Writer) error {
		var err error
		project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: "Example project", Profile: "native", RequireDependencies: dependencies}, States: states})
		if err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_example", ProjectID: id, CanWrite: true, ManageRunner: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "test_admin", ProjectID: id})
	})
	return tracker.NativeProject{OrganizationID: organization, ID: id, Name: project.Core.Name, Slug: project.Core.Slug, States: states, Profile: "native"}
}

func NativeChangeWorker(t *testing.T, cfg hubserver.Config, project tracker.ProjectID, machine tracker.MachineID) string {
	t.Helper()
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	id, token := "runner_"+uuid.NewString(), uuid.NewString()
	now := cfg.Clock().UTC()
	expires := now.Add(24 * time.Hour)
	NativeChangeWrite(t, cfg, func(w tenantstore.Writer) error {
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machine), Hostname: "host", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: "Example worker", Hash: apikey.HashToken(token), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: project}); err != nil {
			return err
		}
		enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: id, MachineID: machine, TokenHash: apikey.HashToken("enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_admin"}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, id, machine, now); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: machine, TokenID: id, EnrollmentID: enrollment.ID, Scope: "projects", State: "active", Operations: enrollment.Operations, ProjectIDs: enrollment.ProjectIDs, CapacityLimit: 1, ReportedCapacity: 1, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), id, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, ObservedAt: now})
	})
	return token
}
