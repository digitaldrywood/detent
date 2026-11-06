package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func metricImportSource(t *testing.T, db *sql.DB, schema string, organization tracker.OrganizationID) postgres.MetricsImportSource {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	source := postgres.MetricsImportSource{DB: db, Catalog: catalog, Source: storagecatalog.Source{Schema: schema, Store: "metrics", CatalogVersion: catalog.Version, OrganizationID: string(organization)}, Schema: storagecatalog.Schema{}, SnapshotID: "example-frozen-snapshot", RetainAt: time.Date(2026, 11, 1, 12, 34, 0, 123456000, time.UTC)}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	for name, table := range catalog.Tables {
		name, ok := strings.CutPrefix(name, "metrics/")
		if !ok {
			continue
		}
		source.Schema[name] = map[string]string{}
		var columns []string
		for column, definition := range table.Columns {
			source.Schema[name][column] = definition.Source
			kind := `text COLLATE "C"`
			if column == "histogram" {
				kind = "bytea"
			}
			columns = append(columns, pgx.Identifier{column}.Sanitize()+" "+kind)
		}
		if table.Disposition != "target" {
			continue
		}
		slices.Sort(columns)
		keys := map[string]string{"tenant": "organization", "request_days": "day,route,caller_kind,status_class", "request_minutes": "minute,route,caller_kind,caller_id,status_class", "request_hours": "hour,route,status_class"}[name]
		statement := "CREATE TABLE " + pgx.Identifier{schema}.Sanitize() + "." + pgx.Identifier{name}.Sanitize() + "(" + strings.Join(columns, ",") + ",PRIMARY KEY(" + keys + "))"
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO "+pgx.Identifier{schema}.Sanitize()+".tenant VALUES($1)", organization); err != nil {
		t.Fatal(err)
	}
	return source
}

