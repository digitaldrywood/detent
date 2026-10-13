package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func rebuildTestAnalyticsRollups(t *testing.T, s *Service, scope nativeScope) {
	t.Helper()
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := rebuildAnalyticsRollups(t.Context(), tx, scope); err != nil {
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

func TestAnalyticsRollupFailurePreservesEvent(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "projection failure")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_analytics BEFORE INSERT ON analytics_hourly_rollups BEGIN SELECT RAISE(ABORT,'projection unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "ready"}}, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatalf("analytics failure blocked event: %v", err)
	}
	var events int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision'", scope.organization, scope.project, issue.WorkItemID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("event lost: count=%d err=%v", events, err)
	}
	if err := requireAnalyticsRollups(t.Context(), f.service.database.reader, scope); err == nil {
		t.Fatal("failed projection served complete analytics")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER reject_analytics"); err != nil {
		t.Fatal(err)
	}
	rebuildTestAnalyticsRollups(t, f.service, scope)
	if err := requireAnalyticsRollups(t.Context(), f.service.database.reader, scope); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsRollupMigrationBackfill(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	other := nativeScope{organization: scope.organization, project: tracker.ProjectID(f.privateProject)}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := createNativeIssueTx(t.Context(), tx, other, tracker.CreateIssue{Title: "second project history", State: "Todo"}, f.service.config.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	rebuildTestAnalyticsRollups(t, f.service, other)
	want := readTestRollupRows(t, f.service.database.reader, scope)
	wantOther := readTestRollupRows(t, f.service.database.reader, other)
	if len(want) == 0 || len(wantOther) == 0 {
		t.Fatal("backfill requires two populated projects")
	}
	migration, err := migrationFiles.ReadFile("migrations/20261013083000_analytics_rollups.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := strings.Split(string(migration), "-- +goose Down")[1]
	if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id=?", 20261013083000); err != nil {
		t.Fatal(err)
	}
	if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if got := readTestRollupRows(t, f.service.database.reader, scope); !reflect.DeepEqual(want, got) {
		t.Fatalf("migration changed rollups\nwant=%v\ngot=%v", want, got)
	}
	if got := readTestRollupRows(t, f.service.database.reader, other); !reflect.DeepEqual(wantOther, got) {
		t.Fatalf("migration lost second project: %v", got)
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
	w := operatortool.AnalyticsWindow{From: start, To: start.Add(time.Hour), Bucket: time.Hour}
	before, err := readNativeAnalytics(t.Context(), analyticsRollupReader{f.service.database.reader}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}
	if before.ProviderCapacityWait.ClaimsObserved != 1 || before.ProviderCapacityWait.IntervalsUnavailable != 1 {
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
	issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "completed history", State: "Todo"}, now.Add(-72*time.Hour))
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
				{analyticsRollupReader{analyticsNoRawEvents{f.service.database.reader, t}}, &rolled},
			} {
				project, err := readNativeAnalytics(t.Context(), input.q, scope, test.request, w)
				if err != nil {
					t.Fatal(err)
				}
				*input.out = nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}
			}
			assertAnalyticsJSONEqual(t, raw, rolled)
			wantDiagnostics, err := f.service.readHostedDiagnosticsFrom(t.Context(), raw, f.service.database.reader)
			if err != nil {
				t.Fatal(err)
			}
			gotDiagnostics, err := f.service.readHostedDiagnosticsFrom(t.Context(), rolled, analyticsRollupReader{analyticsNoRawEvents{f.service.database.reader, t}})
			if err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, wantDiagnostics, gotDiagnostics)
			wantReports, err := f.service.readHostedReportsFrom(t.Context(), raw, f.service.database.reader)
			if err != nil {
				t.Fatal(err)
			}
			gotReports, err := f.service.readHostedReportsFrom(t.Context(), rolled, analyticsRollupReader{analyticsNoRawEvents{f.service.database.reader, t}})
			if err != nil {
				t.Fatal(err)
			}
			assertAnalyticsJSONEqual(t, wantReports, gotReports)
		})
	}
}

type analyticsNoRawEvents struct {
	nativeQueryer
	t *testing.T
}

func (q analyticsNoRawEvents) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.assertProjected(statement)
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (q analyticsNoRawEvents) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	q.assertProjected(statement)
	return q.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func (q analyticsNoRawEvents) assertProjected(statement string) {
	q.t.Helper()
	if regexp.MustCompile(`\bcollaboration_events\b`).MatchString(statement) {
		q.t.Fatal("request queried raw collaboration events")
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
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("response differs\nwant: %s\ngot: %s", wantJSON, gotJSON)
	}
}
