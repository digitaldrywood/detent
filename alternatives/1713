package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsLongRangeReadBudget(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	f.service.config.now = func() time.Time { return now }
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	seedProductionProject(t, f.service, scope, productionProjectShape{issues: 990, attempts: 990, usage: 660, leases: 1682, comments: 7000, days: 180})
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH n(k) AS (VALUES(1),(2),(3),(4),(5),(6))
INSERT INTO change_evidence(change_id,sequence,kind,version_id,record_json)
SELECT c.id,(SELECT coalesce(max(sequence),0) FROM change_evidence)+row_number() OVER (ORDER BY c.id,k),CASE WHEN k%2=1 THEN 'review' ELSE 'discussion' END,v.id,
CASE WHEN k%2=1 THEN json_object('review_id',c.id||k,'version_id',v.id,'decision','changes_requested','created_at',json_extract(c.record_json,'$.landed.landed_at'),'body',hex(zeroblob(2048)))
ELSE json_object('discussion_id',c.id||k,'version_id',v.id,'created_at',json_extract(c.record_json,'$.landed.landed_at'),'body',hex(zeroblob(2048)),
'escape',json_object('occurrence_id',c.id||k,'kind','reopened','cause','missing_criterion','observed_at',json_extract(c.record_json,'$.landed.landed_at'))) END
FROM change_requests c JOIN change_versions v ON v.change_id=c.id CROSS JOIN n WHERE c.organization_id=? AND c.project_id=? AND c.id LIKE 'change_wi_shaped_%'`, scope.organization, scope.project); err != nil {
		t.Fatal(err)
	}
	projectTestChangeColumns(t, f.service)
	for _, test := range []struct {
		name      string
		rangeName string
		hour      int
		days      int
	}{
		{"30d/midnight", "30d", 0, 30},
		{"30d/daytime", "30d", 10, 29},
		{"90d/midnight", "90d", 0, 90},
		{"90d/daytime", "90d", 10, 89},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.now = func() time.Time { return now.Add(time.Duration(test.hour) * time.Hour) }
			var mu sync.Mutex
			var statements []tracedStatement
			timing := &requestTiming{trace: func(query string, args []driver.NamedValue) {
				values := make([]any, 0, len(args))
				for _, arg := range args {
					if arg.Name != "" {
						values = append(values, sql.Named(arg.Name, arg.Value))
					} else {
						values = append(values, arg.Value)
					}
				}
				mu.Lock()
				statements = append(statements, tracedStatement{query, values})
				mu.Unlock()
			}}
			ctx, cancel := context.WithTimeout(withRequestTiming(t.Context(), timing), hubReadTimeout)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+test.rangeName, nil).WithContext(ctx)
			request.AddCookie(f.cookies["owner"])
			response := httptest.NewRecorder()
			started := time.Now()
			f.service.Handler().ServeHTTP(response, request)
			elapsed := time.Since(started)
			if response.Code != http.StatusOK || elapsed >= hubReadTimeout {
				t.Fatalf("report took %s: HTTP %d: %s", elapsed, response.Code, response.Body.String())
			}
			var report hostedReportsReport
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			now := f.service.config.now().UTC()
			wantFrom := now.Truncate(24*time.Hour).AddDate(0, 0, -test.days)
			if !report.Analytics.Window.From.Equal(wantFrom) || !report.Analytics.Window.To.Equal(now) || report.Completion.Done < test.days {
				t.Fatalf("window=%+v shipped=%d, want %s through %s and at least %d shipped", report.Analytics.Window, report.Completion.Done, wantFrom, now, test.days)
			}
			t.Logf("%s report: %s, %d shipped", test.rangeName, elapsed, report.Completion.Done)
			mu.Lock()
			captured := append([]tracedStatement(nil), statements...)
			mu.Unlock()
			for _, target := range []struct {
				name, fragment, index string
			}{
				{"quality reopened items", "WITH reopened_items", "analytics_event_order"},
				{"quality events", "coalesce((SELECT terminal FROM workflow_states", "analytics_event_order"},
				{"quality evidence", "SELECT e.change_id,e.kind,COALESCE(NULLIF(e.payload_object_key", "change_evidence_analytics_"},
				{"prior attempts", "SELECT count(*) FROM analytics_native_attempts a", "analytics_attempt_"},
			} {
				found := false
				for _, statement := range captured {
					if !strings.Contains(statement.query, target.fragment) {
						continue
					}
					found = true
					plan := explainQueryPlan(t, f.service.database.reader, statement.query, statement.args)
					ranges := 0
					for _, detail := range plan {
						if strings.HasPrefix(detail, "SEARCH ") && strings.Contains(detail, target.index) && strings.Contains(detail, ">?") && strings.Contains(detail, "<?") {
							ranges++
						}
					}
					want := 1
					if target.name == "quality evidence" {
						want = 2
					}
					if ranges < want {
						t.Errorf("%s needs %d index range scans: %v", target.name, want, plan)
					}
				}
				if !found {
					t.Errorf("%s query was not exercised", target.name)
				}
			}
		})
	}
}
