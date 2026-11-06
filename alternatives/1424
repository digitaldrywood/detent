package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsSampleReferencesKeepScopedIdentity(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(strconv.FormatBool(replacement), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			d := f.service.database
			write := func(fixture nativeFixture, id string) {
				t.Helper()
				tx, err := d.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				sample := analyticsResidenceSample{ID: id, At: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), LanesNS: map[string]int64{"Todo": 123}}
				if err := writeAnalyticsResidenceSample(t.Context(), tx, nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}, "residence_edge", "example:sample", sample); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			write(f, "example-original")
			var row int64
			var reference string
			if err := d.reader.QueryRowContext(t.Context(), `SELECT rowid,`+analyticsSampleColumn("")+` FROM analytics_hourly_rollups WHERE project_id=? AND dimension='example:sample'`, f.project.ID).Scan(&row, &reference); err != nil {
				t.Fatal(err)
			}
			if replacement {
				if _, err := d.db.ExecContext(t.Context(), `DELETE FROM analytics_hourly_rollups WHERE rowid=?`, row); err != nil {
					t.Fatal(err)
				}
				write(other, "example-replacement")
				if _, err := d.db.ExecContext(t.Context(), `UPDATE analytics_hourly_rollups SET rowid=? WHERE project_id=? AND dimension='example:sample'`, row, other.project.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := d.db.ExecContext(t.Context(), `UPDATE analytics_hourly_rollups SET rowid=rowid+1000000 WHERE rowid=?`, row); err != nil {
					t.Fatal(err)
				}
				if _, err := d.db.ExecContext(t.Context(), `VACUUM`); err != nil {
					t.Fatal(err)
				}
			}
			var sample analyticsResidenceSample
			err := decodeAnalyticsSample(t.Context(), d.reader, reference, &sample)
			if replacement {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("reference followed a reused row into another project: sample=%+v err=%v", sample, err)
				}
			} else if err != nil || sample.ID != "example-original" || sample.LanesNS["Todo"] != 123 {
				t.Fatalf("stable sample identity did not survive VACUUM: sample=%+v err=%v", sample, err)
			}
		})
	}
}

