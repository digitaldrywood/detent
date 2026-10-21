package hubserver

import (
	"database/sql"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCommitReadOnlyTransactionSkipsRefreshes(t *testing.T) {
	f := newUsageHostedFixture(t)
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO analytics_rollup_dirty SELECT native_id FROM issues WHERE native_id IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	dirty := func() int {
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_rollup_dirty").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := dirty()
	tx, err := f.service.database.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatalf("commit a read-only transaction: %v", err)
	}
	if before == 0 || dirty() != before {
		t.Fatalf("read-only commit changed the rollup work queue: %d dirty items, want %d", dirty(), before)
	}
}

func rebuildTestAnalyticsRollups(t testing.TB, s *Service, scope nativeScope) {
	t.Helper()
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := rebuildAnalyticsRollups(t.Context(), tx, scope, s.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsRollupIncremental(t *testing.T) {
	now := time.Date(2026, 10, 12, 13, 30, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	for _, fixture := range []nativeFixture{f, other} {
		issue := fixture.create(t, "rollup")
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "ready", Source: "native_claim", At: now}}, now); err != nil {
			t.Fatal(err)
		}
		for _, delta := range []time.Duration{5 * time.Nanosecond, 25 * time.Nanosecond} {
			at := now.Add(delta)
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Source: "native_claim", Reason: "capacity", At: at}}, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.service.database.commit(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		drainTestAnalytics(t, f.service)
		count, err := readAnalyticsCounter(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, "ready", now.Truncate(time.Hour), now.Add(time.Minute))
		if err != nil || count != 1 {
			t.Fatalf("incremental count=%d err=%v", count, err)
		}
		w := operatortool.AnalyticsWindow{From: now.Truncate(time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
		project, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 1000}, w)
		if err != nil {
			t.Fatal(err)
		}
		if project.DecisionsObserved != 2 || !project.SourceAt.Equal(now.Add(25*time.Nanosecond)) {
			t.Fatalf("skips lost exact source time: count=%d at=%s", project.DecisionsObserved, project.SourceAt)
		}
		for index, event := range []struct{ from, to string }{{"Todo", "In Progress"}, {"In Progress", "Rework"}, {"Rework", "Merging"}, {"Merging", "Done"}} {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			at := now.Add(time.Duration(index+1) * time.Hour)
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: event.from, ToState: event.to}, at); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.commit(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			drainTestAnalytics(t, f.service)
			w := operatortool.AnalyticsWindow{From: now.Truncate(time.Hour), To: at.Add(time.Minute), Bucket: time.Hour}
			var oracle, projected nativeAnalyticsProject
			oracle, err = readNativeAnalytics(t.Context(), f.service.database.reader, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
			if err != nil {
				t.Fatal(err)
			}
			projected, err = readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
			if err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, oracle, projected)
		}
		rows := readTestRollupRows(t, f.service.database.reader, scope)
		rebuildTestAnalyticsRollups(t, f.service, scope)
		if got := readTestRollupRows(t, f.service.database.reader, scope); !reflect.DeepEqual(rows, got) {
			t.Fatalf("rebuild differs from incremental: before=%v after=%v", rows, got)
		}
	}
	for _, project := range []tracker.NativeProject{f.project, other.project} {
		count, err := readAnalyticsCounter(t.Context(), analyticsRollupReader{f.service.database.reader}, nativeScope{organization: project.OrganizationID, project: project.ID}, "ready", now.Truncate(time.Hour), now.Add(time.Hour))
		if err != nil || count != 1 {
			t.Fatalf("project %s count=%d err=%v", project.ID, count, err)
		}
	}
}

