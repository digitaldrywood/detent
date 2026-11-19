package executionrecords_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptWorkerInventory(t *testing.T) {
	t.Parallel()
	db := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
	ctx := t.Context()
	const project = tracker.ProjectID("prj_1")

	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner", "machine_1", pgstoretest.HistoryAt(), false)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "peer", "machine_2", pgstoretest.HistoryAt(), false)
		write := func(fn func(tenantstore.Writer) error) error {
			return repository.WriteTenant(ctx, org, corestore.Serving, fn)
		}
		if err := write(func(w tenantstore.Writer) error {
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: org, ID: "machine_1", Hostname: "example", Capacity: 3, Version: "example"}); err != nil {
				return err
			}
			itemIDs := []int64{3, 4, 5, 6}
			if i == 0 {
				for peer := range runnerauth.AttemptWorkerPageSize {
					itemIDs = append(itemIDs, int64(10+peer))
				}
			}
			for _, id := range itemIDs {
				if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: id, Title: "Example"}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		makeAttempt := func(id int64, runner, machine string, typed bool) (leasestore.Lease, leasestore.AttemptEvent) {
			t.Helper()
			claim := pgstoretest.LeaseClaim(id)
			claim.MachineID = machine
			if id >= 10 {
				claim.LeaseID = fmt.Sprintf("lease_0%03d", id)
			}
			lease := pgstoretest.ClaimLease(t, repository, org, claim)
			event := pgstoretest.AttemptEvent(lease)
			event.WorkerCleanupRequired = new(id >= 6)
			if typed {
				pid := int32(100*i) + int32(id)
				event.Worker = &tracker.NativeAttemptWorker{PID: pid, PGID: pid, StartedAt: lease.AcquiredAt.Truncate(time.Microsecond), StartKey: fmt.Sprintf("birth-%d-%d", i, id)}
			}
			if err := write(func(w tenantstore.Writer) error {
				if err := w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: lease.AcquiredAt}); err != nil {
					return err
				}
				_, err := w.RecordAttempt(ctx, event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return lease, event
		}

		lease, event := makeAttempt(1, "runner", "machine_1", true)
		liveLease, live := makeAttempt(3, "runner", "machine_1", true)
		_, peer := makeAttempt(4, "peer", "machine_2", true)
		legacyLease, legacy := makeAttempt(5, "runner", "machine_1", false)
		var legacyNull bool
		if err := db.QueryRowContext(ctx, `SELECT num_nonnulls(worker_pid,worker_pgid,worker_started_at,worker_start_key)=0 FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, legacy.ID).Scan(&legacyNull); err != nil || !legacyNull {
			t.Fatalf("legacy tuple = NULL: %t, %v", legacyNull, err)
		}

		record := func(e leasestore.AttemptEvent, duplicate bool) error {
			return write(func(w tenantstore.Writer) error {
				_, got, err := w.RecordAttemptEvent(ctx, e)
				if err == nil && got != duplicate {
					t.Fatalf("duplicate at sequence %d = %t, want %t", e.Sequence, got, duplicate)
				}
				return err
			})
		}
		if err := record(event, true); err != nil {
			t.Fatal(err)
		}
		other := *event.Worker
		other.StartKey += "-different"
		conflict := event
		conflict.Worker = &other
		if err := record(conflict, false); !errors.Is(err, leasestore.ErrAttemptConflict) {
			t.Fatalf("conflicting replay = %v", err)
		}
		reordered := event
		reordered.Sequence = 3
		if err := record(reordered, false); !errors.Is(err, leasestore.ErrAttemptConflict) {
			t.Fatalf("out-of-order write = %v", err)
		}
		heartbeat := event
		heartbeat.Sequence, heartbeat.Worker = 2, nil
		stale := heartbeat
		stale.Fence.FencingToken++
		if err := record(stale, false); !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("stale fence write = %v", err)
		}
		if err := record(heartbeat, false); err != nil {
			t.Fatal(err)
		}
		if err := record(event, true); err != nil {
			t.Fatalf("reordered identical replay = %v", err)
		}
		conflict.Sequence = 3
		if err := record(conflict, false); !errors.Is(err, leasestore.ErrAttemptConflict) {
			t.Fatalf("unretired replacement = %v", err)
		}
		finished := heartbeat
		finished.Sequence, finished.Status = 3, "succeeded"
		if err := record(finished, false); err != nil {
			t.Fatal(err)
		}

		readLimit := func(runner string, machine tracker.MachineID, after string, limit int, now time.Time) runnerauth.AttemptWorkerPage {
			t.Helper()
			var page runnerauth.AttemptWorkerPage
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(runnerstore.AttemptWorkerReader).RunnerAttemptWorkers(ctx, runner, machine, project, after, limit, now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return page
		}
		read := func(runner, after string, now time.Time) runnerauth.AttemptWorkerPage {
			t.Helper()
			machine := tracker.MachineID("machine_1")
			if runner == "peer" {
				machine = "machine_2"
			}
			return readLimit(runner, machine, after, 1, now)
		}
		expect := func(page runnerauth.AttemptWorkerPage, event leasestore.AttemptEvent, state runnerauth.AttemptLeaseState) {
			t.Helper()
			workers := page.Workers
			if page.ProjectID != project || len(workers) != 1 || workers[0].AttemptID != event.ID || workers[0].LeaseState != state ||
				workers[0].FencingToken != tracker.FencingToken(event.Fence.FencingToken) ||
				workers[0].LeaseID != tracker.LeaseID(event.Fence.LeaseID) || workers[0].Worker.PID != event.Worker.PID || workers[0].Worker.PGID != event.Worker.PGID ||
				workers[0].Worker.StartKey != event.Worker.StartKey || !workers[0].Worker.StartedAt.Equal(event.Worker.StartedAt) {
				t.Fatalf("worker page = %+v, want %s %s %+v", page, event.ID, state, event.Worker)
			}
		}
		now := liveLease.AcquiredAt
		expect(read("runner", "", now), event, runnerauth.AttemptLeaseLive)
		if err := write(func(w tenantstore.Writer) error {
			_, err := w.Release(ctx, pgstoretest.LeaseFence(lease), "finished")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		first := read("runner", "", now)
		expect(first, event, runnerauth.AttemptLeaseFinished)
		if first.Next != event.ID {
			t.Fatalf("incomplete page cursor = %q, want %q", first.Next, event.ID)
		}
		second := read("runner", first.Next, now)
		expect(second, live, runnerauth.AttemptLeaseLive)
		if second.Next != "" {
			t.Fatalf("complete page advertised a peer: %+v", second)
		}
		if exhausted := read("runner", live.ID, now); exhausted.Next != "" || len(exhausted.Workers) != 0 {
			t.Fatalf("legacy owner entered inventory: %+v", exhausted)
		}
		expect(read("peer", live.ID, now), peer, runnerauth.AttemptLeaseLive)
		expect(read("runner", event.ID, liveLease.ExpiresAt), live, runnerauth.AttemptLeaseExpired)

		replacement := pgstoretest.LeaseClaim(1)
		replacement.LeaseID, replacement.SessionID = "lease_replacement", "session_replacement"
		newLease := pgstoretest.ClaimLease(t, repository, org, replacement)
		expect(read("runner", "", newLease.AcquiredAt), event, runnerauth.AttemptLeaseReplaced)
		if err := write(func(w tenantstore.Writer) error {
			if _, err := w.Release(ctx, pgstoretest.LeaseFence(newLease), "finished"); err != nil {
				return err
			}
			return w.DeleteWorkItem(ctx, project, "wi_1")
		}); err != nil {
			t.Fatal(err)
		}
		expect(read("runner", "", newLease.AcquiredAt), event, runnerauth.AttemptLeaseReplaced)
		legacy.Sequence = 2
		legacy.Worker = &tracker.NativeAttemptWorker{PID: 505, PGID: 505, StartedAt: legacyLease.AcquiredAt.Truncate(time.Microsecond), StartKey: "late-launch"}
		if err := record(legacy, false); err != nil {
			t.Fatalf("late launch on legacy attempt = %v", err)
		}
		expect(read("runner", peer.ID, newLease.AcquiredAt), legacy, runnerauth.AttemptLeaseLive)

		if i == 0 {
			for peerIndex := range runnerauth.AttemptWorkerPageSize {
				makeAttempt(int64(10+peerIndex), "peer", "machine_2", true)
			}
			legacyRelease, legacyOwner := makeAttempt(6, "runner", "machine_1", false)
			if err := write(func(w tenantstore.Writer) error {
				_, err := w.Release(ctx, pgstoretest.LeaseFence(legacyRelease), "released")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.(runnerstore.ClaimStateReader).RunnerClaimState(ctx, "runner", project, "", runnerauth.AttemptWorkerPageSize, newLease.AcquiredAt)
				if err == nil && (len(page.Candidates) != 1 || page.Candidates[0].AttemptID != legacyOwner.ID || page.Next != "") {
					t.Fatalf("typed peers hid released legacy owner: %+v", page)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			page := readLimit("runner", "machine_1", "", runnerauth.AttemptWorkerPageSize, newLease.AcquiredAt)
			if len(page.Workers) != 3 || page.Workers[0].AttemptID != event.ID || page.Workers[1].AttemptID != live.ID || page.Workers[2].AttemptID != legacy.ID || page.Next != "" {
				t.Fatalf("full peer page hid runner's own inventory: %+v", page)
			}
		}

		if err := write(func(w tenantstore.Writer) error {
			cleanup := leasestore.AttemptWorkerCleanup{ProjectID: project, WorkItemID: "wi_1", AttemptID: event.ID, RunnerID: "runner", MachineID: "machine_1", Fence: event.Fence, Worker: *event.Worker}
			if err := w.(leasestore.AttemptWorkerCleanupWriter).AcknowledgeAttemptWorker(ctx, cleanup); err != nil {
				return err
			}
			attempt, err := w.Attempt(ctx, event.ID)
			if err == nil && (attempt.CleanupAcknowledgedAt != nil || attempt.Status != "succeeded" || attempt.Sequence != finished.Sequence) {
				t.Fatalf("retirement changed the terminal attempt: %+v", attempt)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		expect(read("runner", "", newLease.AcquiredAt), live, runnerauth.AttemptLeaseLive)

		if err := write(func(w tenantstore.Writer) error {
			return w.(leasestore.AttemptWorkerCleanupWriter).AcknowledgeAttemptWorker(ctx, leasestore.AttemptWorkerCleanup{
				ProjectID: project, WorkItemID: "wi_3", AttemptID: live.ID, RunnerID: "runner", MachineID: "machine_1", Fence: live.Fence, Worker: *live.Worker,
			})
		}); err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{liveLease.AcquiredAt, liveLease.ExpiresAt} {
			page := read("runner", "", at)
			if len(page.Workers) != 0 || len(page.PendingTerminations) != 0 || page.Next != live.ID {
				t.Fatalf("unreleased retired worker page = %+v", page)
			}
		}
		if err := write(func(w tenantstore.Writer) error {
			_, err := w.Release(ctx, live.Fence, "released")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		page := read("runner", "", now)
		want := runnerauth.AttemptTermination{WorkItemID: "wi_3", AttemptID: live.ID, LeaseID: tracker.LeaseID(live.Fence.LeaseID), FencingToken: tracker.FencingToken(live.Fence.FencingToken)}
		if len(page.Workers) != 0 || len(page.PendingTerminations) != 1 || page.PendingTerminations[0] != want || page.Next != live.ID {
			t.Fatalf("released termination page = %+v", page)
		}
		expect(read("runner", page.Next, now), legacy, runnerauth.AttemptLeaseLive)
	}
}
