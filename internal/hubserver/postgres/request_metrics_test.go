package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func metricSample(at time.Time, route, caller string) metricsstore.Metric {
	return metricsstore.Metric{At: at, Route: route, CallerKind: "runner", CallerID: caller, StatusClass: 2, Latency: metricsstore.Latency{Count: 4, Sum: 24, Max: 16, Histogram: [64]int64{10: 2, 13: 1, 14: 1}}}
}

func readMetrics(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, grain metricsstore.Grain, from, to time.Time) []metricsstore.Metric {
	t.Helper()
	page := metricsstore.Page{Grain: grain, From: from, To: to, Limit: metricsstore.MergeLimit}
	var all []metricsstore.Metric
	for {
		rows, err := r.RequestMetrics(t.Context(), org, page)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if len(rows) < page.Limit {
			return all
		}
		page.After = &rows[len(rows)-1]
	}
}

func TestRequestMetricsConcurrentMerge(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		separateMinutes bool
	}{{"same keys", false}, {"shared hours across different minutes", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), postgres.PoolConfig{MaxConnections: 10, ServingConnections: 2, BackgroundConnections: 8})
			const workers, iterations = 8, 8
			start := make(chan struct{})
			failures := make(chan error, workers)
			var wg sync.WaitGroup
			for worker := range workers {
				wg.Go(func() {
					<-start
					org := tracker.OrganizationID(fmt.Sprintf("org_%d", worker%2))
					at := now
					if tc.separateMinutes {
						at = at.Add(time.Duration(worker/2) * time.Minute)
					}
					for i := range iterations {
						records := []metricsstore.Metric{metricSample(at, "GET /first", "caller_first"), metricSample(at, "GET /second", "caller_second")}
						if i%2 == 0 {
							records[0], records[1] = records[1], records[0]
						}
						if err := r.MergeRequestMetrics(t.Context(), org, now, records); err != nil {
							failures <- err
							return
						}
					}
				})
			}
			close(start)
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			for _, org := range []tracker.OrganizationID{"org_0", "org_1"} {
				for _, grain := range []metricsstore.Grain{metricsstore.Minute, metricsstore.Hour} {
					rows := readMetrics(t, r, org, grain, now, now.Add(time.Hour))
					totals := map[string]metricsstore.Latency{}
					for _, row := range rows {
						l := totals[row.Route]
						l.Count += row.Count
						l.Sum += row.Sum
						l.Max = max(l.Max, row.Max)
						for i, n := range row.Histogram {
							l.Histogram[i] += n
						}
						totals[row.Route] = l
					}
					want := metricsstore.Latency{Count: 128, Sum: 768, Max: 16, Histogram: [64]int64{10: 64, 13: 32, 14: 32}}
					for _, route := range []string{"GET /first", "GET /second"} {
						if got := totals[route]; got != want {
							t.Errorf("%s %s %s = %+v, want %+v", org, grain, route, got, want)
						}
						p50, p95 := totals[route].Percentiles()
						if p50 != 1.024 || p95 != 16 {
							t.Errorf("percentiles = %g,%g", p50, p95)
						}
					}
				}
			}
			if rows := readMetrics(t, r, "org_unknown", metricsstore.Minute, now, now.Add(time.Hour)); len(rows) != 0 {
				t.Fatalf("unknown tenant read %d metrics", len(rows))
			}
		})
	}
}