func readTestRollupRows(t *testing.T, q nativeQueryer, scope nativeScope) []string {
	t.Helper()
	rows, err := q.QueryContext(t.Context(), `SELECT json_array(hour,metric,dimension,value,json(data_json)) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? ORDER BY metric,hour,dimension`, scope.organization, scope.project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result = append(result, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func analyticsRollupsServed(t *testing.T, s *Service, scope nativeScope) bool {
	t.Helper()
	q, err := analyticsProjectReader(t.Context(), analyticsRollupReader{s.database.reader}, scope)
	if err != nil {
		t.Fatal(err)
	}
	_, served := q.(analyticsRollupReader)
	return served
}

func commitTestAnalytics(t *testing.T, s *Service, write func(*sql.Tx)) {
	t.Helper()
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	write(tx)
	if err := s.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestAnalytics(t, s)
}

func TestAnalyticsRollupFailurePreservesEvent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recover func(*testing.T, nativeFixture, nativeScope, string)
	}{
		{"maintenance retries failed item", func(t *testing.T, f nativeFixture, _ nativeScope, _ string) {
			if err := f.service.database.retryFailedAnalyticsRollups(t.Context(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"item change retries failed item", func(t *testing.T, f nativeFixture, scope nativeScope, id string) {
			commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
				if err := appendNativeHistory(t.Context(), tx, scope, id, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "projection failure")
			healthy := f.create(t, "healthy projection")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_analytics BEFORE INSERT ON analytics_hourly_rollups WHEN json_extract(NEW.data_json,'$.work_item_id')='`+string(issue.WorkItemID)+`' BEGIN SELECT RAISE(ABORT,'projection unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
				for _, id := range []tracker.NativeWorkItemID{issue.WorkItemID, healthy.WorkItemID} {
					if err := appendNativeHistory(t.Context(), tx, scope, string(id), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "ready"}}, f.service.config.now()); err != nil {
						t.Fatal(err)
					}
				}
			})
			var events, projected int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='scheduler.decision'", scope.organization, scope.project).Scan(&events); err != nil || events != 2 {
				t.Fatalf("event lost: count=%d err=%v", events, err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='event' AND json_extract(data_json,'$.work_item_id')=? AND json_extract(data_json,'$.type')='scheduler.decision'", scope.organization, scope.project, healthy.WorkItemID).Scan(&projected); err != nil || projected != 1 {
				t.Fatalf("failure blocked another item's projection: count=%d err=%v", projected, err)
			}
			if analyticsRollupsServed(t, f.service, scope) {
				t.Fatal("failed projection served rollups instead of the raw computation")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER reject_analytics"); err != nil {
				t.Fatal(err)
			}
			commitTestAnalytics(t, f.service, func(*sql.Tx) {})
			if analyticsRollupsServed(t, f.service, scope) {
				t.Fatal("an unrelated commit retried the failed item")
			}
			tc.recover(t, f, scope, string(issue.WorkItemID))
			if !analyticsRollupsServed(t, f.service, scope) {
				t.Fatal("recovered projection stayed on the raw computation")
			}
			recovered := readTestRollupRows(t, f.service.database.reader, scope)
			rebuildTestAnalyticsRollups(t, f.service, scope)
			if got := readTestRollupRows(t, f.service.database.reader, scope); !reflect.DeepEqual(recovered, got) {
				t.Fatalf("recovered rollups differ from rebuild\nrecovered=%v\nrebuild=%v", recovered, got)
			}
		})
	}
}

func TestAnalyticsRollupMigrationBackfill(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	other := nativeScope{organization: scope.organization, project: tracker.ProjectID(f.privateProject)}
	commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
		for i := range analyticsBackfillBatch + 1 {
			if _, err := createNativeFixtureIssueTx(t.Context(), tx, other, tracker.CreateIssue{Body: nativeContractTestBody, Title: "second project history", State: "Todo"}, f.service.config.now().Add(-time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	})
	scopes := []nativeScope{scope, other}
	want := make([][]string, len(scopes))
	for i, project := range scopes {
		rebuildTestAnalyticsRollups(t, f.service, project)
		want[i] = readTestRollupRows(t, f.service.database.reader, project)
		if len(want[i]) == 0 {
			t.Fatalf("project %s has no rollups to backfill", project.project)
		}
	}
	for _, version := range []struct {
		id   int64
		name string
	}{{20261014233000, "analytics_attempt_projection"}, {20261014200000, "drop_redundant_attempt_indexes"}, {20261014160000, "analytics_attempt_payload"}, {20261014120000, "analytics_rollup_report_columns"}, {20261013083000, "analytics_rollups"}} {
		migration, err := migrationFiles.ReadFile("migrations/" + strconv.FormatInt(version.id, 10) + "_" + version.name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		down := strings.Split(string(migration), "-- +goose Down")[1]
		if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id=?", version.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	for _, project := range scopes {
		if got := readTestRollupRows(t, f.service.database.reader, project); len(got) != 0 {
			t.Fatalf("migration copied history: %v", got)
		}
		if analyticsRollupsServed(t, f.service, project) {
			t.Fatal("reads used rollups before the backfill completed")
		}
	}
	batches := 0
	for {
		more, err := f.service.database.backfillAnalyticsRollups(t.Context(), f.service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		batches++
	}
	if batches < 3 {
		t.Fatalf("backfill ran in %d batches; want bounded batches per project", batches)
	}
	for i, project := range scopes {
		if !analyticsRollupsServed(t, f.service, project) {
			t.Fatalf("project %s stayed on the raw computation after backfill", project.project)
		}
		if got := readTestRollupRows(t, f.service.database.reader, project); !reflect.DeepEqual(want[i], got) {
			t.Fatalf("backfill differs from rebuild\nwant=%v\ngot=%v", want[i], got)
		}
	}
}

func TestAnalyticsRollupUsageWrites(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"update", "UPDATE attempt_usage SET cost_estimate=cost_estimate+1.5 WHERE attempt_id='attempt_browser_usage_0_0'"},
		{"insert", "INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,cost_estimate,currency,updated_at) SELECT attempt_id,organization_id,project_id,period,provider,'second-model',2.25,currency,updated_at FROM attempt_usage WHERE attempt_id='attempt_browser_usage_0_0' LIMIT 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(t.Context(), test.statement); err != nil {
					t.Fatal(err)
				}
			})
			var raw, projected float64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sum(cost_estimate) FROM attempt_usage WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sum(cost_estimate) FROM analytics_attempt_usage WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&projected); err != nil {
				t.Fatal(err)
			}
			if raw != projected {
				t.Fatalf("projected spend=%v raw=%v", projected, raw)
			}
		})
	}
}

