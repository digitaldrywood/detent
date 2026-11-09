package testsupport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const NativeChangeMachineID tracker.MachineID = "machine_11111111111111111111111111111111"
const NativeChangeOtherMachineID tracker.MachineID = "machine_22222222222222222222222222222222"

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
	repository, err := nativeChangeRepository(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(cfg.Hosted.OrganizationID), corestore.Serving, write); err != nil {
		t.Fatal(err)
	}
}

func NativeChangeProject(t *testing.T, cfg hubserver.Config, states []tracker.NativeState, dependencies bool) tracker.NativeProject {
	t.Helper()
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	id := tracker.ProjectID("prj_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	var project projectstore.Record
	NativeChangeWrite(t, cfg, func(w tenantstore.Writer) error {
		var err error
		project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: "Example project " + string(id), Profile: "native", RequireDependencies: dependencies}, States: states})
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
	id, token := "runner_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	credentialID := "key_" + strings.TrimPrefix(id, "runner_")
	now := cfg.Clock().UTC()
	expires := now.Add(24 * time.Hour)
	NativeChangeWrite(t, cfg, func(w tenantstore.Writer) error {
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machine), Hostname: "host", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credentialID, Name: "Example worker", Hash: apikey.HashToken(token), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: project}); err != nil {
			return err
		}
		enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: id, MachineID: machine, TokenHash: apikey.HashToken("enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_admin"}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, id, machine, now); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: machine, TokenID: credentialID, EnrollmentID: enrollment.ID, Scope: "projects", State: "active", Operations: enrollment.Operations, ProjectIDs: enrollment.ProjectIDs, CapacityLimit: 1, ReportedCapacity: 1, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), id, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, ObservedAt: now})
	})
	return token
}

func NativeChangePublisher(t *testing.T, cfg hubserver.Config, project tracker.ProjectID) (string, string) {
	return NativeChangeServiceCredential(t, cfg, project, "worker")
}

func NativeChangeServiceCredential(t *testing.T, cfg hubserver.Config, project tracker.ProjectID, scope string) (string, string) {
	t.Helper()
	id, token := "publisher_"+uuid.NewString(), uuid.NewString()
	now := cfg.Clock().UTC()
	NativeChangeWrite(t, cfg, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(cfg.Hosted.OrganizationID), ID: id, Name: "Example publisher " + id, Hash: apikey.HashToken(token), Scope: scope, NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(cfg.Hosted.OrganizationID), TokenID: id, ProjectID: project})
	})
	return id, token
}

func NativeChangeRefreshBoard(ctx context.Context, service *hubserver.Service, organization tracker.OrganizationID) error {
	for {
		budget := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(ctx, hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		worked, err := service.MaintainWorkItemPage(budget, organization)
		if err != nil {
			return err
		}
		if !worked {
			break
		}
	}
	var after tracker.NativeWorkItemID
	attempted := []tracker.NativeWorkItemID{}
	for {
		budget := (hubmaintenance.Budget{Items: 1, Bytes: hubserver.BoardMaintenanceBytes}).Context(ctx, hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		next, processed, err := service.MaintainBoardRecord(budget, organization, after, attempted...)
		if err != nil || !processed {
			return err
		}
		attempted = append(attempted, next)
		after = next
	}
}

func NativeChangeRefreshReviews(ctx context.Context, cfg hubserver.Config) error {
	owner, err := nativeChangeRepository(cfg)
	if err != nil {
		return err
	}

	for {
		count, err := owner.BackfillCurrentChangeReviews(ctx, tracker.OrganizationID(cfg.Hosted.OrganizationID), eventstore.NewContent(cfg.RunDetails), corestore.MaxPageSize)
		if err != nil || count == 0 {
			return err
		}
	}
}
