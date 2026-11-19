package executionrecords_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityFactsClipLeaseWindowsAndExcludeOldHistory(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	cfg := pool.Config()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	pool, err = pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = postgres.NewRepository(t.Context(), pool, pgstoretest.SmallPool())
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	from := pgstoretest.HistoryAt().UTC().Truncate(time.Hour)
	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		for index := int64(1); index <= 2; index++ {
			runner := fmt.Sprintf("runner_%d", index)
			machine := fmt.Sprintf("machine_%d", index)
			pgstoretest.SeedRunnerIdentity(t, repository, org, runner, machine, from, false)
			receipt := func(operation, key string) eventstore.Receipt {
				t.Helper()
				value, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "production-import", Operation: operation, CommandKey: key, RequestHash: artifact.Digest([]byte(operation + key)), CreatedAt: from}, []byte(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			add := func(id int, start, expires time.Time, released *time.Time) {
				t.Helper()
				key := fmt.Sprintf("%s_lease_%d", runner, id)
				lease := leasestore.Lease{OrganizationID: org, LeaseID: key, FencingToken: index*1000 + int64(id) + 1, IssueID: index, MachineID: machine, SessionID: key, WorkItemRevision: 1, AcquiredAt: start, RenewedAt: start, ExpiresAt: expires, ReleasedAt: released}
				if err := repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: receipt("core-lease", key)}); err != nil {
					t.Fatal(err)
				}
				assignment := runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: key, RunnerID: runner, AssignedAt: start}
				if err := repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: receipt("core-runner-lease-assignment", key)}); err != nil {
					t.Fatal(err)
				}
				data := tracker.NativeRunData{AttemptID: key, RunID: key, LeaseID: tracker.LeaseID(key), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, RunnerID: runner, MachineID: tracker.MachineID(machine), Outcome: "succeeded", ProcessUsage: &procgroup.Usage{PeakMemoryBytes: 4096, WallSeconds: 10, UserCPUSeconds: 8, SystemCPUSeconds: 2}}
				value := tracker.CollaborationEvent{ID: key, OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", index)), AggregateID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index)), Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: expires, Data: tracker.CollaborationData{Run: &data}}
				event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				attempt := leasestore.Attempt{OrganizationID: org, ID: key, IssueID: index, LeaseID: key, FencingToken: lease.FencingToken, RunID: key, Sequence: 1, Status: "succeeded", WorkItemRevision: 1, PayloadObjectKey: event.PayloadObjectKey, StartedAt: start, UpdatedAt: expires}
				if err := repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt("core-attempt", key)}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
					t.Fatal(err)
				}

			}
			add(0, from.Add(-30*time.Minute), from.Add(30*time.Minute), nil)
			late := from.Add(150 * time.Minute)
			add(1, from.Add(15*time.Minute), from.Add(90*time.Minute), &late)
			early := from.Add(5 * time.Hour)
			add(2, from.Add(4*time.Hour), from.Add(6*time.Hour), &early)
			release := from.Add(4 * time.Hour)
			add(3, from.Add(-2*time.Hour), from.Add(-time.Hour), &release)
			add(4, from.Add(-time.Hour), from, &from)
			end := from.Add(25 * time.Hour)
			add(5, from.Add(24*time.Hour), end, &end)
			read := func(limit int) reportstore.RunnerCapacityFacts {
				t.Helper()
				var facts reportstore.RunnerCapacityFacts
				err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					facts, err = r.(reportstore.RunnerCapacityReader).ReadRunnerCapacityFacts(t.Context(), reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: tracker.MachineID(machine), From: from, To: from.Add(24 * time.Hour), Limit: limit})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				return facts
			}
			for _, history := range []int{2} {
				for i := 0; i < history; i++ {
					start := from.Add(-time.Duration(i+10) * 24 * time.Hour)
					end := start.Add(time.Hour)
					add(i+10, start, end, &end)
				}
				facts := read(10)
				if facts.OccupancyPartial || facts.RunsPartial || len(facts.Occupancy) != 3 || len(facts.Runs) != 2 || facts.EligibleRuns != 2 || facts.UnknownRuns != 0 {
					t.Fatalf("history=%d runner=%s facts=%+v", history, runner, facts)
				}
				for i, wantEnd := range []time.Time{from.Add(30 * time.Minute), from.Add(90 * time.Minute), from.Add(5 * time.Hour)} {
					wantFrom := []time.Time{from.Add(-30 * time.Minute), from.Add(15 * time.Minute), from.Add(4 * time.Hour)}[i]
					if facts.Occupancy[i].LeaseID != fmt.Sprintf("%s_lease_%d", runner, i) || !facts.Occupancy[i].From.Equal(wantFrom) || !facts.Occupancy[i].To.Equal(wantEnd) {
						t.Fatalf("lease expiry/release/window clipped incorrectly: %+v", facts.Occupancy)
					}
				}
			}
			clipped := read(2)
			if !clipped.OccupancyPartial || len(clipped.Occupancy) != 2 {
				t.Fatalf("bounded leases=%+v", clipped)
			}
			err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				reader := r.(reportstore.RunnerCapacityReader)
				q := reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: "foreign_machine", From: from, To: from.Add(24 * time.Hour), Limit: 10}
				foreign, err := reader.ReadRunnerCapacityFacts(t.Context(), q)
				if err != nil {
					return err
				}
				if len(foreign.Occupancy) != 0 || len(foreign.Runs) != 0 {
					t.Fatalf("foreign machine facts=%+v", foreign)
				}
				q.MachineID = tracker.MachineID(machine)
				q.RunnerID = "foreign_runner"
				foreign, err = reader.ReadRunnerCapacityFacts(t.Context(), q)
				if err != nil {
					return err
				}
				if len(foreign.Occupancy) != 0 || len(foreign.Runs) != 0 {
					t.Fatal("foreign runner read occupied another runner's machine")
				}
				q.Limit = 0
				if _, err := reader.ReadRunnerCapacityFacts(t.Context(), q); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("unbounded capacity read accepted: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
