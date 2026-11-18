package requestmetrics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestMetricsPostgres(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, backend := range []string{"postgres"} {
		t.Run(string(backend), func(t *testing.T) {
			store := hubtest.Repository(t)
			for tenant, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(string(org), func(t *testing.T) {
					owner := store
					m, err := New(Deps{Repository: owner, Organization: org})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := m.Close(); err != nil {
							t.Error(err)
						}
					})
					if backend == "postgres" && (m.Repository == nil) {
						t.Fatal("PostgreSQL collector opened SQLite")
					}
					m.usage = &BillingOwner{Plans: func() *BillingPlan { return nil }}
					for i := range metricsstore.MergeLimit + 1 {
						for range tenant + 1 {
							m.Record(now.Add(-time.Minute), fmt.Sprintf("GET /route/%03d", i), "runner", "caller", 200, time.Millisecond)
						}
					}
					for key, value := range m.pending {
						value.HTTP.Requests = 1
						m.pending[key] = value
					}
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					if err := m.Flush(ctx, now); !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled flush: %v", err)
					}
					if len(m.pending) != metricsstore.MergeLimit+1 {
						t.Fatalf("failed flush lost pending metrics: %d", len(m.pending))
					}
					if err := m.Flush(t.Context(), now); err != nil {
						t.Fatal(err)
					}
					if backend == "postgres" && len(m.pending) != 1 {
						t.Fatalf("flush exceeded batch budget: %d pending", len(m.pending))
					}
					if backend == "postgres" {
						if err := m.Flush(ctx, now); !errors.Is(err, context.Canceled) {
							t.Fatalf("failed second batch: %v", err)
						}
						if len(m.pending) != 1 {
							t.Fatalf("retry lost uncommitted latency: %d", len(m.pending))
						}
					}
					if err := m.Drain(t.Context(), now); err != nil {
						t.Fatal(err)
					}
					report, err := m.Report(t.Context(), now)
					if err != nil {
						t.Fatal(err)
					}
					if len(report.Callers) != 1 || report.Callers[0].Count != int64((metricsstore.MergeLimit+1)*(tenant+1)) || len(report.Routes) != metricsstore.RouteLimit {
						t.Fatalf("report: %+v", report)
					}
					if report.Callers[0].P50 != 1 || report.Callers[0].P95 != 1 {
						t.Fatalf("percentiles: %+v", report.Callers)
					}
					history, err := m.History(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					if len(history.Buckets) != metricsstore.RouteLimit || history.Buckets[0].Count != int64(tenant+1) {
						t.Fatalf("history: %+v", history)
					}
					s := m
					if err := s.Retain(t.Context(), now.Add(15*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					if err := s.Retain(t.Context(), now.Add(15*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					report, err = m.Report(t.Context(), now)
					if err != nil || len(report.Callers) != 0 {
						t.Fatalf("retention report: %+v, %v", report, err)
					}
					history, err = m.History(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil || len(history.Buckets) != metricsstore.RouteLimit {
						t.Fatalf("hour retention: %+v, %v", history, err)
					}
					if backend == "postgres" {
						rows, err := m.Repository.RequestMetrics(t.Context(), org, metricsstore.Page{Grain: metricsstore.Day, From: now.Add(-24 * time.Hour), To: now, Limit: metricsstore.MergeLimit})
						if err != nil || len(rows) != metricsstore.MergeLimit {
							t.Fatalf("retained days: %d, %v", len(rows), err)
						}
						var total int64
						for _, row := range rows {
							total += row.Count
						}
						after := rows[len(rows)-1]
						rows, err = m.Repository.RequestMetrics(t.Context(), org, metricsstore.Page{Grain: metricsstore.Day, From: now.Add(-24 * time.Hour), To: now, After: &after, Limit: metricsstore.MergeLimit})
						if err != nil || len(rows) != 1 {
							t.Fatalf("day cursor: %d, %v", len(rows), err)
						}
						total += rows[0].Count
						if total != int64((metricsstore.MergeLimit+1)*(tenant+1)) {
							t.Fatalf("day total: %d", total)
						}
					}
					if err := s.Retain(t.Context(), now.Add(401*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					if err := s.Retain(t.Context(), now.Add(401*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					history, err = m.History(t.Context(), now.Add(-time.Hour), now, time.Hour)
					if err != nil || len(history.Buckets) != 0 {
						t.Fatalf("expired history: %+v, %v", history, err)
					}
				})
			}
		})
	}
}

type flushPageStore struct {
	metricsstore.Repository
	writes []int
}

func (s *flushPageStore) MergeRequestMetrics(_ context.Context, _ tracker.OrganizationID, _ time.Time, rows []metricsstore.Metric) error {
	s.writes = append(s.writes, len(rows))
	return nil
}

func TestMetricFlushPageLeavesRemainingKeysForLater(t *testing.T) {
	for _, test := range []struct{ limit, keys, want int }{{8, 7, 1}, {32, 12, 5}, {64, 19, 10}} {
		t.Run(fmt.Sprint(test.limit), func(t *testing.T) {
			store := &flushPageStore{}
			service, err := New(Deps{Repository: store, Organization: "org_example"})
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			for i := range test.keys {
				service.Record(at, "/items", "runner", fmt.Sprint(i), 200, time.Millisecond)
			}
			for remaining := test.keys; remaining > 0; remaining -= test.want {
				if err := service.FlushPage(t.Context(), at, test.limit); err != nil {
					t.Fatal(err)
				}
				if n := store.writes[len(store.writes)-1]; n != min(remaining, test.want) {
					t.Fatalf("page merged %d keys, want %d", n, min(remaining, test.want))
				}
			}
			if len(service.pending) != 0 {
				t.Fatalf("%d keys left after drain", len(service.pending))
			}
			service.usage = &BillingOwner{Plans: func() *BillingPlan { t.Fatal("clean view retirement called billing maintenance"); return nil }}
			if err := service.Drain(t.Context(), at); err != nil {
				t.Fatal(err)
			}

		})
	}
}
