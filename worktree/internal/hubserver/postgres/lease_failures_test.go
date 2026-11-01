package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func TestLeaseTransactionFailureReturnsToRetryOwner(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		for _, stage := range []string{"statement", "commit"} {
			t.Run(code+"/"+stage, func(t *testing.T) {
				admin := template.Database(t)
				r, _ := repositoryFor(t, admin, smallPool())
				seedLeaseFamily(t, r, "org_first")
				if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_attempt_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure' USING ERRCODE='%s'; END $$`, code)); err != nil {
					t.Fatal(err)
				}
				trigger := `CREATE TRIGGER reject_receipt BEFORE INSERT ON native_attempt_events FOR EACH ROW EXECUTE FUNCTION reject_attempt_receipt()`
				if stage == "commit" {
					trigger = `CREATE CONSTRAINT TRIGGER reject_receipt AFTER INSERT ON native_attempt_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_attempt_receipt()`
				}
				if _, err := admin.ExecContext(t.Context(), trigger); err != nil {
					t.Fatal(err)
				}
				calls := 0
				write := func(w leasestore.Writer) error {
					calls++
					lease, err := w.Claim(t.Context(), leaseClaim(1))
					if err != nil {
						return err
					}
					_, err = w.RecordAttempt(t.Context(), attemptEvent(lease))
					return err
				}
				err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, write)
				if !errors.Is(err, corestore.ErrSerializationFailure) || !errors.Is(err, mutation.ErrConflict) || calls != 1 {
					t.Fatalf("failure=%v callback calls=%d", err, calls)
				}
				requireSQLState(t, err, code)
				for _, table := range []string{"leases", "native_attempt_runs", "native_attempts", "native_attempt_events"} {
					var n int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
						t.Fatalf("partial %s rows=%d error=%v", table, n, err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), `DROP TRIGGER reject_receipt ON native_attempt_events`); err != nil {
					t.Fatal(err)
				}
				if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, write); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatalf("whole callback calls=%d, want 2", calls)
				}
				lease := claimLease(t, r, "org_first", leaseClaim(1))
				recordLeaseAttempt(t, r, "org_first", attemptEvent(lease))
				requireLeaseUsage(t, r, "org_first", "machine_1", 1)
			})
		}
	}
}

func TestLeaseCancellationRollsBackWholeTransaction(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := r.WriteLeases(ctx, "org_first", corestore.Serving, func(w leasestore.Writer) error {
		lease, err := w.Claim(ctx, leaseClaim(1))
		if err != nil {
			return err
		}
		if _, err := w.RecordAttempt(ctx, attemptEvent(lease)); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	requireLeaseUsage(t, r, "org_first", "machine_1", 0)
	lease := claimLease(t, r, "org_first", leaseClaim(1))
	recordLeaseAttempt(t, r, "org_first", attemptEvent(lease))
}

func TestLeaseReleaseFailurePreservesCapacityAndAttempt(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, smallPool())
			seedLeaseFamily(t, r, "org_first")
			lease := claimLease(t, r, "org_first", leaseClaim(1))
			event := attemptEvent(lease)
			recordLeaseAttempt(t, r, "org_first", event)
			if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_lease_heartbeat() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure' USING ERRCODE='%s'; END $$`, code)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `CREATE TRIGGER reject_heartbeat BEFORE UPDATE ON machines FOR EACH ROW EXECUTE FUNCTION reject_lease_heartbeat()`); err != nil {
				t.Fatal(err)
			}
			release := func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "cancelled")
				return err
			}
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, release)
			if !errors.Is(err, corestore.ErrSerializationFailure) {
				t.Fatalf("release error = %v", err)
			}
			requireSQLState(t, err, code)
			if err := r.ReadLeases(t.Context(), "org_first", corestore.Serving, func(reader leasestore.Reader) error {
				got, err := reader.Lease(t.Context(), lease.LeaseID)
				if err != nil {
					return err
				}
				if got.ReleasedAt != nil {
					t.Error("failed release lost capacity")
				}
				attempt, err := reader.Attempt(t.Context(), event.ID)
				if err == nil && (attempt.Sequence != 1 || attempt.Status != "running") {
					t.Errorf("failed release changed attempt: %+v", attempt)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireLeaseUsage(t, r, "org_first", "machine_1", 1)
			if _, err := admin.ExecContext(t.Context(), `DROP TRIGGER reject_heartbeat ON machines`); err != nil {
				t.Fatal(err)
			}
			if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, release); err != nil {
				t.Fatal(err)
			}
			requireLeaseUsage(t, r, "org_first", "machine_1", 0)
		})
	}
}
