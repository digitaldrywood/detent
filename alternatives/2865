package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClaimStateScopesSlotsAndPagesReleasedAttempts(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		seedTenantAccess(t, repository, org)
		seedRunnerIdentity(t, repository, org, "runner", "machine_1", historyAt(), false)
		capacity := int64(1)
		if org == "org_second" {
			capacity = 2
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine_1", Hostname: "example", Capacity: capacity, Version: "example"})
		}); err != nil {
			t.Fatal(err)
		}
		first := claimLease(t, repository, org, leaseClaim(1))
		secondRequest := leaseClaim(2)
		secondRequest.MachineID = "machine_1"
		second := claimLease(t, repository, org, secondRequest)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: first.LeaseID, RunnerID: "runner", AssignedAt: first.AcquiredAt}); err != nil {
				return err
			}
			if _, err := w.RecordAttempt(t.Context(), attemptEvent(first)); err != nil {
				return err
			}
			_, err := w.Release(t.Context(), leaseFence(first), "example")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(runnerstore.ClaimStateReader)
			if !ok {
				t.Fatal("typed runner claim-state reader is unavailable")
			}
			page, err := reader.RunnerClaimState(t.Context(), "runner", "prj_1", "", 1, second.AcquiredAt.Add(time.Microsecond))
			if err != nil {
				return err
			}
			if page.State.HostCapacity != int(capacity) || page.State.RunnerCapacity != 1 || len(page.State.Slots) != 1 || page.State.Slots[0].ID != tracker.LeaseID(second.LeaseID) {
				t.Fatalf("claim capacities/slots = %+v", page.State)
			}
			if len(page.Candidates) != 1 || page.Candidates[0].AttemptID != "attempt_lease_1" || page.Candidates[0].WorkItemID != "wi_1" || page.Next != "attempt_lease_1" {
				t.Fatalf("released page = %+v", page)
			}
			next, err := reader.RunnerClaimState(t.Context(), "runner", "prj_1", page.Next, 1, second.AcquiredAt.Add(time.Microsecond))
			if err != nil {
				return err
			}
			if len(next.Candidates) != 0 || next.Next != "" {
				t.Fatalf("next page = %+v", next)
			}
			expired, err := reader.RunnerClaimState(t.Context(), "runner", "prj_1", "", 1, second.ExpiresAt)
			if err != nil {
				return err
			}
			if len(expired.State.Slots) != 0 {
				t.Fatalf("expired slots = %+v", expired.State.Slots)
			}
			other, err := reader.RunnerClaimState(t.Context(), "runner", "prj_2", "", 1, second.AcquiredAt.Add(time.Microsecond))
			if err == nil && (len(other.Candidates) != 0 || other.Next != "attempt_lease_1") {
				t.Fatalf("other project page = %+v", other)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
