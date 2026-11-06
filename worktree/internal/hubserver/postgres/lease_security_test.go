package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLeaseFamilyRowSecurity(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			lease := claimLease(t, r, org, leaseClaim(id))
			recordLeaseAttempt(t, r, org, attemptEvent(lease))
		}
	}
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct{ table, columns string }{
		{"machines", "id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at"},
		{"leases", "lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at"},
		{"native_attempt_runs", "run_id,issue_id"},
		{"native_attempts", "id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at"},
		{"native_attempt_events", "attempt_id,sequence,request_hash"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			for _, org := range []string{"", "org_unknown", "org_first", "org_second"} {
				t.Run("scope_"+org, func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, org)
					defer tx.Rollback()
					var count int
					if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+tc.table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					want := 0
					if org == "org_first" || org == "org_second" {
						want = 2
					}
					if count != want {
						t.Fatalf("unfiltered rows=%d, want %d", count, want)
					}
					result, err := tx.ExecContext(t.Context(), "DELETE FROM "+tc.table+" WHERE organization_id='org_foreign'")
					if err != nil {
						t.Fatal(err)
					}
					if n, err := result.RowsAffected(); err != nil || n != 0 {
						t.Fatalf("foreign delete=%d error=%v", n, err)
					}
				})
			}
			for _, operation := range []string{"insert", "move"} {
				t.Run(operation, func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, "org_first")
					defer tx.Rollback()
					query := "UPDATE " + tc.table + " SET organization_id='org_foreign'"
					if operation == "insert" {
						query = fmt.Sprintf("INSERT INTO %s(organization_id,%s) SELECT 'org_foreign',%s FROM %s LIMIT 1", tc.table, tc.columns, tc.columns, tc.table)
					}
					_, err := tx.ExecContext(t.Context(), query)
					requireSQLState(t, err, "42501")
				})
			}
			var owner string
			if err := admin.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=$1::regclass`, tc.table).Scan(&owner); err != nil || owner != "detent_migration_v2" {
				t.Fatalf("owner=%s error=%v", owner, err)
			}
			if _, err := admin.ExecContext(t.Context(), "ALTER TABLE "+tc.table+" NO FORCE ROW LEVEL SECURITY"); err != nil {
				t.Fatal(err)
			}
			_, err := postgres.NewRuntime(t.Context(), pool, pool)
			if !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Errorf("unforced RLS accepted: %v", err)
			}
			if _, err := admin.ExecContext(t.Context(), "ALTER TABLE "+tc.table+" FORCE ROW LEVEL SECURITY"); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			for _, id := range []string{"lease_1", "lease_2"} {
				lease, err := reader.Lease(t.Context(), id)
				if err != nil {
					return err
				}
				if lease.OrganizationID != org {
					t.Error("lease escaped tenant scope")
				}
				attempt, err := reader.Attempt(t.Context(), "attempt_"+id)
				if err != nil {
					return err
				}
				if attempt.OrganizationID != org || attempt.FencingToken != lease.FencingToken {
					t.Error("attempt escaped tenant or lease scope")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeaseFamilyConstraints(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			lease := claimLease(t, r, org, leaseClaim(id))
			recordLeaseAttempt(t, r, org, attemptEvent(lease))
		}
	}
	if err := r.WriteLeases(t.Context(), "org_second", corestore.Serving, func(w leasestore.Writer) error {
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_second", ID: "foreign_only", Hostname: "example", Version: "example"})
	}); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct{ name, query, state string }{
		{"one active owner", `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT organization_id,'duplicate',issue_id,machine_id,'duplicate',work_item_revision,acquired_at,renewed_at,expires_at FROM leases WHERE lease_id='lease_1'`, "23505"},
		{"tenant required", `UPDATE machines SET organization_id=NULL`, "42501"},
		{"negative capacity", `UPDATE machines SET capacity=-1`, "23514"},
		{"foreign machine", `UPDATE leases SET machine_id='foreign_only'`, "23503"},
		{"missing item", `UPDATE leases SET issue_id=99 WHERE lease_id='lease_1'`, "23503"},
		{"wrong lease fence", `UPDATE native_attempts SET fencing_token=fencing_token+100`, "23503"},
		{"wrong lease item", `UPDATE native_attempts SET issue_id=3-issue_id`, "23503"},
		{"wrong lease revision", `UPDATE native_attempts SET work_item_revision=work_item_revision+1`, "23503"},
		{"wrong run item", `UPDATE native_attempts SET run_id='run_lease_2' WHERE id='attempt_lease_1'`, "23503"},
		{"missing event parent", `UPDATE native_attempt_events SET attempt_id='missing' WHERE attempt_id='attempt_lease_1'`, "23503"},
		{"zero event sequence", `UPDATE native_attempt_events SET sequence=0`, "23514"},
		{"invalid event hash", `UPDATE native_attempt_events SET request_hash='invalid'`, "23514"},
		{"invalid attempt status", `UPDATE native_attempts SET status='invalid'`, "23514"},
		{"negative dispatch", `UPDATE native_attempts SET dispatch_generation=-1`, "23514"},
		{"blank payload", `UPDATE native_attempts SET payload_object_key=' '`, "23514"},
		{"expiry before renewal", `UPDATE leases SET expires_at=renewed_at`, "23514"},
		{"sequence reset forbidden", `SELECT setval('leases_fencing_token_seq',1)`, "42501"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := tenantTx(t, runtime, pool, "org_first")
			defer tx.Rollback()
			_, err := tx.ExecContext(t.Context(), tc.query)
			requireSQLState(t, err, tc.state)
		})
	}
}