func TestRequestMetricsBucketRoundTripAndPages(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, route := range []string{"GET /first", "GET /second"} {
			m := metricSample(now, route, "same_caller")
			m.Count = 2080
			m.Sum = 2e16
			m.Max = 1e16
			for i := range m.Histogram {
				m.Histogram[i] = int64(i + 1)
			}
			if err := r.MergeRequestMetrics(t.Context(), org, now, []metricsstore.Metric{m}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, grain := range []metricsstore.Grain{metricsstore.Minute, metricsstore.Hour} {
		t.Run(string(grain), func(t *testing.T) {
			page := metricsstore.Page{Grain: grain, From: now, To: now.Add(time.Hour), Limit: 1}
			for _, route := range []string{"GET /first", "GET /second"} {
				rows, err := r.RequestMetrics(t.Context(), "org_first", page)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Route != route || rows[0].Count != 2080 || rows[0].Sum != 2e16 || rows[0].Max != 1e16 {
					t.Fatalf("page = %+v", rows)
				}
				for i, n := range rows[0].Histogram {
					if n != int64(i+1) {
						t.Errorf("bucket %d = %d", i, n)
					}
				}
				page.After = &rows[0]
			}
			rows, err := r.RequestMetrics(t.Context(), "org_first", page)
			if err != nil || len(rows) != 0 {
				t.Fatalf("final page = %+v, %v", rows, err)
			}
			rows = readMetrics(t, r, "org_first", grain, now.Add(-time.Hour), now)
			if len(rows) != 0 {
				t.Errorf("exclusive upper boundary returned %+v", rows)
			}
		})
	}
}

func TestRequestMetricsCallerLimit(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, _ := repositoryFor(t, template.Database(t), postgres.PoolConfig{MaxConnections: 10, ServingConnections: 2, BackgroundConnections: 8})
	start := make(chan struct{})
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			<-start
			records := make([]metricsstore.Metric, 30)
			for i := range records {
				records[i] = metricSample(now, "GET /example", fmt.Sprintf("caller_%03d", worker*30+i))
			}
			failures <- r.MergeRequestMetrics(t.Context(), "org_first", now, records)
		})
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := readMetrics(t, r, "org_first", metricsstore.Minute, now, now.Add(time.Hour))
	if len(rows) != metricsstore.CallerLimit+1 {
		t.Fatalf("caller rows = %d", len(rows))
	}
	var overflow int64
	var retained metricsstore.Metric
	for _, row := range rows {
		if row.CallerKind == metricsstore.OverflowKind && row.CallerID == metricsstore.OverflowCaller {
			overflow = row.Count
		} else {
			retained = row
		}
	}
	if overflow != 160 {
		t.Errorf("overflow count = %d, want 160", overflow)
	}
	if err := r.MergeRequestMetrics(t.Context(), "org_first", now, []metricsstore.Metric{metricSample(now, "GET /example", retained.CallerID)}); err != nil {
		t.Fatal(err)
	}
	if err := r.MergeRequestMetrics(t.Context(), "org_second", now, []metricsstore.Metric{metricSample(now, "GET /example", "independent_caller")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		org   tracker.OrganizationID
		count int64
	}{{"org_first", 964}, {"org_second", 4}} {
		rows := readMetrics(t, r, tc.org, metricsstore.Hour, now, now.Add(time.Hour))
		if len(rows) != 1 || rows[0].Count != tc.count || rows[0].Sum != float64(tc.count)*6 {
			t.Fatalf("hour total = %+v", rows)
		}
	}
	rows = readMetrics(t, r, "org_first", metricsstore.Minute, now, now.Add(time.Hour))
	for _, row := range rows {
		if row.CallerID == retained.CallerID && row.CallerKind == retained.CallerKind && row.Count != 8 {
			t.Errorf("retained caller = %+v", row)
		}
	}
}

func TestRequestMetricsRejectInvalidBatch(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, tc := range []struct {
		name   string
		change func(*metricsstore.Metric)
	}{
		{"negative bucket", func(m *metricsstore.Metric) { m.Histogram[0] = -1 }},
		{"wrong count", func(m *metricsstore.Metric) { m.Count++ }},
		{"negative count", func(m *metricsstore.Metric) { m.Count = -1 }},
		{"nan sum", func(m *metricsstore.Metric) { m.Sum = math.NaN() }},
		{"infinite max", func(m *metricsstore.Metric) { m.Max = math.Inf(1) }},
		{"unknown kind", func(m *metricsstore.Metric) { m.CallerKind = "unbounded" }},
		{"missing time", func(m *metricsstore.Metric) { m.At = time.Time{} }},
		{"invalid status", func(m *metricsstore.Metric) { m.StatusClass = 10 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			good := metricSample(now, "GET /example", "caller")
			bad := good
			tc.change(&bad)
			if err := r.MergeRequestMetrics(t.Context(), "org_first", now, []metricsstore.Metric{good, bad}); !errors.Is(err, metricsstore.ErrInvalidMetric) {
				t.Fatalf("error = %v", err)
			}
			if rows := readMetrics(t, r, "org_first", metricsstore.Minute, now, now.Add(time.Hour)); len(rows) != 0 {
				t.Fatalf("invalid batch wrote %+v", rows)
			}
		})
	}
	records := make([]metricsstore.Metric, metricsstore.MergeLimit+1)
	if err := r.MergeRequestMetrics(t.Context(), "org_first", now, records); !errors.Is(err, metricsstore.ErrInvalidMetric) {
		t.Fatalf("oversized batch = %v", err)
	}
	if err := r.MergeRequestMetrics(t.Context(), "", now, []metricsstore.Metric{metricSample(now, "GET /example", "caller")}); !errors.Is(err, postgres.ErrTenantRequired) {
		t.Fatalf("empty tenant = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.MergeRequestMetrics(ctx, "org_first", now, []metricsstore.Metric{metricSample(now, "GET /example", "caller")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled merge = %v", err)
	}
	for _, page := range []metricsstore.Page{{Grain: metricsstore.Minute, From: now, To: now.Add(time.Hour), Limit: 0}, {Grain: metricsstore.Minute, From: now, To: now.Add(time.Hour), Limit: 201}} {
		if _, err := r.RequestMetrics(t.Context(), "org_first", page); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("invalid page = %v", err)
		}
	}
}

