package hubserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestMetricsBackendParity(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, backend := range []DatabaseBackend{DatabaseSQLite, DatabasePostgres} {
		t.Run(string(backend), func(t *testing.T) {
			var store *database
			if backend == DatabasePostgres {
				admin := postgresStartupTemplate.Database(t)
				connection := startupConnection(t, admin, "detent_runtime_v2", "")
				var err error
				store, err = openDatabase(t.Context(), startupConfig(connection))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			for tenant, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(string(org), func(t *testing.T) {
					owner := store
					if owner == nil {
						owner = &database{path: filepath.Join(t.TempDir(), "hub.sqlite")}
					}
					if owner.postgres != nil {
						owner.path = filepath.Join(t.TempDir(), "hub.sqlite")
					}
					m, err := owner.openRequestMetrics(t.Context(), org)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := m.close(); err != nil {
							t.Error(err)
						}
					})
					if backend == DatabasePostgres && (m.db != nil || m.repository == nil) {
						t.Fatal("PostgreSQL collector opened SQLite")
					}
					m.usage = &database{}
					for i := range metricsstore.MergeLimit + 1 {
						for range tenant + 1 {
							m.record(now.Add(-time.Minute), fmt.Sprintf("GET /route/%03d", i), "runner", "caller", 200, time.Millisecond)
						}
					}
					for key, value := range m.pending {
						value.http.requests = 1
						m.pending[key] = value
					}
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					if err := m.flush(ctx, now); !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled flush: %v", err)
					}
					if len(m.pending) != metricsstore.MergeLimit+1 {
						t.Fatalf("failed flush lost pending metrics: %d", len(m.pending))
					}
					if err := m.flush(t.Context(), now); err != nil {
						t.Fatal(err)
					}
					if backend == DatabasePostgres && len(m.pending) != 1 {
						t.Fatalf("flush exceeded batch budget: %d pending", len(m.pending))
					}
					if backend == DatabasePostgres {
						if err := m.flush(ctx, now); !errors.Is(err, context.Canceled) {
							t.Fatalf("failed second batch: %v", err)
						}
						if len(m.pending) != 1 {
							t.Fatalf("retry lost uncommitted latency: %d", len(m.pending))
						}
					}
					if err := m.drain(t.Context(), now); err != nil {
						t.Fatal(err)
					}
					report, err := m.report(t.Context(), now)
					if err != nil {
						t.Fatal(err)
					}
					if len(report.Callers) != 1 || report.Callers[0].Count != int64((metricsstore.MergeLimit+1)*(tenant+1)) || len(report.Routes) != metricsstore.RouteLimit {
						t.Fatalf("report: %+v", report)
					}
					if report.Callers[0].P50 != 1 || report.Callers[0].P95 != 1 {
						t.Fatalf("percentiles: %+v", report.Callers)
					}
					history, err := m.history(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					if len(history.Buckets) != metricsstore.RouteLimit || history.Buckets[0].Count != int64(tenant+1) {
						t.Fatalf("history: %+v", history)
					}
					s := &Service{requestMetrics: m}
					if err := s.expireRequestMetrics(t.Context(), now.Add(15*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					if err := s.expireRequestMetrics(t.Context(), now.Add(15*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					report, err = m.report(t.Context(), now)
					if err != nil || len(report.Callers) != 0 {
						t.Fatalf("retention report: %+v, %v", report, err)
					}
					history, err = m.history(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil || len(history.Buckets) != metricsstore.RouteLimit {
						t.Fatalf("hour retention: %+v, %v", history, err)
					}
					if backend == DatabasePostgres {
						rows, err := m.repository.RequestMetrics(t.Context(), org, metricsstore.Page{Grain: metricsstore.Day, From: now.Add(-24 * time.Hour), To: now, Limit: metricsstore.MergeLimit})
						if err != nil || len(rows) != metricsstore.MergeLimit {
							t.Fatalf("retained days: %d, %v", len(rows), err)
						}
						var total int64
						for _, row := range rows {
							total += row.Count
						}
						after := rows[len(rows)-1]
						rows, err = m.repository.RequestMetrics(t.Context(), org, metricsstore.Page{Grain: metricsstore.Day, From: now.Add(-24 * time.Hour), To: now, After: &after, Limit: metricsstore.MergeLimit})
						if err != nil || len(rows) != 1 {
							t.Fatalf("day cursor: %d, %v", len(rows), err)
						}
						total += rows[0].Count
						if total != int64((metricsstore.MergeLimit+1)*(tenant+1)) {
							t.Fatalf("day total: %d", total)
						}
					}
					if err := s.expireRequestMetrics(t.Context(), now.Add(401*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					if err := s.expireRequestMetrics(t.Context(), now.Add(401*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					history, err = m.history(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil || len(history.Buckets) != 0 {
						t.Fatalf("expired history: %+v, %v", history, err)
					}
				})
			}
		})
	}
}
