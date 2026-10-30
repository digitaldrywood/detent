package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type backgroundCPUQuery struct {
	statement string
	args      []any
}

type backgroundCPUReader struct {
	nativeQueryer
	queries []backgroundCPUQuery
}

func (r *backgroundCPUReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	r.queries = append(r.queries, backgroundCPUQuery{statement, args})
	return r.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (r *backgroundCPUReader) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	r.queries = append(r.queries, backgroundCPUQuery{statement, args})
	return r.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func (r *backgroundCPUReader) plans(t *testing.T) string {
	t.Helper()
	var plans []string
	for _, query := range r.queries {
		rows, err := r.nativeQueryer.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.statement, query.args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plans = append(plans, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(plans, "\n")
}

func TestAnalyticsUsageDrainBoundsHistory(t *testing.T) {
	for _, history := range []int{2, 2000} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			now := f.service.config.now()
			for index, item := range items {
				attempt := fmt.Sprintf("usage-attempt-%d", index)
				for row := range history {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,analytics_attempt_id) VALUES(?,?,?,?,?,'run.started',1,'{}','{}',?,?)`, fmt.Sprintf("usage-event-%d-%d", index, row), scope.organization, scope.project, item.WorkItemID, row+100, formatHubTime(now), attempt); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,updated_at) VALUES(?,?,?,?, 'example','example',?)`, fmt.Sprintf("unrelated-%d-%d", index, row), scope.organization, scope.project, formatHubTime(now), formatHubTime(now)); err != nil {
						t.Fatal(err)
					}
				}
				for row := range 2*hubanalytics.DrainWrites + 1 {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,cost_estimate,updated_at) VALUES(?,?,?,?, 'example','example',1,?)`, attempt, scope.organization, scope.project, formatHubTime(now.Add(time.Duration(row)*time.Hour)), formatHubTime(now)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				position := hubanalytics.DrainPosition{Scope: hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, Phase: hubanalytics.DrainUsage}
				reader := &backgroundCPUReader{nativeQueryer: f.service.database.reader}
				writes := 0
				for tick := range 6 {
					reader.queries = nil
					plan := &hubanalytics.ProjectionPlan{Queryer: reader}
					if err := hubanalytics.PlanAnalyticsUsage(t.Context(), plan, string(item.WorkItemID), &position); err != nil {
						t.Fatal(err)
					}
					if len(plan.Writes) > hubanalytics.DrainWrites || len(reader.queries) > 2 {
						t.Fatalf("tick %d: writes=%d queries=%d", tick, len(plan.Writes), len(reader.queries))
					}
					plans := reader.plans(t)
					if tick < 2 {
						t.Logf("history=%d tick=%d source rows written=%d queries=%d plans:\n%s", history, tick, len(plan.Writes), len(reader.queries), plans)
					}
					if strings.Contains(plans, "TEMP B-TREE") || strings.Contains(plans, "SCAN u") {
						t.Fatalf("unbounded usage read: %s", plans)
					}
					if tick == 0 && (!strings.Contains(plans, "native_attempts_usage_drain") || !strings.Contains(plans, "collaboration_events_usage_drain")) {
						t.Fatalf("attempt selection missing seek indexes: %s", plans)
					}
					if len(plan.Writes) > 0 && !strings.Contains(plans, "attempt_usage_drain (attempt_id=? AND rowid>?") {
						t.Fatalf("usage page missing rowid seek: %s", plans)
					}
					for _, write := range plan.Writes {
						if _, err := f.service.database.db.ExecContext(t.Context(), write.Query, write.Args...); err != nil {
							t.Fatal(err)
						}
					}
					writes += len(plan.Writes)
					if position.Phase == hubanalytics.DrainLeases {
						break
					}
				}
				if position.Phase != hubanalytics.DrainLeases || writes != 65 {
					t.Fatalf("usage did not converge: phase=%d writes=%d", position.Phase, writes)
				}
			}
			var count int
			var cost float64
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*),sum(value) FROM analytics_hourly_rollups WHERE metric='usage'`).Scan(&count, &cost); err != nil || count != 130 || cost != 130 {
				t.Fatalf("usage totals count=%d cost=%g err=%v", count, cost, err)
			}
		})
	}
}

func TestHealthLandingReadBoundsHistory(t *testing.T) {
	for _, test := range []struct {
		name    string
		history int
		legacy  bool
	}{
		{"small converted", 2, false},
		{"large converted", 2000, false},
		{"small legacy", 2, true},
		{"legacy boundary", 49, true},
		{"legacy first overflow", 50, true},
		{"legacy overflow", 101, true},
		{"large legacy", 2000, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			now := f.service.config.now()
			window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now}
			items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for index, item := range items {
				for row := range test.history + 1 {
					id := fmt.Sprintf("change-%d-%05d", index, row)
					at := now.Add(-48 * time.Hour)
					if row == test.history {
						at = now.Add(-time.Minute)
					}
					landing := &tracker.ChangeLanding{VersionID: "version-" + id, HeadSHA: "head-" + id, MergeSHA: strings.Repeat("a", 40), LandedAt: at}
					raw, err := marshalNative(tracker.ChangeRequest{Landed: landing})
					if err != nil {
						t.Fatal(err)
					}
					var created, landed any = changeColumnTime(at), changeColumnTime(at)
					if test.legacy {
						created, landed = nil, nil
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,created_at,landed_at) VALUES(?,?,?,?,?,?,?)`, id, scope.organization, scope.project, item.WorkItemID, raw, created, landed); err != nil {
						t.Fatal(err)
					}
					version, err := marshalNative(tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: landing.HeadSHA}})
					if err != nil {
						t.Fatal(err)
					}
					var head any = landing.HeadSHA
					if test.legacy {
						head = nil
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,head_sha) VALUES(?,?,1,?,?)`, landing.VersionID, id, version, head); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			reader := &backgroundCPUReader{nativeQueryer: f.service.database.reader}
			landings, err := readHealthLandings(t.Context(), reader, scope, window)
			if test.legacy && 2*(test.history+1) > changeLegacyReadLimit {
				if !errors.Is(err, hubhealth.ErrReadLimit) || len(reader.queries) != 1 {
					t.Fatalf("legacy overflow must be explicit and bounded: landings=%d queries=%d err=%v", len(landings), len(reader.queries), err)
				}
			} else if err != nil || len(landings) != 2 || len(reader.queries) != 3 || landings[0].WorkItemID == landings[1].WorkItemID {
				t.Fatalf("landing observation: landings=%+v queries=%d err=%v", landings, len(reader.queries), err)
			}
			plans := reader.plans(t)
			var candidates int
			query := reader.queries[0]
			if err := reader.nativeQueryer.QueryRowContext(t.Context(), "SELECT count(*) FROM ("+query.statement+")", query.args...).Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if candidates > hubhealth.ReadLimit+changeLegacyReadLimit+2 {
				t.Fatalf("unbounded health candidate rows: %d", candidates)
			}
			t.Logf("history=%d legacy=%v candidate rows=%d queries=%d plans:\n%s", 2*test.history, test.legacy, candidates, len(reader.queries), plans)
			if !strings.Contains(plans, "change_requests_analytics_landed") || !strings.Contains(plans, "change_requests_policy_unknown") || strings.Contains(plans, "TEMP B-TREE") {
				t.Fatalf("health landing reads must seek bounded indexes: %s", plans)
			}
		})
	}
}