func drainTestAnalytics(t *testing.T, s *Service) {
	t.Helper()
	for {
		more, err := s.database.drainAnalyticsDirty(t.Context(), s.config.now())
		if err != nil {
			t.Log(err)
		}
		if !more {
			return
		}
	}
}

func projectTestAnalytics(t *testing.T, s *Service, tx *sql.Tx) {
	t.Helper()
	for {
		if err := s.database.projectAnalyticsBatch(t.Context(), tx, s.config.now(), analyticsBackfillBatch); err != nil {
			t.Fatal(err)
		}
		var more bool
		if err := tx.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM analytics_rollup_dirty)").Scan(&more); err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

func countTestAnalyticsDirty(t *testing.T, s *Service) int {
	t.Helper()
	var count int
	if err := s.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_rollup_dirty").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAnalyticsRollupDeferredWork(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range analyticsBackfillBatch + 5 {
		if _, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "bulk", Body: nativeContractTestBody, State: "Todo"}, f.service.config.now().Add(-time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if got := countTestAnalyticsDirty(t, f.service); got != analyticsBackfillBatch+5 {
		t.Fatalf("deferred items=%d want all %d left to maintenance", got, analyticsBackfillBatch+5)
	}
	drainTestAnalytics(t, f.service)
	commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
		if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET require_dependencies=NOT require_dependencies WHERE organization_id=? AND id=?", scope.organization, scope.project); err != nil {
			t.Fatal(err)
		}
	})
	if got := countTestAnalyticsDirty(t, f.service); got != 0 {
		t.Fatalf("project policy change left %d analytics items; want only the deferred items drained", got)
	}
	commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
		if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET require_dependencies=NOT require_dependencies WHERE organization_id=? AND id=?", scope.organization, scope.project); err != nil {
			t.Fatal(err)
		}
		var marked int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_rollup_dirty").Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked != 0 {
			t.Fatalf("project policy change marked %d analytics items", marked)
		}
	})
	more, err := f.service.database.drainAnalyticsDirty(t.Context(), f.service.config.now())
	if err != nil || more {
		t.Fatalf("drain more=%t err=%v", more, err)
	}
	projected := readTestRollupRows(t, f.service.database.reader, scope)
	rebuildTestAnalyticsRollups(t, f.service, scope)
	if got := readTestRollupRows(t, f.service.database.reader, scope); !reflect.DeepEqual(projected, got) {
		t.Fatalf("deferred projection differs from rebuild\nprojected=%v\nrebuild=%v", projected, got)
	}
}

