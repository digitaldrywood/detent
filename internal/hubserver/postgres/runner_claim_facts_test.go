package postgres_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClaimFactsUpgradeRetainsUnresolvedTenantAttempts(t *testing.T) {
	db, fixture := migrationDatabaseAtVersion(t, 20261109002731)
	seedCore(t, db)
	for _, organization := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'machine_shared','example',1,'example',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) VALUES($1,'lease_shared',41,'machine_shared','session_shared',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 hour')`,
			`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_shared',41)`,
			`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,started_at,updated_at) SELECT organization_id,'attempt_shared',issue_id,lease_id,fencing_token,'run_shared',1,'succeeded',work_item_revision,0,$1||'/retained_payload',$1||'/retained_checkpoint',acquired_at,renewed_at FROM leases WHERE organization_id=$1 AND lease_id='lease_shared'`,
		} {
			if _, err := db.ExecContext(t.Context(), statement, organization); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 20261109092607); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	pool := rolePoolFromTemplate(t, fixture, db, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for index, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			var attempt leasestore.Attempt
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				attempt, err = r.Attempt(t.Context(), "attempt_shared")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if attempt.ClaimDisposition != nil || attempt.Sequence != 1 || attempt.PayloadObjectKey != string(organization)+"/retained_payload" || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != string(organization)+"/retained_checkpoint" {
				t.Fatalf("upgrade lost unresolved source: %+v", attempt)
			}
			disposition := "human_action"
			if index == 1 {
				disposition = "progress"
			}
			facts := leasestore.AttemptClaimFacts{AttemptID: attempt.ID, Sequence: attempt.Sequence, Status: attempt.Status, PayloadObjectKey: attempt.PayloadObjectKey, CheckpointObjectKey: attempt.CheckpointObjectKey, Disposition: disposition, UnresolvedOnly: true}
			for _, test := range []struct {
				name   string
				mutate func(*leasestore.AttemptClaimFacts)
				want   error
			}{
				{name: "sequence changed", mutate: func(f *leasestore.AttemptClaimFacts) { f.Sequence++ }, want: corestore.ErrRevisionConflict},
				{name: "payload changed", mutate: func(f *leasestore.AttemptClaimFacts) { f.PayloadObjectKey += "/new" }, want: corestore.ErrRevisionConflict},
				{name: "checkpoint changed", mutate: func(f *leasestore.AttemptClaimFacts) { f.CheckpointObjectKey = new("different") }, want: corestore.ErrRevisionConflict},
				{name: "status changed", mutate: func(f *leasestore.AttemptClaimFacts) { f.Status = "failed" }, want: corestore.ErrRevisionConflict},
				{name: "unknown classification", mutate: func(f *leasestore.AttemptClaimFacts) { f.Disposition = "unknown" }, want: corestore.ErrInvalidPage},
			} {
				t.Run(test.name, func(t *testing.T) {
					changed := facts
					test.mutate(&changed)
					err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.SetAttemptClaimFacts(t.Context(), changed) })
					if !errors.Is(err, test.want) {
						t.Fatalf("stale recovery error=%v want %v", err, test.want)
					}
				})
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.SetAttemptClaimFacts(t.Context(), facts) }); err != nil {
				t.Fatal(err)
			}
			changed := facts
			changed.Disposition = "permission_wait"
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.SetAttemptClaimFacts(t.Context(), changed) }); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("already recovered facts overwritten: %v", err)
			}
			var stored sql.NullString
			if err := db.QueryRowContext(t.Context(), `SELECT claim_disposition FROM native_attempts WHERE organization_id=$1 AND id='attempt_shared'`, organization).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if !stored.Valid || stored.String != disposition {
				t.Fatalf("tenant recovery=%+v want %q", stored, disposition)
			}
		})
	}
}
