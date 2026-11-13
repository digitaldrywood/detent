package executionrecords_test

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLostLeaseAttemptReadersFollowTheLease(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
	connection := pool.Config().ConnConfig
	pool.Close()
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var clock atomic.Pointer[time.Time]
	clock.Store(&start)
	repository, err := postgres.Open(t.Context(), postgres.Config{
		Connection: pgstoretest.Template.Connection(t, admin, connection.User, connection.Password), Pool: pgstoretest.SmallPool(),
		LeaseClock: func() time.Time { return *clock.Load() },
	}, os.DirFS("../migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for index, tc := range []struct {
		name               string
		checkpoint         string
		release            bool
		expire             bool
		retire             bool
		laterPeriod        bool
		held, unsettled    bool
		active, unreported int64
	}{
		{name: "live running attempt holds work and is active", held: true, unsettled: true, active: 1, unreported: 1},
		{name: "expired lease on a current runner waits for audited retirement but ends activity", expire: true, held: true, unreported: 1},
		{name: "expired lease after audited retirement releases the sprite", expire: true, retire: true, unreported: 1},
		{name: "raw release releases the sprite and ends activity", release: true, unreported: 1},
		{name: "lost attempt leaves later periods", expire: true, retire: true, laterPeriod: true},
		{name: "interrupted checkpoint awaiting cleanup still holds the sprite", checkpoint: "interrupted", expire: true, retire: true, held: true, unsettled: true, unreported: 1},
		{name: "lost running attempt with a checkpoint awaiting cleanup still holds the sprite", checkpoint: "running", expire: true, retire: true, held: true, unsettled: true, unreported: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.Store(&start)
			org := tracker.OrganizationID(fmt.Sprintf("org_lost_lease_%d", index))
			pgstoretest.SeedLeaseFamily(t, repository, org)
			pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_1", "machine_1", start, false)
			claim := pgstoretest.LeaseClaim(1)
			claim.RunnerID, claim.TTL = "runner_1", 90*time.Second
			lease := pgstoretest.ClaimLease(t, repository, org, claim)
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, repository, org, event)
			if tc.checkpoint != "" {
				event.Sequence, event.Status, event.CheckpointObjectKey, event.CheckpointPreservesSource = 2, tc.checkpoint, new("example/checkpoint"), new(true)
				event.PayloadObjectKey = "example/checkpoint-event"
				if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, _, err := w.RecordAttemptEvent(t.Context(), event)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.release {
				if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "released")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.retire {
				if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET removed_at=$3 WHERE organization_id=$1 AND runner_id=$2`, org, "runner_1", lease.ExpiresAt); err != nil {
					t.Fatal(err)
				}
			}
			now := start.Add(time.Second)
			if tc.expire {
				now = lease.ExpiresAt
			}
			clock.Store(&now)
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.Attempt(t.Context(), event.ID)
				if err != nil {
					return err
				}
				if stored.Status != event.Status {
					t.Errorf("reader fixture rewrote the attempt: status=%s want %s", stored.Status, event.Status)
				}
				held, err := r.(postgres.SpriteReader).SpriteMemberHoldsWork(t.Context(), "enrollment_runner_1")
				if err != nil {
					return err
				}
				if held != tc.held {
					t.Errorf("sprite member holds work=%t want %t", held, tc.held)
				}
				unsettled, err := r.(postgres.ConfigurationReader).ConfigurationUnsettled(t.Context(), "runner_1", "prj_1", now)
				if err != nil {
					return err
				}
				if unsettled != tc.unsettled {
					t.Errorf("runner project configuration unsettled=%t want %t", unsettled, tc.unsettled)
				}
				window := billingstore.MonthlyCostQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)}
				if tc.laterPeriod {
					window = billingstore.MonthlyCostQuery{From: lease.ExpiresAt.Add(time.Hour), To: lease.ExpiresAt.Add(2 * time.Hour)}
				}
				costs, err := r.(billingstore.MonthlyCostReader).ReadMonthlyCosts(t.Context(), window)
				if err != nil {
					return err
				}
				if costs.ActiveAttempts != tc.active || costs.UnreportedAttempts != tc.unreported {
					t.Errorf("monthly coverage active=%d unreported=%d want active=%d unreported=%d", costs.ActiveAttempts, costs.UnreportedAttempts, tc.active, tc.unreported)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