func TestAnalyticsBackfillItemsBoundedByEvents(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence int64
		want     int
	}{
		{"small items fill the batch", 1, 3},
		{"large items stop at the event budget", analyticsBackfillEvents/2 + 1, 1},
		{"one oversized item still progresses", analyticsBackfillEvents * 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			for i := range 3 {
				f.create(t, "backfill-"+strconv.Itoa(i))
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range []string{"DELETE FROM analytics_rollup_positions WHERE organization_id=? AND project_id=?", "UPDATE issues SET event_sequence=" + strconv.FormatInt(test.sequence, 10) + " WHERE organization_id=? AND project_id=?"} {
				if _, err := tx.ExecContext(t.Context(), statement, scope.organization, scope.project); err != nil {
					t.Fatal(err)
				}
			}
			ids, err := analyticsBackfillItems(t.Context(), tx, string(scope.organization), string(scope.project))
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != test.want {
				t.Fatalf("batch=%d want %d", len(ids), test.want)
			}
		})
	}
}

func TestAnalyticsRetryRotatesFailedItems(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for i := range analyticsBackfillBatch + 1 {
		f.create(t, "retry-"+strconv.Itoa(i))
	}
	drainTestAnalytics(t, f.service)
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_analytics BEFORE INSERT ON analytics_hourly_rollups BEGIN SELECT RAISE(ABORT,'projection unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE analytics_rollup_positions SET failed=1,sequence=0 WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.retryFailedAnalyticsRollups(t.Context(), f.service.config.now()); err == nil {
		t.Fatal("retry succeeded while projection was rejected")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER reject_analytics"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.retryFailedAnalyticsRollups(t.Context(), f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	var failed int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND failed=1", scope.organization, scope.project).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != analyticsBackfillBatch {
		t.Fatalf("failed=%d after the second retry; want the items past the first batch retried first", failed)
	}
}

func TestAnalyticsRollupCompactionPreservesPredecessor(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "capacity adjacency")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	start := f.service.config.now().UTC()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, kind := range []string{"scheduler.decision", "run.observed", "scheduler.decision"} {
		decision := &tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", RunnerID: "runner", WorkItemRevision: 1, At: start}
		if i == 2 {
			decision.Source = "native_claim"
			decision.Outcome = "claimed"
			decision.At = start.Add(time.Minute)
		}
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), kind, tracker.CollaborationData{Decision: decision}, start.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestAnalytics(t, f.service)
	w := operatortool.AnalyticsWindow{From: start.Truncate(time.Hour), To: start.Truncate(time.Hour).Add(2 * time.Hour), Bucket: time.Hour}
	before, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}
	if before.ProviderCapacityWait.ClaimsObserved != 1 || before.ProviderCapacityWait.IntervalsUnavailable != 1 || before.DecisionsObserved != 1 {
		t.Fatalf("predecessor did not interrupt wait: %+v", before.ProviderCapacityWait)
	}
	tx, err = f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "DROP TRIGGER collaboration_events_no_delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "DELETE FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='run.observed'", scope.organization, scope.project, issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events BEGIN SELECT RAISE(ABORT,'collaboration events are immutable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	after, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}
	assertAnalyticsJSONEqual(t, before, after)
	countSamples := func() int {
		t.Helper()
		var samples int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_hourly_rollups WHERE metric='skip_sample'").Scan(&samples); err != nil {
			t.Fatal(err)
		}
		return samples
	}
	for _, pass := range []struct {
		name    string
		now     time.Time
		samples int
	}{
		{"inside the sample window", start.Add(analyticsSampleRetention - time.Hour), 1},
		{"past the sample window", start.Add(analyticsSampleRetention + 2*time.Hour), 0},
	} {
		if err := f.service.database.compactAnalyticsSamples(t.Context(), pass.now); err != nil {
			t.Fatal(err)
		}
		if samples := countSamples(); samples != pass.samples {
			t.Fatalf("%s: skip samples=%d want %d", pass.name, samples, pass.samples)
		}
		compacted, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
		if err != nil {
			t.Fatal(err)
		}
		assertAnalyticsJSONEqual(t, before, compacted)
	}
	edge := operatortool.AnalyticsWindow{From: start.Add(-time.Minute), To: start.Add(time.Hour), Bucket: time.Hour}
	partial, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, edge)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Partial {
		t.Fatal("a window edge inside compacted samples was reported as complete")
	}
}