func TestAnalyticsRollupInlineBytesIgnoreSourcePayload(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	for _, fixture := range []nativeFixture{f, other} {
		for _, metric := range []string{"attempt", "event", "residence_hour", "residence_edge", "completion", "residence_merge", "merge_entry", "capacity_claim", "capacity_wait", "skip_sample"} {
			t.Run(string(fixture.project.ID)+"/"+metric, func(t *testing.T) {
				var sizes []int
				for _, size := range []int{0, 1 << 20} {
					raw, err := json.Marshal(map[string]any{"ID": "example-item", "work_item_id": "example-item", "started_at": formatHubTime(f.service.config.now()), "status": "failed", "At": f.service.config.now(), "AtNS": f.service.config.now().UnixNano(), "completion_body": strings.Repeat("x", size)})
					if err != nil {
						t.Fatal(err)
					}
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if err := writeAnalyticsSample(t.Context(), tx, string(fixture.project.OrganizationID), string(fixture.project.ID), f.service.config.now().Truncate(time.Hour).Unix(), metric, "example-sample", 1, string(raw)); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					var bytes int
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT length(data_json) FROM analytics_hourly_rollups WHERE project_id=? AND metric=? AND dimension='example-sample'`, fixture.project.ID, metric).Scan(&bytes); err != nil {
						t.Fatal(err)
					}
					sizes = append(sizes, bytes)
				}
				if sizes[0] != 2 || sizes[1] != sizes[0] {
					t.Fatalf("inline bytes grew with source payload: small=%d large=%d", sizes[0], sizes[1])
				}
			})
		}
	}
}

func TestAnalyticsPayloadConversionPreservesWindowReports(t *testing.T) {
	start := time.Date(2026, 10, 25, 0, 0, 0, 123, time.UTC)
	now := start.Add(3 * time.Hour)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	ctx := f.service.runDetailContext(t.Context())
	for _, fixture := range []nativeFixture{f, other} {
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		tx, err := f.service.database.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		issue, err := createNativeFixtureIssueTx(ctx, tx, scope, tracker.CreateIssue{Title: "example history", Body: nativeContractTestBody, State: "Todo"}, start)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range []struct {
			kind string
			at   time.Time
			data tracker.CollaborationData
		}{
			{"scheduler.decision", start.Add(30 * time.Minute), tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "capacity", RunnerID: "example-runner", WorkItemRevision: 1, At: start.Add(30 * time.Minute)}}},
			{"scheduler.decision", start.Add(time.Hour), tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", RunnerID: "example-runner", WorkItemRevision: 1, At: start.Add(time.Hour)}}},
			{"workflow.transitioned", start.Add(time.Hour), tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}},
			{"workflow.transitioned", start.Add(90 * time.Minute), tracker.CollaborationData{FromState: "In Progress", ToState: "Rework", ReasonDetail: "example repair"}},
			{"workflow.transitioned", start.Add(2 * time.Hour), tracker.CollaborationData{FromState: "Rework", ToState: "Merging"}},
			{"workflow.transitioned", now, tracker.CollaborationData{FromState: "Merging", ToState: "Done"}},
		} {
			if err := appendLegacyNativeHistory(ctx, tx, scope, string(issue.WorkItemID), event.kind, event.data, event.at); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.service.database.commit(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	drainTestAnalytics(t, f.service)
	windows := []operatortool.AnalyticsWindow{
		{From: start, To: start.Add(time.Hour), Bucket: time.Hour},
		{From: start.Add(time.Hour), To: now, Bucket: time.Hour},
		{From: start.Add(time.Hour - time.Nanosecond), To: now.Add(time.Nanosecond), Bucket: time.Hour},
		{From: start.Add(45 * time.Minute), To: now.Add(-15 * time.Minute), Bucket: time.Hour},
	}
	read := func() []any {
		t.Helper()
		var result []any
		q := analyticsRollupReader{f.service.database.reader}
		for _, fixture := range []nativeFixture{f, other} {
			scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
			for _, window := range windows {
				project, err := readNativeAnalytics(ctx, q, scope, operatortool.AnalyticsRequest{Limit: 1000}, window)
				if err != nil {
					t.Fatal(err)
				}
				report := nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: window, Projects: []nativeAnalyticsProject{project}}
				diagnostics, err := f.service.readHostedDiagnosticsFrom(ctx, report, q)
				if err != nil {
					t.Fatal(err)
				}
				diagnostics.StorageConversions = nil
				reports, err := f.service.readHostedReportsFrom(ctx, report, q)
				if err != nil {
					t.Fatal(err)
				}
				result = append(result, report, diagnostics, reports)
			}
		}
		return result
	}
	want := read()
	for i := range 2 {
		report, ok := want[i*12+6].(nativeAnalyticsReport)
		if !ok {
			t.Fatal("fixture observation is not an analytics report")
		}
		causes := report.Projects[0].LaneResidence.ReworkCauses
		if len(causes) != 1 || causes[0].FromState != "In Progress" || causes[0].ReasonDetail != "example repair" || causes[0].Count != 1 {
			t.Fatalf("missing rework cause in fixture: %+v", causes)
		}
	}
	rows, err := f.service.database.reader.QueryContext(ctx, `SELECT rowid,metric,`+analyticsSampleColumn("")+` FROM analytics_hourly_rollups WHERE sample_storage_version=1`)
	if err != nil {
		t.Fatal(err)
	}
	type legacyRow struct {
		id          int64
		metric, raw string
	}
	var legacy []legacyRow
	for rows.Next() {
		var row legacyRow
		if err := rows.Scan(&row.id, &row.metric, &row.raw); err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, row := range legacy {
		var sample any
		switch row.metric {
		case "capacity_claim", "capacity_wait":
			sample = &analyticsCapacitySample{}
		case "skip_sample":
			sample = &analyticsSkipSample{}
		case "residence_merge", "merge_entry":
			sample = &analyticsMergeSample{}
		default:
			sample = &analyticsResidenceSample{}
		}
		if err := decodeAnalyticsSample(ctx, f.service.database.reader, row.raw, sample); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(ctx, `UPDATE analytics_hourly_rollups SET data_json=?,sample_storage_version=0 WHERE rowid=?`, string(raw), row.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"analytics_sample_lanes", "analytics_sample_causes"} {
		if _, err := f.service.database.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	var completion int64
	var original string
	if err := f.service.database.reader.QueryRowContext(ctx, `SELECT rowid,data_json FROM analytics_hourly_rollups WHERE metric='completion' LIMIT 1`).Scan(&completion, &original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(ctx, `CREATE TRIGGER reject_sample_lane BEFORE INSERT ON analytics_sample_lanes BEGIN SELECT RAISE(ABORT,'example sample failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertAnalyticsSample(ctx, completion, now); err == nil {
		t.Fatal("conversion ignored child aggregate failure")
	}
	var retained string
	var version, children int
	if err := f.service.database.reader.QueryRowContext(ctx, `SELECT data_json,sample_storage_version,(SELECT count(*) FROM analytics_sample_lanes) FROM analytics_hourly_rollups WHERE rowid=?`, completion).Scan(&retained, &version, &children); err != nil || retained != original || version != 0 || children != 0 {
		t.Fatalf("failed conversion left partial sample: version=%d children=%d preserved=%t err=%v", version, children, retained == original, err)
	}
	if _, err := f.service.database.db.ExecContext(ctx, `DROP TRIGGER reject_sample_lane`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(ctx, `UPDATE analytics_hourly_rollups SET data_json=? WHERE metric IN ('event','issue')`, `{"work_item_id":"obsolete","copied_source":"`+strings.Repeat("x", 16<<10)+`"}`); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []nativeFixture{f, other} {
		for i := range storageConversionRows + 2 {
			at := start.Add(-48 * time.Hour)
			raw, err := json.Marshal(map[string]any{"At": at, "AtNS": at.UnixNano(), "Source": "example-source", "Reason": "example-reason", "obsolete": strings.Repeat("x", 16<<10)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version,sample_at,sample_ns,skip_source,skip_reason) VALUES(?,?,?,'skip_sample',?,1,?,1,?,?,'example-source','example-reason')`, fixture.project.OrganizationID, fixture.project.ID, at.Truncate(time.Hour).Unix(), "example-legacy-"+strconv.Itoa(i), string(raw), formatHubTime(at), at.UnixNano()); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertAnalyticsJSONEqual(t, want, read())
	if len(legacy) < 2 {
		t.Fatal("fixture has no mixed representation boundary")
	}
	var previousRows, previousBytes int
	for tick := range len(legacy) + 1 {
		if err := f.service.convertStorageBatch(ctx, now); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		var examined, examinedBytes int
		if err := f.service.database.reader.QueryRowContext(ctx, `SELECT examined_rows,examined_bytes FROM storage_conversion_cursors WHERE family='analytics'`).Scan(&examined, &examinedBytes); err != nil {
			t.Fatal(err)
		}
		if examined-previousRows > storageConversionRows || examinedBytes-previousBytes > storageConversionBytes {
			t.Fatalf("unbounded analytics tick: rows=%d bytes=%d", examined-previousRows, examinedBytes-previousBytes)
		}
		previousRows, previousBytes = examined, examinedBytes
		var pending int
		if err := f.service.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM analytics_hourly_rollups WHERE data_json<>'{}'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if tick == 0 {
			if pending == 0 || examined == 0 {
				t.Fatalf("fixture did not exercise partial conversion: pending=%d examined=%d", pending, examined)
			}
			assertAnalyticsJSONEqual(t, want, read())
		}
		if pending == 0 {
			break
		}
		if tick == len(legacy) {
			t.Fatalf("analytics conversion did not drain: %d pending", pending)
		}
	}
	assertAnalyticsJSONEqual(t, want, read())
	var bytes, count int
	if err := f.service.database.reader.QueryRowContext(ctx, `SELECT sum(length(data_json)),count(*) FROM analytics_hourly_rollups`).Scan(&bytes, &count); err != nil || bytes != 2*count {
		t.Fatalf("inline bytes=%d rows=%d err=%v", bytes, count, err)
	}
	migration, err := migrationFiles.ReadFile("migrations/20261103174341_analytics_sample_columns.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(migration), "-- +goose Down")
	if !found {
		t.Fatal("sample migration has no downgrade")
	}
	tx, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, downgrade := tx.ExecContext(ctx, down)
	rollback := tx.Rollback()
	if downgrade == nil || !strings.Contains(downgrade.Error(), "CHECK constraint failed") || rollback != nil {
		t.Fatalf("downgrade did not preserve typed samples: error=%v rollback=%v", downgrade, rollback)
	}
	assertAnalyticsJSONEqual(t, want, read())
	t.Logf("converted %d samples; inline bytes=%d across %d rollups; reports unchanged for %d windows in two projects", len(legacy), bytes, count, len(windows))
}

func BenchmarkAnalyticsSampleStorage(b *testing.B) {
	f := benchmarkAnalyticsFixture(b, 0)
	d := f.service.database
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	at := f.service.config.now()
	sample := analyticsResidenceSample{ID: "example-item", Created: at.Add(-time.Hour), At: at, State: "Rework", Entered: at, Started: &at, Events: 3, Transitions: 2, Lanes: map[string]float64{"Todo": 1800, "In Progress": 1800}, LanesNS: map[string]int64{"Todo": int64(30 * time.Minute), "In Progress": int64(30 * time.Minute)}, Causes: map[string]operatortool.AnalyticsReworkCause{"example-cause": {FromState: "In Progress", ReasonDetail: "example repair", Count: 1}}}
	tx, err := d.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	for _, dimension := range []string{"legacy", "typed"} {
		if err := writeAnalyticsResidenceSample(b.Context(), tx, scope, "residence_edge", dimension, sample); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	sample.AtNS = at.UnixNano()
	raw, err := json.Marshal(sample)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := d.db.ExecContext(b.Context(), `UPDATE analytics_hourly_rollups SET data_json=?,sample_storage_version=0 WHERE metric='residence_edge' AND dimension='legacy'`, string(raw)); err != nil {
		b.Fatal(err)
	}
	for _, dimension := range []string{"legacy", "typed"} {
		b.Run(dimension+"/read", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var document string
				if err := d.reader.QueryRowContext(b.Context(), `SELECT `+analyticsSampleColumn("")+` FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, scope.organization, scope.project, dimension).Scan(&document); err != nil {
					b.Fatal(err)
				}
				var sample analyticsResidenceSample
				if err := decodeAnalyticsSample(b.Context(), d.reader, document, &sample); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("typed/write", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			tx, err := d.db.BeginTx(b.Context(), nil)
			if err != nil {
				b.Fatal(err)
			}
			err = writeAnalyticsResidenceSample(b.Context(), tx, scope, "residence_edge", "typed", sample)
			rollback := tx.Rollback()
			if err != nil || rollback != nil {
				b.Fatalf("write sample: %v; rollback: %v", err, rollback)
			}
		}
	})
}
