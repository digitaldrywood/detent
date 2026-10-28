package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
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
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	for _, project := range []string{f.project, f.privateProject} {
		scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(project)}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		q := diagnosticsTestQuery(t, tx, scope)
		for _, age := range []int{2, 2400} {
			at := formatHubTime(now.Add(-time.Duration(age) * time.Hour))
			if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
   INSERT INTO diagnostics_attempts(id,organization_id,project_id,work_item_id,status,started_at,updated_at,data_json,report_failed_at)
   SELECT ?||x,?,?,'example-item','failed',?,?,?,? FROM n`, 4*maxAnalyticsPopulation, fmt.Sprintf("diagnostics-%d-", age), scope.organization, scope.project, at, at, `{"runtime":{"landing":{"landed":false,"refusal_kind":"conflict"}}}`, at); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name, query, index string
			args               []any
		}{
			{"attempts", diagnosticsAttemptsQuery, "native_attempts_reports_updated", []any{scope.organization, scope.project, w.From.Add(-24 * time.Hour).Format(time.DateOnly), maxAnalyticsPopulation + 1}},
			{"failures", diagnosticsFailuresQuery, "native_attempts_report_failures", []any{scope.organization, scope.project, changeColumnTime(w.From), changeColumnTime(w.To), maxAnalyticsPopulation + 1}},
			{"usage", diagnosticsUsageQuery, "attempt_usage_analytics_window", []any{scope.organization, scope.project, w.From.Format(usagePeriodLayout), w.To.Format(usagePeriodLayout), maxAnalyticsPopulation + 1}},
		} {
			t.Run(project+"/"+test.name, func(t *testing.T) {
				source := strings.ReplaceAll(test.query, "report_attempts", "native_attempts")
				plan := strings.Join(explainQueryPlan(t, f.service.database.reader, source, test.args), "\n")
				if !strings.Contains(plan, test.index) || strings.Contains(plan, "USE TEMP B-TREE") {
					t.Fatalf("unindexed source diagnostics read: %s", plan)
				}
				rows, err := q.QueryContext(t.Context(), test.query, test.args...)
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
				if count > maxAnalyticsPopulation+1 || test.name != "usage" && count != maxAnalyticsPopulation+1 {
					t.Fatalf("unbounded diagnostics rows=%d", count)
				}
			})
		}
		recorded := &diagnosticsRecordedReader{nativeQueryer: q.nativeQueryer}
		q.nativeQueryer = recorded
		out, err := readNativeDiagnostics(t.Context(), q, scope, operatortool.AnalyticsRequest{Limit: 100}, w)
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range recorded.queries {
			if strings.Contains(query, "a.data_json") {
				t.Fatalf("diagnostics fetched full attempt population: %s", query)
			}
		}
		if out.AttemptsObserved != maxAnalyticsPopulation || !out.Partial || len(out.FailureSignatures.Items) != 1 || out.FailureSignatures.Items[0].Count != maxAnalyticsPopulation {
			t.Fatalf("bounded diagnostics population: %+v", out)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
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

func TestDiagnosticsAttemptCandidatesBound(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := f.service.config.now().UTC().Truncate(time.Hour)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	for _, population := range []int{4000, 40000} {
		for _, test := range []struct {
			name             string
			started, updated time.Time
			observed         int
		}{
			{"matching", now.Add(-2 * time.Hour), now.Add(-time.Hour), maxAnalyticsPopulation},
			{"before exact window", w.From.Add(-time.Hour), w.From.Add(-time.Minute), 0},
			{"started at exclusive end", w.To, w.To.Add(time.Hour), 0},
			{"overlapping old start", w.From.Add(-365 * 24 * time.Hour), w.From, maxAnalyticsPopulation},
		} {
			t.Run(fmt.Sprintf("%d/%s", population, test.name), func(t *testing.T) {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				q := diagnosticsTestQuery(t, tx, scope)
				if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
    INSERT INTO diagnostics_attempts(id,organization_id,project_id,started_at,updated_at,data_json)
    SELECT 'candidate-'||x,?,?,?,?,'{"runtime":{"phases":[{"name":"implement"}]}}' FROM n`, population, scope.organization, scope.project, formatHubTime(test.started), formatHubTime(test.updated)); err != nil {
					t.Fatal(err)
				}
				out := nativeAnalyticsProject{Window: w}
				if err := readDiagnosticsAttempts(t.Context(), q, scope, &out); err != nil {
					t.Fatal(err)
				}
				if out.AttemptsObserved != test.observed || !out.Partial {
					t.Fatalf("observed=%d partial=%t, want observed=%d partial=true for capped candidates", out.AttemptsObserved, out.Partial, test.observed)
				}
			})
		}
	}
}

