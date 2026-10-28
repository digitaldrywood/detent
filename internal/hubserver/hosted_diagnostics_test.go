package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiagnosticsTodoHistory(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	states := map[string]tracker.NativeState{"Todo": {Name: "Todo", Dispatchable: true}, "In Progress": {Name: "In Progress", Dispatchable: true}}
	issue := analyticsResidenceIssue{created: at.Add(-time.Hour), initial: "Todo", events: []analyticsResidenceEvent{{at: at, kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}}}
	for _, test := range []struct {
		name        string
		at          time.Time
		initial     string
		partial     bool
		clipped     bool
		want        int
		unavailable bool
	}{
		{name: "not yet created", at: at.Add(-2 * time.Hour), initial: "Todo", want: 0},
		{name: "future dispatch cannot change past queue", at: at.Add(-time.Minute), initial: "Todo", want: 1},
		{name: "exclusive bucket end", at: at, initial: "Todo", want: 1},
		{name: "dispatch leaves queue", at: at.Add(time.Minute), initial: "Todo", want: 0},
		{name: "missing initial history", at: at, unavailable: true},
		{name: "incomplete history", at: at, initial: "Todo", partial: true, unavailable: true},
		{name: "clipped population", at: at, initial: "Todo", clipped: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := issue
			input.initial, input.partial = test.initial, test.partial
			got := diagnosticsTodoDepth([]analyticsResidenceIssue{input}, states, test.clipped, test.at)
			if test.unavailable {
				if got != nil {
					t.Fatalf("queue depth = %d, want unavailable", *got)
				}
				return
			}
			if got == nil || *got != test.want {
				t.Fatalf("queue depth = %v, want %d", got, test.want)
			}
		})
	}
}

func TestDiagnosticsProjectionReadBound(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := f.service.config.now().UTC().Truncate(time.Hour)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	const payload = `{"runtime":{"landing":{"landed":false,"refusal_kind":"conflict"}}}`
	for _, project := range []string{f.project, "prj_diagnostics_other"} {
		if project != f.project {
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,'Other project','native',states_json,created_at FROM projects WHERE id=?`, project, f.project); err != nil {
				t.Fatal(err)
			}
		}
		for _, age := range []int{2, 2400} {
			if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,started_at,updated_at,failed_at,failed_day,projection_version,status,work_item_id)
SELECT ?,?,?,'attempt',?||x,1,?,?,?,?,julianday(?),1,'failed','example-item' FROM n`, 4*maxAnalyticsPopulation, scope.organization, project, now.Add(-time.Duration(age)*time.Hour).Unix(), fmt.Sprintf("diagnostics-%d-", age), payload, formatHubTime(now.Add(-time.Duration(age)*time.Hour)), formatHubTime(now.Add(-time.Duration(age)*time.Hour)), formatHubTime(now.Add(-time.Duration(age)*time.Hour)), formatHubTime(now.Add(-time.Duration(age)*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name, query, index string
			args               []any
		}{
			{"attempts", diagnosticsAttemptsQuery, "analytics_attempt_reports_updated", []any{scope.organization, project, w.From.Add(-24 * time.Hour).Format(time.DateOnly), formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation + 1}},
			{"failures", diagnosticsFailuresQuery, "analytics_attempt_failed", []any{scope.organization, project, formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation + 1}},
			{"usage", diagnosticsUsageQuery, "analytics_usage_window", []any{scope.organization, project, w.From.Format(usagePeriodLayout), w.To.Format(usagePeriodLayout), maxAnalyticsPopulation + 1}},
		} {
			t.Run(project+"/"+test.name, func(t *testing.T) {
				plan := strings.Join(explainQueryPlan(t, f.service.database.reader, test.query, test.args), "\n")
				if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, test.index) {
					t.Fatalf("unindexed diagnostics read: %s", plan)
				}
				rows, err := f.service.database.reader.QueryContext(t.Context(), test.query, test.args...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				count := 0
				for rows.Next() {
					count++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if count > maxAnalyticsPopulation+1 || (test.name != "usage" && count != maxAnalyticsPopulation+1) {
					t.Fatalf("rows=%d, want at most %d and a full page for attempts/failures", count, maxAnalyticsPopulation+1)
				}
			})
		}
	}
	recorded := &diagnosticsRecordedReader{nativeQueryer: f.service.database.reader}
	project, err := readNativeDiagnostics(t.Context(), analyticsRollupReader{recorded}, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range recorded.queries {
		if strings.Contains(query, "sum(u.cost_estimate)") || strings.Contains(query, "sum(u.input+u.output)") || strings.Contains(query, "a.data_json") {
			t.Fatalf("diagnostics fetched full attempt details or failure usage: %s", query)
		}
	}
	if project.AttemptsObserved != maxAnalyticsPopulation || !project.Partial || len(project.FailureSignatures.Items) != 1 || project.FailureSignatures.Items[0].Count != maxAnalyticsPopulation {
		t.Fatalf("bounded diagnostics population: %+v", project)
	}
}

type diagnosticsRecordedReader struct {
	nativeQueryer
	queries []string
}

func (q *diagnosticsRecordedReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.queries = append(q.queries, statement)
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}
