package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestRecordBillingCostsSerializesOppositeOrderBatches(t *testing.T) {
	for _, check := range []struct {
		name      string
		install   func(*testing.T, *sql.DB, int64)
		gate      int64
		batchA    func(*testing.T, tracker.OrganizationID, time.Time) []billingstore.CostObservation
		batchB    func(*testing.T, tracker.OrganizationID, time.Time) []billingstore.CostObservation
		assertion func(*testing.T, *sql.DB, tracker.OrganizationID, time.Time)
	}{
		{
			name: "shared resource source locks",
			gate: 706_001,
			install: func(t *testing.T, admin *sql.DB, gate int64) {
				t.Helper()
				_, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION billing_cost_source_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$; CREATE TRIGGER billing_cost_source_gate BEFORE INSERT ON cost_sources FOR EACH ROW EXECUTE FUNCTION billing_cost_source_gate()`, gate))
				if err != nil {
					t.Fatal(err)
				}
			},
			batchA: func(t *testing.T, org tracker.OrganizationID, base time.Time) []billingstore.CostObservation {
				return []billingstore.CostObservation{
					billingCostObservation(t, org, "project_example", "source_first", "resource_shared", "runner_api", base, base.Add(time.Hour), 11),
					billingCostObservation(t, org, "project_example", "source_second", "resource_shared", "runner_api", base.Add(time.Hour), base.Add(2*time.Hour), 13),
				}
			},
			batchB: func(t *testing.T, org tracker.OrganizationID, base time.Time) []billingstore.CostObservation {
				return []billingstore.CostObservation{
					billingCostObservation(t, org, "project_example", "source_second", "resource_shared", "runner_api", base.Add(time.Hour), base.Add(2*time.Hour), 13),
					billingCostObservation(t, org, "project_example", "source_first", "resource_shared", "runner_api", base, base.Add(time.Hour), 11),
				}
			},
			assertion: func(t *testing.T, admin *sql.DB, org tracker.OrganizationID, month time.Time) {
				var sources, amount int64
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*),coalesce(sum(amount_micros),0) FROM cost_sources WHERE organization_id=$1`, org).Scan(&sources, &amount); err != nil {
					t.Fatal(err)
				}
				if sources != 2 || amount != 24 {
					t.Fatalf("sources=%d amount=%d, want 2 sources and 24 micros", sources, amount)
				}
				assertCostBucketTotal(t, admin, org, "project_example", month, "runner_api", 24)
			},
		},
		{
			name: "monthly buckets in opposite order",
			gate: 706_002,
			install: func(t *testing.T, admin *sql.DB, gate int64) {
				t.Helper()
				_, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION billing_cost_total_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$; CREATE TRIGGER billing_cost_total_gate BEFORE UPDATE ON cost_monthly_totals FOR EACH ROW EXECUTE FUNCTION billing_cost_total_gate()`, gate))
				if err != nil {
					t.Fatal(err)
				}
			},
			batchA: func(t *testing.T, org tracker.OrganizationID, base time.Time) []billingstore.CostObservation {
				return []billingstore.CostObservation{
					billingCostObservation(t, org, "project_example", "source_runner_first", "resource_runner_first", "runner_api", base, base.Add(time.Hour), 17),
					billingCostObservation(t, org, "project_example", "source_luna_second", "resource_luna_second", "luna_api", base, base.Add(time.Hour), 19),
				}
			},
			batchB: func(t *testing.T, org tracker.OrganizationID, base time.Time) []billingstore.CostObservation {
				return []billingstore.CostObservation{
					billingCostObservation(t, org, "project_example", "source_luna_first", "resource_luna_first", "luna_api", base.Add(time.Hour), base.Add(2*time.Hour), 23),
					billingCostObservation(t, org, "project_example", "source_runner_second", "resource_runner_second", "runner_api", base.Add(time.Hour), base.Add(2*time.Hour), 29),
				}
			},
			assertion: func(t *testing.T, admin *sql.DB, org tracker.OrganizationID, month time.Time) {
				assertCostBucketTotal(t, admin, org, "project_example", month, "runner_api", 46)
				assertCostBucketTotal(t, admin, org, "project_example", month, "luna_api", 42)
				assertCostBucketTotal(t, admin, org, "", month, "runner_api", 46)
				assertCostBucketTotal(t, admin, org, "", month, "luna_api", 42)
			},
		},
	} {
		t.Run(check.name, func(t *testing.T) {
			admin := template.Database(t)
			poolConfig := postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1}
			repository, pool := repositoryFor(t, admin, poolConfig)
			admin.SetMaxOpenConns(4)
			var login string
			if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
				t.Fatal(err)
			}
			const organization tracker.OrganizationID = "org_cost_batch"
			seedTenantAccess(t, repository, organization)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "project_example", Name: "Example", Profile: "native"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			base := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
			if check.name == "monthly buckets in opposite order" {
				for _, bucket := range []string{"luna_api", "runner_api"} {
					for _, project := range []string{"", "project_example"} {
						if _, err := admin.ExecContext(t.Context(), `INSERT INTO cost_monthly_totals (organization_id,project_id,month,bucket,currency,amount_micros) VALUES ($1,$2,$3,$4,'USD',0)`, organization, project, base, bucket); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			check.install(t, admin, check.gate)
			release := lockBillingTestGate(t, admin, check.gate)
			recordsA := check.batchA(t, organization, base)
			recordsB := check.batchB(t, organization, base)
			batchA := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.RecordBillingCosts(ctx, recordsA)
					return err
				})
			})
			batchB := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.RecordBillingCosts(ctx, recordsB)
					return err
				})
			})
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(release) }
			t.Cleanup(releaseGate)
			waitCtx, cancelWait := context.WithCancel(t.Context())
			defer cancelWait()
			waitForBillingLockWaiters(t, waitCtx, admin, login, 2)
			releaseGate()
			if err := batchA.wait(t); err != nil {
				t.Fatalf("first billing batch: %v", err)
			}
			if err := batchB.wait(t); err != nil {
				t.Fatalf("second billing batch: %v", err)
			}
			check.assertion(t, admin, organization, base)
		})
	}
}

func billingCostObservation(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, source, resource, bucket string, from, to time.Time, amount int64) billingstore.CostObservation {
	t.Helper()
	observation := usagecost.Observation{Provider: "example", ProviderAccount: "example-account", SourceID: source, ResourceID: resource, Bucket: bucket, Metric: "tokens", Currency: "USD", Unit: "tokens", Revision: 1, From: from, To: to, AmountMicros: &amount}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	archive, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return billingstore.CostObservation{OrganizationID: organization, ProjectID: project, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(archive), ReceivedAt: to.Add(time.Hour)}
}

func lockBillingTestGate(t *testing.T, admin *sql.DB, key int64) func() {
	t.Helper()
	connection, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := connection.BeginTx(t.Context(), nil)
	if err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if _, err := transaction.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		_ = transaction.Rollback()
		_ = connection.Close()
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := transaction.Commit(); err != nil {
				t.Errorf("release PostgreSQL test gate: %v", err)
			}
			if err := connection.Close(); err != nil {
				t.Errorf("close PostgreSQL test gate connection: %v", err)
			}
		})
	}
}

func waitForBillingLockWaiters(t *testing.T, ctx context.Context, admin *sql.DB, login string, minimum int) {
	t.Helper()
	testenv.WaitUntil(t, ctx, func() error {
		var waiting int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='active' AND wait_event_type='Lock'`, login).Scan(&waiting); err != nil {
			return err
		}
		if waiting < minimum {
			return fmt.Errorf("observed %d billing lock waiters, want at least %d", waiting, minimum)
		}
		return nil
	})
}

func assertCostBucketTotal(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, project string, month time.Time, bucket string, want int64) {
	t.Helper()
	var got int64
	if err := admin.QueryRowContext(t.Context(), `SELECT amount_micros FROM cost_monthly_totals WHERE organization_id=$1 AND project_id=$2 AND month=$3 AND bucket=$4 AND currency='USD'`, organization, project, month, bucket).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("monthly total org=%s project=%q bucket=%s = %d, want %d", organization, project, bucket, got, want)
	}
}