func TestDiagnosticsRefusalPageByteParity(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := f.service.config.now().UTC().Truncate(time.Hour)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	signatures := make([]nativeFailureSignature, 0, 51)
	for i := range 50 {
		example := fmt.Sprintf("failure %c%c ", 'g'+i/20, 'g'+i%20) + strings.Repeat("z", 501)
		signatures = append(signatures, nativeFailureSignature{Signature: example[:256], Example: example[:512], Class: "attempt", Count: 1, WorkItems: []string{fmt.Sprintf("item-%d", i)}, WorkItemsCount: 1, FirstSeen: now.Add(-time.Hour), LastSeen: now.Add(-time.Hour), Cost: 123456789.125, Tokens: 999999999999, SlotMinutes: 60})
	}
	signatures = append(signatures, nativeFailureSignature{Signature: "landing refused: conflict", Example: "landing refused: conflict", Class: "merge", Count: 1, WorkItems: []string{"conflict-item"}, WorkItemsCount: 1, FirstSeen: now.Add(-time.Hour), LastSeen: now.Add(-time.Hour), Cost: 123456789.125, Tokens: 999999999999, SlotMinutes: 60})
	size := 2
	for _, signature := range signatures {
		raw, err := json.Marshal(signature)
		if err != nil {
			t.Fatal(err)
		}
		size += len(raw) + 1
	}
	padding := operatortool.WorkListPageBytes + 1 - size
	if padding < 0 {
		t.Fatalf("fixture already exceeds byte limit: %d", size)
	}
	for i := range 50 {
		signatures[i].WorkItems[0] += strings.Repeat("z", padding/50)
		if i < padding%50 {
			signatures[i].WorkItems[0] += "z"
		}
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := diagnosticsTestQuery(t, tx, scope)
	for i, signature := range signatures {
		attempt := fmt.Sprintf("boundary-%d", i)
		payload := tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: signature.Example}}
		if signature.Class == "merge" {
			payload = tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict"}}}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO diagnostics_attempts(id,organization_id,project_id,work_item_id,status,started_at,updated_at,data_json,report_failed_at) VALUES(?,?,?,?,'failed',?,?,?,?)`, attempt, scope.organization, scope.project, signature.WorkItems[0], formatHubTime(now.Add(-2*time.Hour)), formatHubTime(signature.LastSeen), string(raw), changeColumnTime(signature.LastSeen)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO temp.attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,cached_input,output,cost_estimate,currency) VALUES(?,?,?,?,'provider','model',?,0,0,?,'USD')`, attempt, scope.organization, scope.project, formatHubTime(signature.LastSeen), signature.Tokens, signature.Cost); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name                 string
		offset, limit, count int
		trim                 int
	}{
		{"byte clipped before refusal", 0, 100, 50, 0},
		{"exact byte limit includes refusal", 0, 100, 51, 1},
		{"below byte limit includes refusal", 0, 100, 51, 2},
		{"row clipped before refusal", 0, 50, 50, 0},
		{"refusal on next page", 50, 100, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected := append([]nativeFailureSignature(nil), signatures...)
			firstItem := signatures[0].WorkItems[0]
			expected[0].WorkItems = []string{firstItem[:len(firstItem)-test.trim]}
			if _, err := tx.ExecContext(t.Context(), `UPDATE diagnostics_attempts SET work_item_id=? WHERE organization_id=? AND project_id=? AND id='boundary-0'`, expected[0].WorkItems[0], scope.organization, scope.project); err != nil {
				t.Fatal(err)
			}
			qPrepared, err := prepareReportsQuery(t.Context(), q, scope, "report_attempts")
			if err != nil {
				t.Fatal(err)
			}
			out := nativeAnalyticsProject{Window: w}
			if err := readDiagnosticsRefusals(t.Context(), qPrepared, scope, operatortool.AnalyticsRequest{RowOffset: test.offset, Limit: test.limit}, &out); err != nil {
				t.Fatal(err)
			}
			want := nativeFailureSignaturePage(expected, test.offset, test.limit)
			if len(want.Items) != test.count {
				t.Fatalf("fixture page count=%d, want %d", len(want.Items), test.count)
			}
			if len(out.FailureSignatures.Items) != test.count {
				t.Fatalf("page contains %d signatures, want %d", len(out.FailureSignatures.Items), test.count)
			}
			if !reflect.DeepEqual(want, out.FailureSignatures) {
				t.Fatal("failure page differs from existing pagination")
			}
			out.ProjectID = string(scope.project)
			baseline := out
			baseline.FailureSignatures = want
			reports := make([]diagnosticsReport, 0, 2)
			for _, project := range []nativeAnalyticsProject{baseline, out} {
				report, err := f.service.readHostedDiagnosticsFrom(t.Context(), nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}, qPrepared)
				if err != nil {
					t.Fatal(err)
				}
				reports = append(reports, report)
			}
			assertAnalyticsJSONEqual(t, reports[0], reports[1])
			if out.Partial {
				t.Fatal("byte or row pagination changed population partial")
			}
		})
	}
}

func diagnosticsTestQuery(t *testing.T, tx *sql.Tx, scope nativeScope) *nativePageQuery {
	t.Helper()
	for _, statement := range []string{
		`CREATE TEMP TABLE diagnostics_attempts(id TEXT PRIMARY KEY,organization_id TEXT,project_id TEXT,work_item_id TEXT,lease_id TEXT,status TEXT,started_at TEXT,updated_at TEXT,data_json TEXT,runner_id TEXT,role TEXT,execution_role TEXT,report_requested_model TEXT,report_resolved_model TEXT,report_failed_at TEXT,has_landing_receipt INTEGER)`,
		`CREATE TEMP TABLE attempt_usage(attempt_id TEXT,organization_id TEXT,project_id TEXT,period TEXT,provider TEXT,model TEXT,input INTEGER,cached_input INTEGER,output INTEGER,cost_estimate REAL,currency TEXT)`,
	} {
		if _, err := tx.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	columns := "id,organization_id,project_id,work_item_id,lease_id,status,started_at,updated_at,data_json,runner_id,role,execution_role,report_requested_model,report_resolved_model,report_failed_at,has_landing_receipt"
	return &nativePageQuery{nativeQueryer: tx, organization: scope.organization, project: scope.project, facts: map[string]nativePageFact{"report_attempts": {name: "report_attempts", columns: columns, known: "SELECT " + columns + " FROM temp.diagnostics_attempts", override: true}}}
}