func insertImportMetric(t *testing.T, source postgres.MetricsImportSource, grain metricsstore.Grain, m metricsstore.Metric) {
	t.Helper()
	histogram, err := json.Marshal(m.Histogram)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{string(grain): strconv.FormatInt(m.At.Unix(), 10), "route": m.Route, "caller_kind": m.CallerKind, "caller_id": m.CallerID, "status_class": strconv.FormatInt(m.StatusClass, 10), "count": strconv.FormatInt(m.Count, 10), "sum_ms": strconv.FormatFloat(m.Sum, 'g', -1, 64), "max_ms": strconv.FormatFloat(m.Max, 'g', -1, 64), "histogram": histogram, "p50_ms": "rebuild", "p95_ms": "rebuild"}
	table := "request_" + string(grain) + "s"
	var columns []string
	for name := range source.Schema[table] {
		columns = append(columns, name)
	}
	slices.Sort(columns)
	args, placeholders := make([]any, len(columns)), make([]string, len(columns))
	for n, column := range columns {
		args[n], placeholders[n], columns[n] = values[column], fmt.Sprintf("$%d", n+1), pgx.Identifier{column}.Sanitize()
	}
	statement := "INSERT INTO " + pgx.Identifier{source.Source.Schema}.Sanitize() + "." + pgx.Identifier{table}.Sanitize() + "(" + strings.Join(columns, ",") + ") VALUES(" + strings.Join(placeholders, ",") + ")"
	if _, err := source.DB.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func finishMetricImport(t *testing.T, r *postgres.Repository, source postgres.MetricsImportSource, limit int) postgres.MetricsImportProgress {
	t.Helper()
	previous := int64(-1)
	for {
		importer, err := r.NewMetricsImporter(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		progress, err := importer.Step(t.Context(), limit)
		if err != nil {
			t.Fatal(err)
		}
		if previous >= 0 && progress.Processed-previous > int64(limit) {
			t.Fatal("import exceeded its page budget")
		}
		previous = progress.Processed
		if progress.Done {
			return progress
		}
	}
}

func TestRequestMetricImportRetainsHistoryAcrossRestart(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	for index, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		source := metricImportSource(t, db, fmt.Sprintf("raw_metrics_%d", index), organization)
		now := source.RetainAt
		dayCutoff := now.Add(-metricsstore.DayRetention).Truncate(24 * time.Hour)
		minuteCutoff := now.Add(-metricsstore.MinuteRetention).Truncate(time.Minute)
		hourCutoff := now.Add(-metricsstore.HourRetention).Truncate(time.Hour)
		precise := metricSample(minuteCutoff, "GET /edge", "same-caller")
		precise.CallerKind, precise.StatusClass = "mcp", 5
		precise.Latency = metricsstore.Latency{Count: 9007199254740993 + int64(index), Sum: 1.125 + float64(index), Max: 0.125, Histogram: [64]int64{7: 9007199254740993 + int64(index)}}
		day := metricSample(now.Add(-20*24*time.Hour).Truncate(24*time.Hour), "GET /compacted", "")
		boundaryDay := metricSample(dayCutoff, "GET /day-boundary", "")
		minute := metricSample(now.Truncate(time.Minute), "GET /first", "same-caller")
		secondMinute := minute
		secondMinute.Route, secondMinute.StatusClass = "GET /second", 4
		overflow := minute
		overflow.CallerKind, overflow.CallerID = "other", ""
		hour := precise
		hour.At, hour.CallerKind, hour.CallerID = hourCutoff, "", ""
		fixtures := []struct {
			grain metricsstore.Grain
			m     metricsstore.Metric
		}{
			{metricsstore.Day, boundaryDay}, {metricsstore.Day, day},
			{metricsstore.Day, metricSample(dayCutoff.Add(-24*time.Hour), "GET /expired", "")},
			{metricsstore.Minute, metricSample(day.At.Add(time.Minute), day.Route, "expired-caller")},
			{metricsstore.Minute, metricSample(dayCutoff.Add(-time.Minute), "GET /expired", "")},
			{metricsstore.Minute, precise}, {metricsstore.Minute, overflow}, {metricsstore.Minute, minute}, {metricsstore.Minute, secondMinute},
			{metricsstore.Hour, hour}, {metricsstore.Hour, metricSample(hourCutoff.Add(-time.Hour), "GET /expired", "")},
		}
		for _, fixture := range fixtures {
			insertImportMetric(t, source, fixture.grain, fixture.m)
		}
		progress := finishMetricImport(t, r, source, 2)
		if progress.Processed != int64(len(fixtures)) {
			t.Fatalf("processed=%d, want %d", progress.Processed, len(fixtures))
		}
		for repeat := range 2 {
			if repeat == 1 {
				replay := finishMetricImport(t, r, source, 2)
				if replay != progress {
					t.Fatalf("replay changed progress: %+v", replay)
				}
			}
			day.Count, day.Sum, day.Histogram = 8, 48, [64]int64{}
			boundaryDay.Histogram = [64]int64{}
			for _, tc := range []struct {
				grain metricsstore.Grain
				want  []metricsstore.Metric
			}{{metricsstore.Minute, []metricsstore.Metric{precise, overflow, minute, secondMinute}}, {metricsstore.Hour, []metricsstore.Metric{hour}}, {metricsstore.Day, []metricsstore.Metric{boundaryDay, day}}} {
				got := readMetrics(t, r, organization, tc.grain, dayCutoff.Add(-24*time.Hour), now.Add(time.Hour))
				for n := range got {
					got[n].At = got[n].At.UTC()
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("tenant=%s grain=%s got=%+v want=%+v", organization, tc.grain, got, tc.want)
				}
			}
		}
	}
}

func TestRequestMetricImportRollsBackPageAndCheckpoint(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	source := metricImportSource(t, db, "raw_metrics", "org_first")
	for _, route := range []string{"GET /first", "GET /second"} {
		insertImportMetric(t, source, metricsstore.Minute, metricSample(source.RetainAt.Truncate(time.Minute), route, "same-caller"))
	}
	importer, err := r.NewMetricsImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Step(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE request_minutes ADD CONSTRAINT example_import_failure CHECK(route <> 'GET /second')`); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Step(t.Context(), 2); err == nil {
		t.Fatal("target failure accepted")
	}
	for _, table := range []string{"request_minutes", "request_metric_windows", "request_metric_callers"} {
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("page rollback %s count=%d err=%v", table, count, err)
		}
	}
	var processed int64
	if err := db.QueryRowContext(t.Context(), "SELECT processed FROM request_metric_imports").Scan(&processed); err != nil || processed != 0 {
		t.Fatalf("checkpoint advanced: %d %v", processed, err)
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE request_minutes DROP CONSTRAINT example_import_failure`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := importer.Step(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	progress := finishMetricImport(t, r, source, 2)
	if progress.Processed != 2 {
		t.Fatalf("recovered checkpoint=%+v", progress)
	}
	var callers int64
	if err := db.QueryRowContext(t.Context(), "SELECT caller_count FROM request_metric_windows").Scan(&callers); err != nil || callers != 1 {
		t.Fatalf("restored caller reservation=%d err=%v", callers, err)
	}
	if got := readMetrics(t, r, "org_first", metricsstore.Minute, source.RetainAt.Add(-time.Hour), source.RetainAt.Add(time.Hour)); len(got) != 2 || got[0].Count != 4 || got[1].Count != 4 {
		t.Fatalf("page replay=%+v", got)
	}
}

func TestRequestMetricImportConcurrentReplay(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 1, BackgroundConnections: 3})
	source := metricImportSource(t, db, "raw_metrics", "org_first")
	for n := range metricsstore.CallerLimit + 2 {
		insertImportMetric(t, source, metricsstore.Minute, metricSample(source.RetainAt.Truncate(time.Minute), "GET /example", fmt.Sprintf("caller_%03d", n)))
	}
	importer, err := r.NewMetricsImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			for {
				progress, err := importer.Step(t.Context(), 50)
				if errors.Is(err, corestore.ErrRevisionConflict) {
					continue
				}
				if err != nil || progress.Done {
					failures <- err
					return
				}
			}
		})
	}
	close(start)
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := readMetrics(t, r, "org_first", metricsstore.Minute, source.RetainAt.Add(-time.Hour), source.RetainAt.Add(time.Hour))
	if len(rows) != metricsstore.CallerLimit+2 {
		t.Fatalf("import collapsed callers: %d", len(rows))
	}
	for _, row := range rows {
		if row.Count != 4 || row.CallerID == "" {
			t.Fatalf("replay changed metric: %+v", row)
		}
	}
	var reservations int64
	if err := db.QueryRowContext(t.Context(), `SELECT caller_count FROM request_metric_windows`).Scan(&reservations); err != nil || reservations != metricsstore.CallerLimit {
		t.Fatalf("caller budget=%d err=%v", reservations, err)
	}
	if err := r.MergeRequestMetrics(t.Context(), "org_first", source.RetainAt, []metricsstore.Metric{metricSample(source.RetainAt, "GET /example", "new-caller")}); err != nil {
		t.Fatal(err)
	}
	rows = readMetrics(t, r, "org_first", metricsstore.Minute, source.RetainAt.Add(-time.Hour), source.RetainAt.Add(time.Hour))
	if len(rows) != metricsstore.CallerLimit+3 || rows[0].CallerKind != "other" || rows[0].CallerID != "" {
		t.Fatalf("live caller admission ignored imported reservations: %+v", rows)
	}
}

func TestRequestMetricImportRejectsChangedSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *postgres.MetricsImportSource)
		want   error
	}{
		{"tenant mismatch", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			source.Source.OrganizationID = "org_second"
		}, storagecatalog.ErrOrganization},
		{"unknown column", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			if _, err := source.DB.ExecContext(t.Context(), `ALTER TABLE raw_metrics.request_minutes ADD COLUMN unmapped text`); err != nil {
				t.Fatal(err)
			}
		}, storagecatalog.ErrUnmapped},
		{"missing cursor index", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			if _, err := source.DB.ExecContext(t.Context(), `ALTER TABLE raw_metrics.request_minutes DROP CONSTRAINT request_minutes_pkey`); err != nil {
				t.Fatal(err)
			}
		}, postgres.ErrMetricImport},
		{"snapshot mismatch", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			source.SnapshotID = "another-snapshot"
		}, postgres.ErrMetricImport},
		{"retention mismatch", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			source.RetainAt = source.RetainAt.Add(time.Hour)
		}, postgres.ErrMetricImport},
		{"catalog mismatch", func(t *testing.T, source *postgres.MetricsImportSource) {
			t.Helper()
			table := source.Catalog.Tables["metrics/request_minutes"]
			table.Contract += " changed"
			source.Catalog.Tables["metrics/request_minutes"] = table
		}, postgres.ErrMetricImport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := template.Database(t)
			r, _ := repositoryFor(t, db, smallPool())
			source := metricImportSource(t, db, "raw_metrics", "org_first")
			importer, err := r.NewMetricsImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := importer.Step(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			tc.change(t, &source)
			importer, err = r.NewMetricsImporter(t.Context(), source)
			if err == nil {
				_, err = importer.Step(t.Context(), 1)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("source change=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestRequestMetricImportRejectsMalformedPage(t *testing.T) {
	for _, tc := range []struct{ name, column, value string }{
		{"histogram length", "histogram", "[4]"},
		{"histogram mismatch", "count", "5"},
		{"count overflow", "count", "9223372036854775808"},
		{"nonfinite sum", "sum_ms", "NaN"},
		{"negative max", "max_ms", "-1"},
		{"unaligned time", "minute", "1793536441"},
		{"unsupported caller", "caller_kind", "unsupported"},
		{"status out of range", "status_class", "10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := template.Database(t)
			r, _ := repositoryFor(t, db, smallPool())
			source := metricImportSource(t, db, "raw_metrics", "org_first")
			insertImportMetric(t, source, metricsstore.Minute, metricSample(source.RetainAt.Truncate(time.Minute), "GET /example", "caller"))
			var value any = tc.value
			if tc.column == "histogram" {
				value = []byte(tc.value)
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE raw_metrics.request_minutes SET "+pgx.Identifier{tc.column}.Sanitize()+"=$1", value); err != nil {
				t.Fatal(err)
			}
			importer, err := r.NewMetricsImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := importer.Step(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			if _, err := importer.Step(t.Context(), 1); !errors.Is(err, storagecatalog.ErrValue) {
				t.Fatalf("malformed row accepted: %v", err)
			}
			var processed int
			if err := db.QueryRowContext(t.Context(), "SELECT processed FROM request_metric_imports").Scan(&processed); err != nil || processed != 0 {
				t.Fatalf("malformed page advanced: %d %v", processed, err)
			}
		})
	}
}

func TestRequestMetricImportCheckpointIsolation(t *testing.T) {
	db := template.Database(t)
	r, pool := repositoryFor(t, db, smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		source := metricImportSource(t, db, "raw_"+string(organization), organization)
		finishMetricImport(t, r, source, 1)
	}
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct {
		organization string
		want         int
	}{{"", 0}, {"org_first", 1}, {"org_second", 1}, {"org_unknown", 0}} {
		t.Run(tc.organization, func(t *testing.T) {
			tx := tenantTx(t, runtime, pool, tc.organization)
			var count int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM request_metric_imports`).Scan(&count); err != nil || count != tc.want {
				t.Fatalf("checkpoint scope=%d err=%v", count, err)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	tx := tenantTx(t, runtime, pool, "org_first")
	result, err := tx.Exec(t.Context(), `UPDATE request_metric_imports SET processed=99 WHERE organization_id='org_second'`)
	if err != nil {
		t.Fatal(err)
	}
	if changed := result.RowsAffected(); changed != 0 {
		t.Fatalf("foreign checkpoint changed=%d err=%v", changed, err)
	}
	_, err = tx.Exec(t.Context(), `UPDATE request_metric_imports SET organization_id='org_foreign'`)
	requireSQLState(t, err, "42501")
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRequestMetricImportRejectsExistingHistoryAndInvalidPage(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	source := metricImportSource(t, db, "raw_metrics", "org_first")
	importer, err := r.NewMetricsImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, metricsstore.MergeLimit + 1} {
		if _, err := importer.Step(t.Context(), limit); !errors.Is(err, postgres.ErrMetricImport) {
			t.Fatalf("invalid page accepted: %v", err)
		}
	}
	if err := r.MergeRequestMetrics(t.Context(), "org_first", source.RetainAt, []metricsstore.Metric{metricSample(source.RetainAt, "GET /example", "caller")}); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Step(t.Context(), 1); !errors.Is(err, postgres.ErrMetricImport) {
		t.Fatalf("existing history accepted: %v", err)
	}
}
