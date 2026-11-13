package executionrecords_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClaimStateScopesSlotsAndPagesReleasedAttempts(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner", "machine_1", pgstoretest.HistoryAt(), false)
		capacity := int64(1)
		if org == "org_second" {
			capacity = 2
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine_1", Hostname: "example", Capacity: capacity, Version: "example"})
		}); err != nil {
			t.Fatal(err)
		}
		first := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		secondRequest := pgstoretest.LeaseClaim(2)
		secondRequest.MachineID = "machine_1"
		second := pgstoretest.ClaimLease(t, repository, org, secondRequest)
		objects := eventstore.NewContent(pgstoretest.NewHistoryObjects())
		workerRun := func(lease leasestore.Lease, runner string) tracker.NativeRunData {
			return tracker.NativeRunData{
				AttemptID: "attempt_" + lease.LeaseID, RunID: "run_" + lease.LeaseID,
				LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken),
				MachineID: tracker.MachineID(lease.MachineID), RunnerID: runner, SessionID: lease.SessionID, Sequence: 1,
				Identity: &tracker.NativeExecutionIdentity{Role: "code"},
				Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{
					PID: 100 + int(lease.IssueID), ProcessStart: lease.AcquiredAt,
				}},
			}
		}
		prepareAttempt := func(t *testing.T, lease leasestore.Lease, run tracker.NativeRunData) leasestore.AttemptEvent {
			t.Helper()
			payload, err := objects.PreparePayload(t.Context(), org, pgstoretest.HistoryJSON(t, run))
			if err != nil {
				t.Fatal(err)
			}
			cleanupRequired := run.Identity != nil && run.Identity.Role != "" && run.Runtime != nil && run.Runtime.WorkerLaunch != nil && run.Runtime.WorkerLaunch.PID > 0 && !run.Runtime.WorkerLaunch.ProcessStart.IsZero()
			event := pgstoretest.AttemptEvent(lease)
			event.Sequence = run.Sequence
			if run.Outcome != "" {
				event.Status = run.Outcome
			}
			event.PayloadObjectKey = payload.Key
			event.PayloadReference = payload
			event.WorkerCleanupRequired = &cleanupRequired
			return event
		}
		prepareCancellation := func(t *testing.T, lease leasestore.Lease, run tracker.NativeRunData, failure string) *leasestore.AttemptCancellation {
			t.Helper()
			cancelled := run
			cancelled.Sequence++
			cancelled.Outcome = "cancelled"
			cancelled.TerminalFailure = &tracker.NativeTerminalFailure{ErrorClass: "lease_released", Stage: "lease_release", ObservedAt: lease.AcquiredAt, Error: failure}
			attempt := prepareAttempt(t, lease, cancelled)
			history, err := objects.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{
				ID: "evt_cancel_" + run.AttemptID, OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", lease.IssueID)),
				AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", lease.IssueID)), Type: "run.finished", SchemaVersion: 1,
				RecordedAt: lease.AcquiredAt, Actor: tracker.Actor{Kind: "integration", PrincipalID: "lease_release"}, Data: tracker.CollaborationData{Run: &cancelled},
			}))
			if err != nil {
				t.Fatal(err)
			}
			return &leasestore.AttemptCancellation{Attempt: attempt, History: history}
		}
		firstRun := workerRun(first, "runner")
		firstEvent := prepareAttempt(t, first, firstRun)
		firstCancellation := prepareCancellation(t, first, firstRun, "Example lease released")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: first.LeaseID, RunnerID: "runner", AssignedAt: first.AcquiredAt}); err != nil {
				return err
			}
			if _, err := w.RecordAttempt(t.Context(), firstEvent); err != nil {
				return err
			}
			if _, err := w.RecordAttempt(t.Context(), firstCancellation.Attempt); err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), firstCancellation.History); err != nil {
				return err
			}
			_, err := w.Release(t.Context(), pgstoretest.LeaseFence(first), "example")
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
			if err == nil && (len(other.Candidates) != 0 || len(other.Unclassified) != 0 || other.Next != "") {
				t.Fatalf("other project page = %+v", other)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/peer cleanup authority", func(t *testing.T) {
			const peer = "peer"
			pgstoretest.SeedRunnerIdentity(t, repository, org, peer, second.MachineID, pgstoretest.HistoryAt(), false)
			run := workerRun(second, peer)
			event := prepareAttempt(t, second, run)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: second.LeaseID, RunnerID: peer, AssignedAt: second.AcquiredAt}); err != nil {
					return err
				}
				_, err := w.RecordAttempt(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name, status string
				sequence     int64
				expired      bool
			}{
				{name: "live", status: "running", sequence: 1},
				{name: "expired", status: "cancelled", sequence: 2, expired: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					if test.expired {
						expireLeaseFixture(t, admin, org, second.LeaseID)
						cancellation := prepareCancellation(t, second, run, "Example lease expired")
						if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
							_, err := w.ExpireWithCancellation(t.Context(), pgstoretest.LeaseFence(second), cancellation)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(runnerstore.ClaimStateReader)
						if !ok {
							t.Fatal("typed runner claim-state reader is unavailable")
						}
						for _, runner := range []string{"runner", peer} {
							page, err := reader.RunnerClaimState(t.Context(), runner, "prj_2", "attempt_lease_1", 1, second.AcquiredAt.Add(time.Microsecond))
							if err != nil {
								return err
							}
							if page.Next != event.ID {
								t.Fatalf("runner %s did not examine the peer attempt: %+v", runner, page)
							}
							if test.expired {
								if len(page.State.Slots) != 0 {
									t.Fatalf("runner %s still sees an expired slot: %+v", runner, page.State.Slots)
								}
							} else if len(page.State.Slots) != 1 || page.State.Slots[0].ID != tracker.LeaseID(second.LeaseID) || page.State.Slots[0].RunnerID != peer {
								t.Fatalf("runner %s lost the live peer slot: %+v", runner, page.State.Slots)
							}
							if test.expired && runner == peer {
								if len(page.Candidates) != 1 || page.Candidates[0].AttemptID != event.ID || page.Candidates[0].WorkItemID != "wi_2" || page.Candidates[0].ProjectID != "prj_2" {
									t.Fatalf("owner lost the expired cleanup candidate: %+v", page.Candidates)
								}
							} else if len(page.Candidates) != 0 {
								t.Fatalf("runner %s received an ineligible cleanup candidate: %+v", runner, page.Candidates)
							}
						}
						lease, err := r.Lease(t.Context(), second.LeaseID)
						if err != nil {
							return err
						}
						if lease.FencingToken != second.FencingToken || lease.MachineID != second.MachineID || lease.SessionID != second.SessionID {
							t.Fatalf("peer lease identity changed: %+v", lease)
						}
						if test.expired {
							if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" {
								t.Fatalf("peer lease did not expire: %+v", lease)
							}
						} else if lease.ReleasedAt != nil || !lease.ExpiresAt.Equal(second.ExpiresAt) {
							t.Fatalf("live peer lease changed: %+v", lease)
						}
						attempt, err := r.Attempt(t.Context(), event.ID)
						if err != nil {
							return err
						}
						if attempt.LeaseID != second.LeaseID || attempt.FencingToken != second.FencingToken || attempt.Status != test.status || attempt.Sequence != test.sequence {
							t.Fatalf("peer attempt = %+v, want %s at sequence %d", attempt, test.status, test.sequence)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
