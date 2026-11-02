package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestMetricsRowSecurity(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, name := range []string{"first", "second"} {
			m := metricSample(now, "GET /"+name, "caller_"+name)
			if err := r.MergeRequestMetrics(t.Context(), org, now, []metricsstore.Metric{m}); err != nil {
				t.Fatal(err)
			}
			seedMetric(t, pool, org, metricsstore.Day, m)
		}
	}
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct {
		table, columns string
		own            int
	}{
		{"request_metric_windows", "minute,caller_count", 1},
		{"request_metric_callers", "minute,caller_kind,caller_id", 2},
		{"request_minutes", "minute,route,caller_kind,caller_id,status_class,count,sum_ms,max_ms,histogram", 2},
		{"request_hours", "hour,route,status_class,count,sum_ms,max_ms,histogram", 2},
		{"request_days", "day,route,caller_kind,status_class,count,sum_ms,max_ms", 2},
	} {
		t.Run(tc.table, func(t *testing.T) {
			for _, scope := range []struct {
				org  string
				want int
			}{{"", 0}, {"org_first", tc.own}, {"org_second", tc.own}, {"org_unknown", 0}} {
				t.Run("scope_"+scope.org, func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, scope.org)
					var count int
					if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+tc.table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != scope.want {
						t.Fatalf("unfiltered count = %d, want %d", count, scope.want)
					}
					if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+tc.table+" WHERE organization_id='org_foreign'").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatal("foreign predicate escaped RLS")
					}
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				})
			}
			for _, op := range []string{"insert", "update", "delete"} {
				t.Run(op, func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, "org_first")
					switch op {
					case "insert":
						_, err := tx.ExecContext(t.Context(), fmt.Sprintf("INSERT INTO %s(organization_id,%s) SELECT 'org_foreign',%s FROM %s LIMIT 1", tc.table, tc.columns, tc.columns, tc.table))
						requireSQLState(t, err, "42501")
					case "update":
						_, err := tx.ExecContext(t.Context(), "UPDATE "+tc.table+" SET organization_id='org_foreign'")
						requireSQLState(t, err, "42501")
					case "delete":
						result, err := tx.ExecContext(t.Context(), "DELETE FROM "+tc.table+" WHERE organization_id='org_second'")
						if err != nil {
							t.Fatal(err)
						}
						count, err := result.RowsAffected()
						if err != nil {
							t.Fatal(err)
						}
						if count != 0 {
							t.Errorf("deleted %d foreign rows", count)
						}
					}
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestRequestMetricsTypedConstraints(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), smallPool())
	if err := r.MergeRequestMetrics(t.Context(), "org_first", now, []metricsstore.Metric{metricSample(now, "GET /example", "caller")}); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct{ name, query, state string }{
		{"short histogram", `UPDATE request_minutes SET histogram=ARRAY[1]::bigint[]`, "23514"},
		{"empty histogram", `UPDATE request_minutes SET histogram=ARRAY[]::bigint[]`, "23514"},
		{"negative bucket", `UPDATE request_hours SET histogram[1]=-1`, "23514"},
		{"null bucket", `UPDATE request_hours SET histogram[1]=NULL`, "23514"},
		{"extra bucket", `UPDATE request_minutes SET histogram[65]=0`, "23514"},
		{"zero count", `UPDATE request_minutes SET count=0`, "23514"},
		{"nonfinite sum", `UPDATE request_hours SET sum_ms='NaN'::float8`, "23514"},
		{"nonfinite max", `UPDATE request_hours SET max_ms='Infinity'::float8`, "23514"},
		{"tenant required", `UPDATE request_hours SET organization_id=NULL`, "42501"},
		{"caller limit", `UPDATE request_metric_windows SET caller_count=201`, "23514"},
		{"caller parent tenant", `INSERT INTO request_metric_callers VALUES('org_first',TIMESTAMPTZ '2026-10-30 00:00:00+00','runner','caller')`, "23503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := tenantTx(t, runtime, pool, "org_first")
			_, err := tx.ExecContext(t.Context(), tc.query)
			requireSQLState(t, err, tc.state)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestMetricsMergeRollsBackHourFailure(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), smallPool())
	m := metricSample(now, "GET /overflow", "caller")
	seedMetric(t, pool, "org_first", metricsstore.Hour, m)
	runtime := runtimeFor(t, pool, pool)
	tx := tenantTx(t, runtime, pool, "org_first")
	if _, err := tx.ExecContext(t.Context(), `UPDATE request_hours SET count=9223372036854775807`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	err := r.MergeRequestMetrics(t.Context(), "org_first", now, []metricsstore.Metric{metricSample(now, "GET /first", "first_caller"), m})
	requireSQLState(t, err, "22003")
	rows := readMetrics(t, r, "org_first", metricsstore.Minute, now, now.Add(time.Hour))
	if len(rows) != 0 {
		t.Fatalf("minute writes survived failed hour: %+v", rows)
	}
	for _, table := range []string{"request_metric_windows", "request_metric_callers"} {
		tx := tenantTx(t, runtime, pool, "org_first")
		var count int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s writes survived failure", table)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	hours := readMetrics(t, r, "org_first", metricsstore.Hour, now, now.Add(time.Hour))
	if len(hours) != 1 || hours[0].Count != 9223372036854775807 || hours[0].Sum != 24 {
		t.Fatalf("hour changed on failure: %+v", hours)
	}
}

func TestRequestMetricsRuntimeRejectsUnsafeTables(t *testing.T) {
	for _, table := range []string{"request_metric_windows", "request_metric_callers", "request_minutes", "request_hours", "request_days", "request_metric_imports"} {
		t.Run(table, func(t *testing.T) {
			admin := template.Database(t)
			pool := rolePool(t, admin, "detent_runtime_v2", "")
			if _, err := admin.ExecContext(t.Context(), "ALTER TABLE "+table+" NO FORCE ROW LEVEL SECURITY"); err != nil {
				t.Fatal(err)
			}
			_, err := postgres.NewRuntime(t.Context(), pool, pool)
			if err != postgres.ErrRuntimeRole {
				t.Errorf("unsafe metrics table accepted: %v", err)
			}
		})
	}
}