func seedMetric(t *testing.T, pool *sql.DB, org tracker.OrganizationID, grain metricsstore.Grain, m metricsstore.Metric) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
		t.Fatal(err)
	}
	var query string
	args := []any{org, m.At, m.Route}
	switch grain {
	case metricsstore.Minute:
		query = `INSERT INTO request_minutes(organization_id,minute,route,caller_kind,caller_id,status_class,count,sum_ms,max_ms,histogram) VALUES($1,$2,$3,$4,$5,2,4,24,16,$6)`
		args = append(args, m.CallerKind, m.CallerID, pq.Array(m.Histogram[:]))
	case metricsstore.Hour:
		query = `INSERT INTO request_hours(organization_id,hour,route,status_class,count,sum_ms,max_ms,histogram) VALUES($1,$2,$3,2,4,24,16,$4)`
		args = append(args, pq.Array(m.Histogram[:]))
	case metricsstore.Day:
		query = `INSERT INTO request_days(organization_id,day,route,caller_kind,status_class,count,sum_ms,max_ms) VALUES($1,$2,$3,$4,2,4,24,16)`
		args = append(args, m.CallerKind)
	}
	if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestMetricsRetentionBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 34, 56, 123456789, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), smallPool())
	cases := []struct {
		grain           metricsstore.Grain
		retention, unit time.Duration
	}{{metricsstore.Minute, 14 * 24 * time.Hour, time.Minute}, {metricsstore.Hour, 90 * 24 * time.Hour, time.Hour}, {metricsstore.Day, 400 * 24 * time.Hour, 24 * time.Hour}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, tc := range cases {
			cutoff := now.Add(-tc.retention).UTC().Truncate(tc.unit)
			for _, boundary := range []struct {
				name   string
				offset time.Duration
			}{{"expired", -tc.unit}, {"exact", 0}, {"retained", tc.unit}} {
				seedMetric(t, pool, org, tc.grain, metricSample(cutoff.Add(boundary.offset), "GET /"+string(tc.grain)+"/"+boundary.name, "private_caller"))
			}
		}
		seedMetric(t, pool, org, metricsstore.Minute, metricSample(now.Add(-401*24*time.Hour), "GET /ancient", "private_caller"))
	}
	for _, tick := range []time.Time{now, now.Add(time.Nanosecond)} {
		if err := r.RetainRequestMetrics(t.Context(), "org_first", tick); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range cases {
		t.Run(string(tc.grain), func(t *testing.T) {
			rows := readMetrics(t, r, "org_first", tc.grain, now.Add(-402*24*time.Hour), now)
			routes := map[string]metricsstore.Metric{}
			for _, row := range rows {
				routes[row.Route] = row
			}
			for _, name := range []string{"exact", "retained"} {
				row, ok := routes["GET /"+string(tc.grain)+"/"+name]
				if !ok || row.Count != 4 || row.Sum != 24 || row.Max != 16 {
					t.Errorf("%s: %+v", name, row)
				}
			}
			if _, ok := routes["GET /"+string(tc.grain)+"/expired"]; ok {
				t.Error("expired row retained")
			}
			if _, ok := routes["GET /ancient"]; ok {
				t.Error("row older than day retention survived")
			}
			if tc.grain == metricsstore.Day {
				row := routes["GET /minute/expired"]
				if row.Count != 4 || row.Sum != 24 || row.Max != 16 || row.CallerID != "" || row.CallerKind != "runner" {
					t.Errorf("daily compaction = %+v", row)
				}
			}
			other := readMetrics(t, r, "org_second", tc.grain, now.Add(-402*24*time.Hour), now)
			want := 3
			if tc.grain == metricsstore.Minute {
				want = 4
			}
			if len(other) != want {
				t.Errorf("other tenant changed: %+v", other)
			}
		})
	}
}

