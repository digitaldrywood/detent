package executionrecords_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptFailureSummaryFollowsAttemptSequence(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	observed := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	failure := leasestore.AttemptFailure{Example: "example failure", Class: "attempt", LandingObservedAt: &observed}
	summary := func(t *testing.T, org tracker.OrganizationID, issue int64) leasestore.AttemptFailureRow {
		t.Helper()
		var rows []leasestore.AttemptFailureRow
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			var err error
			rows, err = reader.(leasestore.AttemptFailureReader).AttemptFailures(t.Context(), issue, 65)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatal("attempt failure page is empty")
		}
		return rows[0]
	}
	set := func(t *testing.T, org tracker.OrganizationID, fact leasestore.AttemptFailureFact) error {
		t.Helper()
		return r.WriteLeases(t.Context(), org, corestore.Background, func(w leasestore.Writer) error {
			return w.(leasestore.AttemptFailureWriter).SetAttemptFailure(t.Context(), fact)
		})
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		for _, issue := range []int64{1, 2} {
			lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(issue))
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, org, event)
			if got := summary(t, org, issue); got.Recorded || got.Failure != (leasestore.AttemptFailure{}) {
				t.Fatalf("unclassified attempt became recorded: %+v", got)
			}
			event.Sequence, event.Status, event.PayloadObjectKey, event.Failure = 2, "failed", "example/failed", &failure
			recorded := pgstoretest.RecordLeaseAttempt(t, r, org, event)
			if got := summary(t, org, issue); !got.Recorded || got.AttemptID != recorded.ID || got.Status != "failed" || got.Failure.Example != failure.Example || got.Failure.Class != failure.Class || got.Failure.LandingObservedAt == nil || !got.Failure.LandingObservedAt.Equal(observed) || !got.UpdatedAt.Equal(recorded.UpdatedAt) {
				t.Fatalf("recorded failure = %+v", got)
			}
			event.Sequence, event.PayloadObjectKey, event.Failure, event.UsageCorrection = 3, "example/corrected", nil, true
			pgstoretest.RecordLeaseAttempt(t, r, org, event)
			if got := summary(t, org, issue); !got.Recorded || got.Failure.Example != failure.Example {
				t.Fatalf("usage correction lost the failure summary: %+v", got)
			}
			if err := set(t, org, leasestore.AttemptFailureFact{AttemptID: event.ID, IssueID: issue, Sequence: 3, Failure: leasestore.AttemptFailure{}}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("recorded summary was replaceable: %v", err)
			}
			_, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET failure_sequence=NULL,failure_example=NULL,failure_class=NULL,failure_landing_observed_at=NULL WHERE organization_id=$1 AND id=$2`, org, event.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := set(t, org, leasestore.AttemptFailureFact{AttemptID: event.ID, IssueID: issue, Sequence: 2, Failure: failure}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale backfill wrote a summary: %v", err)
			}
			if err := set(t, org, leasestore.AttemptFailureFact{AttemptID: event.ID, IssueID: issue, Sequence: 3, Failure: failure}); err != nil {
				t.Fatal(err)
			}
			if got := summary(t, org, issue); !got.Recorded || got.Failure.Example != failure.Example {
				t.Fatalf("backfilled failure = %+v", got)
			}
			if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			request := pgstoretest.LeaseClaim(issue)
			request.LeaseID, request.SessionID = request.LeaseID+"_next", request.SessionID+"_next"
			next := pgstoretest.ClaimLease(t, r, org, request)
			running := pgstoretest.AttemptEvent(next)
			running.Failure = &leasestore.AttemptFailure{}
			pgstoretest.RecordLeaseAttempt(t, r, org, running)
			if got := summary(t, org, issue); !got.Recorded || got.AttemptID != running.ID || got.Failure != (leasestore.AttemptFailure{}) {
				t.Fatalf("running attempt summary = %+v", got)
			}
			if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(next), "cancelled")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := summary(t, org, issue); !got.Recorded || got.AttemptID != running.ID || got.Status != "running" || got.Failure != (leasestore.AttemptFailure{}) {
				t.Fatalf("lease release rewrote the attempt or its summary: %+v", got)
			}
		}
	}
}
