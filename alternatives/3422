package pgstoretest

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func SeedRunnerIdentity(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID, runnerID, machineID string, now time.Time, registerMachine bool) {
	t.Helper()
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if registerMachine {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: runnerID, Name: runnerID, Hash: apikey.HashToken("credential_" + string(organization) + "_" + runnerID), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: TimePointer(now.Add(time.Hour)), ProjectAccess: "all", Kind: "service"}); err != nil {
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

func TimePointer(value time.Time) *time.Time {
	return &value
}