func TestRequestMetricsRetentionResumes(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), smallPool())
	for i := range metricsstore.RetentionLimit + 1 {
		m := metricSample(now.Add(-15*24*time.Hour), "GET /example", fmt.Sprintf("caller_%d", i))
		seedMetric(t, pool, "org_first", metricsstore.Minute, m)
	}
	for _, want := range []int64{800, 804, 804} {
		if err := r.RetainRequestMetrics(t.Context(), "org_first", now); err != nil {
			t.Fatal(err)
		}
		days := readMetrics(t, r, "org_first", metricsstore.Day, now.Add(-16*24*time.Hour), now)
		if len(days) != 1 || days[0].Count != want || days[0].Sum != float64(want)*6 {
			t.Fatalf("resumed day = %+v, want count %d", days, want)
		}
	}
	if rows := readMetrics(t, r, "org_first", metricsstore.Minute, now.Add(-16*24*time.Hour), now); len(rows) != 0 {
		t.Errorf("expired minutes remain: %d", len(rows))
	}
}

func TestRequestMetricsWriteRetention(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, tc := range []struct {
		name           string
		age            time.Duration
		minutes, hours int
	}{{"minute cutoff", 14 * 24 * time.Hour, 1, 1}, {"before minute cutoff", 14*24*time.Hour + time.Minute, 0, 1}, {"hour cutoff", 90 * 24 * time.Hour, 0, 1}, {"before hour cutoff", 90*24*time.Hour + time.Hour, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			m := metricSample(now.Add(-tc.age), "GET /"+tc.name, "caller")
			if err := r.MergeRequestMetrics(t.Context(), "org_first", now, []metricsstore.Metric{m}); err != nil {
				t.Fatal(err)
			}
			for _, g := range []struct {
				grain metricsstore.Grain
				want  int
			}{{metricsstore.Minute, tc.minutes}, {metricsstore.Hour, tc.hours}} {
				rows := readMetrics(t, r, "org_first", g.grain, m.At.Truncate(time.Hour), m.At.Add(time.Hour))
				var matching []metricsstore.Latency
				for _, row := range rows {
					if row.Route == m.Route {
						matching = append(matching, row.Latency)
					}
				}
				if len(matching) != g.want {
					t.Fatalf("%s retained %d, want %d", g.grain, len(matching), g.want)
				}
				if g.want == 1 && !reflect.DeepEqual(matching[0], m.Latency) {
					t.Errorf("latency = %+v", matching[0])
				}
			}
		})
	}
}

