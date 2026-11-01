package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