func TestAnalyticsRollupResponseEquivalence(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	now := f.service.config.now().UTC().Truncate(time.Hour)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Body: nativeContractTestBody, Title: "completed history", State: "Todo"}, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		kind string
		at   time.Time
		data tracker.CollaborationData
	}{
		{"scheduler.decision", now.Add(-27 * time.Hour), tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "capacity", RunnerID: "runner", WorkItemRevision: 1, At: now.Add(-27 * time.Hour)}}},
		{"scheduler.decision", now.Add(-26 * time.Hour), tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", RunnerID: "runner", WorkItemRevision: 1, At: now.Add(-26 * time.Hour)}}},
		{"workflow.transitioned", now.Add(-25 * time.Hour), tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}},
		{"workflow.transitioned", now.Add(-23 * time.Hour), tracker.CollaborationData{FromState: "In Progress", ToState: "Merging"}},
		{"workflow.transitioned", now.Add(-22 * time.Hour), tracker.CollaborationData{FromState: "Merging", ToState: "Done"}},
		{"scheduler.decision", now.Add(-time.Minute), tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready", At: now.Add(-time.Minute)}}},
	} {
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), event.kind, event.data, event.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT 'rollup-open-lease',id,'machine_browser_preview_runner','rollup-open-session',?,?,?,NULL,?,? FROM issues WHERE native_id=?`, formatHubTime(now.Add(30*time.Minute)), formatHubTime(now.Add(-90*time.Minute)), formatHubTime(now.Add(-time.Minute)), formatHubTime(now.Add(-90*time.Minute)), formatHubTime(now.Add(-time.Minute)), issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		changeID := "rollup-change-" + string(rune('a'+i))
		versionID := changeID + "-version"
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,json_object('landed',json_object('version_id',?,'head_sha',?,'merge_sha',?,'landed_at',?)))`, changeID, scope.organization, scope.project, issue.WorkItemID, versionID, strings.Repeat("a", 40), strings.Repeat("b", 40), formatHubTime(now.Add(-22*time.Hour+time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,json_object('head_sha',?))`, versionID, changeID, strings.Repeat("a", 40)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range maxAnalyticsPopulation + 1 {
		at := now.Add(-10*time.Minute + time.Duration(i)*time.Nanosecond)
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "capacity", At: at}}, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestAnalytics(t, f.service)
	for _, test := range []struct {
		name     string
		from, to time.Time
		bucket   time.Duration
		request  operatortool.AnalyticsRequest
	}{
		{"hourly", now.Add(-24 * time.Hour), now, time.Hour, operatortool.AnalyticsRequest{Limit: 1000}},
		{"week", now.Add(-7 * 24 * time.Hour), now, time.Hour, operatortool.AnalyticsRequest{Limit: 1}},
		{"month", now.Add(-30 * 24 * time.Hour), now, 4 * time.Hour, operatortool.AnalyticsRequest{Limit: 1000}},
		{"partial boundaries", now.Add(-24*time.Hour + 15*time.Minute), now.Add(25 * time.Minute), time.Hour, operatortool.AnalyticsRequest{Limit: 1}},
		{"empty", now.Add(24 * time.Hour), now.Add(48 * time.Hour), time.Hour, operatortool.AnalyticsRequest{Limit: 1}},
		{"clipped decision cursor", now.Add(-24 * time.Hour), now, time.Hour, operatortool.AnalyticsRequest{Limit: 1, PopulationCursor: "decisions:1000:0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := operatortool.AnalyticsWindow{From: test.from, To: test.to, Bucket: test.bucket}
			var raw, rolled nativeAnalyticsReport
			for _, input := range []struct {
				q   nativeQueryer
				out *nativeAnalyticsReport
			}{
				{f.service.database.reader, &raw},
				{analyticsRollupReader{f.service.database.reader}, &rolled},
			} {
				project, err := readNativeAnalytics(t.Context(), input.q, scope, test.request, w)
				if err != nil {
					t.Fatal(err)
				}
				*input.out = nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}
			}
			assertAnalyticsJSONEqual(t, raw, rolled)
			if test.request.PopulationCursor == "" {
				for _, q := range []nativeQueryer{f.service.database.reader, analyticsRollupReader{f.service.database.reader}} {
					shared := withReportsQualityInputs(t.Context(), w)
					for _, window := range []operatortool.AnalyticsWindow{w, {From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}} {
						want, err := readAnalyticsQuality(t.Context(), q, scope, test.request, window)
						if err != nil {
							t.Fatal(err)
						}
						got, err := readAnalyticsQuality(shared, q, scope, test.request, window)
						if err != nil {
							t.Fatal(err)
						}
						assertAnalyticsJSONEqual(t, want, got)
					}
				}
			}

			wantDiagnostics, err := f.service.readHostedDiagnosticsFrom(t.Context(), raw, f.service.database.reader)
			if err != nil {
				t.Fatal(err)
			}
			wantReports, err := f.service.readHostedReportsFrom(t.Context(), raw, f.service.database.reader)
			if err != nil {
				t.Fatal(err)
			}
			projectedTx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer projectedTx.Rollback()
			if _, err := projectedTx.ExecContext(t.Context(), `DELETE FROM analytics_hourly_rollups WHERE metric='event' AND NOT (json_extract(data_json,'$.type')='scheduler.decision' AND coalesce(json_extract(data_json,'$.outcome'),'')='ready')`); err != nil {
				t.Fatal(err)
			}
			q := analyticsRollupReader{projectedTx}
			gotDiagnostics, err := f.service.readHostedDiagnosticsFrom(t.Context(), rolled, q)
			if err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, wantDiagnostics, gotDiagnostics)
			gotReports, err := f.service.readHostedReportsFrom(t.Context(), rolled, q)
			if err != nil {
				t.Fatal(err)
			}
			residence := nativeAnalyticsProject{Window: w, Digest: append([]nativeAnalyticsBucket{}, rolled.Projects[0].Digest...), allAttempts: rolled.Projects[0].allAttempts}
			if err := projectNativeAnalyticsResidence(t.Context(), q, scope, test.request, &residence); err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, raw.Projects[0].LaneResidence, residence.LaneResidence)
			assertAnalyticsJSONEqual(t, raw.Projects[0].QueueTime, residence.QueueTime)
			capacity := nativeAnalyticsProject{Window: w, Digest: append([]nativeAnalyticsBucket{}, rolled.Projects[0].Digest...)}
			if err := projectNativeAnalyticsCapacityWait(t.Context(), q, scope, &capacity); err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, raw.Projects[0].ProviderCapacityWait, capacity.ProviderCapacityWait)
			for i := range capacity.Digest {
				assertAnalyticsJSONEqual(t, raw.Projects[0].Digest[i].ProviderCapacityWaitSeconds, capacity.Digest[i].ProviderCapacityWaitSeconds)
			}

			assertAnalyticsJSONEqual(t, wantReports, gotReports)
		})
	}
}

func assertAnalyticsJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wantValue, gotValue any
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatal(err)
	}
	if !analyticsJSONClose(wantValue, gotValue) {
		t.Fatalf("response differs\nwant: %s\ngot: %s", wantJSON, gotJSON)
	}
}

func analyticsJSONClose(want, got any) bool {
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		return ok && math.Abs(w-g) <= 1e-9*math.Max(1, math.Max(math.Abs(w), math.Abs(g)))
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for i := range w {
			if !analyticsJSONClose(w[i], g[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for k, v := range w {
			gv, ok := g[k]
			if !ok || !analyticsJSONClose(v, gv) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}