func TestRequestMetricsConcurrentRetention(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), postgres.PoolConfig{MaxConnections: 10, ServingConnections: 2, BackgroundConnections: 8})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for i := range 40 {
			seedMetric(t, pool, org, metricsstore.Minute, metricSample(now.Add(-15*24*time.Hour), "GET /example", fmt.Sprintf("caller_%d", i)))
		}
	}
	start := make(chan struct{})
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { <-start; failures <- r.RetainRequestMetrics(t.Context(), "org_first", now) })
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	days := readMetrics(t, r, "org_first", metricsstore.Day, now.Add(-16*24*time.Hour), now)
	if len(days) != 1 || days[0].Count != 160 || days[0].Sum != 960 || days[0].Max != 16 {
		t.Fatalf("concurrent daily compaction = %+v", days)
	}
	if rows := readMetrics(t, r, "org_first", metricsstore.Minute, now.Add(-16*24*time.Hour), now); len(rows) != 0 {
		t.Errorf("compacted minutes remain: %d", len(rows))
	}
	if rows := readMetrics(t, r, "org_second", metricsstore.Minute, now.Add(-16*24*time.Hour), now); len(rows) != 40 {
		t.Errorf("other tenant minute count = %d", len(rows))
	}
}

func TestRequestMetricsRetentionBatchLimits(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r, pool := repositoryFor(t, template.Database(t), smallPool())
	runtime := runtimeFor(t, pool, pool)
	for _, org := range []string{"org_first", "org_second"} {
		tx := tenantTx(t, runtime, pool, org)
		for _, seed := range []struct {
			query     string
			histogram bool
		}{
			{`INSERT INTO request_hours(organization_id,hour,route,status_class,count,sum_ms,max_ms,histogram) SELECT $1,$2,'GET /'||i,2,4,24,16,$3 FROM generate_series(1,201) i`, true},
			{`INSERT INTO request_days(organization_id,day,route,caller_kind,status_class,count,sum_ms,max_ms) SELECT $1,$2,'GET /'||i,'runner',2,4,24,16 FROM generate_series(1,201) i`, false},
			{`INSERT INTO request_metric_windows(organization_id,minute,caller_count) SELECT $1,$2::timestamptz+i*INTERVAL '1 minute',2 FROM generate_series(1,201) i`, false},
		} {
			args := []any{org, now.Add(-401 * 24 * time.Hour)}
			if seed.histogram {
				sample := metricSample(now, "GET /example", "caller")
				args = append(args, pq.Array(sample.Histogram[:]))
			}
			if _, err := tx.ExecContext(t.Context(), seed.query, args...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO request_metric_callers(organization_id,minute,caller_kind,caller_id) SELECT organization_id,minute,'runner',c FROM request_metric_windows CROSS JOIN (VALUES('first'),('second')) caller(c) WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int{1, 0} {
		if err := r.RetainRequestMetrics(t.Context(), "org_first", now); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			table      string
			multiplier int
		}{{"request_hours", 1}, {"request_days", 1}, {"request_metric_windows", 1}, {"request_metric_callers", 2}} {
			for _, org := range []string{"org_first", "org_second"} {
				tx := tenantTx(t, runtime, pool, org)
				var count int
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+tc.table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				expected := want * tc.multiplier
				if org == "org_second" {
					expected = 201 * tc.multiplier
				}
				if count != expected {
					t.Errorf("%s %s rows = %d, want %d", org, tc.table, count, expected)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
