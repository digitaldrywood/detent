package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var hotReadTables = []string{"native_attempts", "collaboration_events", "change_requests", "attempt_usage"}

// knownSlowHotReads lists hot read statements that currently fail the plan
// rules. Each entry names the issue that fixes it and must still fail;
// remove the entry when its fix lands.
var knownSlowHotReads = []struct {
	name     string
	fragment string
	issue    string
}{
	{"activity typical durations sort", "FROM activity_attempt_durations d LEFT JOIN lease_runners lr", hotReadIssue + "1057"},
	{"activity running attempts sort", "n.status='running' AND l.released_at IS NULL", hotReadIssue + "1057"},
	{"activity finished attempts sort", "n.status!='running' AND n.activity_updated_at>=julianday(:from)", hotReadIssue + "1057"},
	{"work item latest change sort", "ORDER BY c.rowid DESC LIMIT 1", hotReadIssue + "1057"},
	{"analytics landed changes sort", "SELECT c.id,c.work_item_id,json_extract(c.record_json,'$.landed') FROM change_requests c JOIN change_versions v", hotReadIssue + "976"},
	{"analytics quality landings sort", "WITH landed AS ( SELECT c.id,c.work_item_id,v.id AS version_id", hotReadIssue + "976"},
	{"analytics residence transitions sort", "type='workflow.transitioned' AND julianday(recorded_at)>=julianday(?)", hotReadIssue + "1045"},
	{"analytics review evidence sort", "FROM change_evidence e JOIN change_requests c ON c.id=e.change_id", hotReadIssue + "976"},
}

const hotReadIssue = "prj_6d4919bebd73446798e6cd807feda10e#"

var sqlTableReference = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_]*)(?:\s+(?:AS\s+)?([a-z_][a-z0-9_]*))?`)

var sqlReservedAfterTable = map[string]bool{"where": true, "join": true, "left": true, "inner": true, "cross": true, "on": true, "order": true, "group": true, "limit": true, "using": true, "natural": true, "indexed": true, "not": true, "union": true, "except": true, "intersect": true, "having": true, "window": true, "as": true, "outer": true, "full": true, "right": true, "returning": true}

// hotReadPlanViolations reports the plan lines that break the hot read rules:
// a full scan of a large table, or a temporary B-tree built for the ORDER BY
// of a statement that reads a large table.
func hotReadPlanViolations(statement string, plan []string) []string {
	tables := sqlTableAliases(statement)
	readsHotTable := false
	for _, table := range tables {
		readsHotTable = readsHotTable || slices.Contains(hotReadTables, table)
	}
	var violations []string
	for _, detail := range plan {
		if readsHotTable && strings.Contains(detail, "TEMP B-TREE") && strings.Contains(detail, "ORDER BY") {
			violations = append(violations, detail)
			continue
		}
		name, ok := strings.CutPrefix(detail, "SCAN ")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, " ")
		if slices.Contains(hotReadTables, tables[strings.ToLower(name)]) {
			violations = append(violations, detail)
		}
	}
	return violations
}

func sqlTableAliases(statement string) map[string]string {
	tables := make(map[string]string)
	for _, match := range sqlTableReference.FindAllStringSubmatch(statement, -1) {
		table := strings.ToLower(match[1])
		tables[table] = table
		if alias := strings.ToLower(match[2]); alias != "" && !sqlReservedAfterTable[alias] {
			tables[alias] = table
		}
	}
	return tables
}

func explainQueryPlan(t *testing.T, db *sql.DB, statement string, args []any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

type tracedStatement struct {
	query string
	args  []any
}

func TestHotReadQueryPlans(t *testing.T) {
	f := newUsageHostedFixture(t)
	reader := f.service.database.reader
	t.Run("rules reject injected unindexed reads", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			statement string
			args      []any
			violates  bool
		}{
			{"full scan through an alias", "SELECT n.id FROM native_attempts n WHERE json_extract(n.data_json,'$.runtime') IS NOT NULL", nil, true},
			{"full scan without an alias", "SELECT count(*) FROM collaboration_events", nil, true},
			{"sort without an index", "SELECT attempt_id FROM attempt_usage WHERE organization_id=? ORDER BY cost_estimate DESC", []any{"org"}, true},
			{"sort of a small table", "SELECT id FROM organizations ORDER BY name", nil, false},
			{"primary key lookup", "SELECT id FROM native_attempts WHERE id=?", []any{"attempt"}, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				plan := explainQueryPlan(t, reader, test.statement, test.args)
				if got := len(hotReadPlanViolations(test.statement, plan)) > 0; got != test.violates {
					t.Fatalf("violates=%t want %t plan=%v", got, test.violates, plan)
				}
			})
		}
	})

	organization := browserHostedOrganizationBase
	project := organization + "/projects/" + f.project
	known := make(map[string]bool)
	for _, read := range []struct {
		name string
		path string
	}{
		{"activity", organization + "/activity"},
		{"work items list", project + "/work-items?limit=50"},
		{"board lanes", project + "/work-items?include=board&state=Todo&open=true&limit=50"},
		{"projects", organization + "/projects"},
		{"reports", organization + "/reports?project=" + f.project + "&range=30d"},
		{"reports filtered by issue", organization + "/reports?project=" + f.project + "&range=30d&labels=cloud&priorities=1&types=bug&filed_by=principal"},
		{"reports filtered by attempt", organization + "/reports?project=" + f.project + "&range=30d&runners=runner_browser_preview&models=gpt-6-astra&stages=code"},
		{"report filter options", organization + "/reports/options?project=all&range=30d"},
		{"reports all projects over a custom range", organization + "/reports?project=all&from=" + f.service.config.now().UTC().AddDate(0, 0, -89).Format(time.DateOnly) + "&to=" + f.service.config.now().UTC().Format(time.DateOnly) + "&labels=cloud"},
		{"diagnostics", organization + "/diagnostics?range=30d"},
	} {
		t.Run(read.name, func(t *testing.T) {
			var mu sync.Mutex
			var statements []tracedStatement
			timing := &requestTiming{trace: func(query string, args []driver.NamedValue) {
				values := make([]any, 0, len(args))
				for _, arg := range args {
					if arg.Name != "" {
						values = append(values, sql.Named(arg.Name, arg.Value))
						continue
					}
					values = append(values, arg.Value)
				}
				mu.Lock()
				statements = append(statements, tracedStatement{query, values})
				mu.Unlock()
			}}
			ctx, cancel := context.WithTimeout(withRequestTiming(t.Context(), timing), 30*time.Second)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, f.server.URL+read.path, nil).WithContext(ctx)
			request.AddCookie(f.cookies["owner"])
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if read.name == "reports" {
				for _, statement := range statements {
					if strings.Contains(statement.query, "main.issues") || strings.Contains(statement.query, "SELECT DISTINCT r.id") {
						t.Errorf("an unfiltered report ran a filter or option statement: %s", statement.query)
					}
				}
			}
			explained := 0
			for _, statement := range statements {
				head := strings.ToUpper(strings.TrimSpace(statement.query))
				if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") {
					continue
				}
				explained++
				plan := explainQueryPlan(t, reader, statement.query, statement.args)
				violations := hotReadPlanViolations(statement.query, plan)
				if len(violations) == 0 {
					continue
				}
				normalized := strings.Join(strings.Fields(statement.query), " ")
				entry := slices.IndexFunc(knownSlowHotReads, func(entry struct{ name, fragment, issue string }) bool {
					return strings.Contains(normalized, entry.fragment)
				})
				if entry >= 0 {
					known[knownSlowHotReads[entry].name] = true
					continue
				}
				t.Errorf("%s read violates the plan rules %v\nstatement: %s\nplan: %v", read.name, violations, statement.query, plan)
			}
			if explained == 0 {
				t.Fatal("no read statements were traced")
			}
		})
	}
	for _, entry := range knownSlowHotReads {
		if !known[entry.name] {
			t.Errorf("known slow read %q (%s) no longer fails the plan rules; remove its entry", entry.name, entry.issue)
		}
	}
}

func TestAnalyticsCommitPlans(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	var item string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? ORDER BY id LIMIT 1", scope.organization, scope.project).Scan(&item); err != nil {
		t.Fatal(err)
	}
	scanned := []string{"collaboration_events", "native_attempts", "attempt_usage", "leases", "issues", "analytics_hourly_rollups", "analytics_rollup_positions"}
	var statements []tracedStatement
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, args []driver.NamedValue) {
		values := make([]any, 0, len(args))
		for _, arg := range args {
			values = append(values, arg.Value)
		}
		statements = append(statements, tracedStatement{query, values})
	}})
	tx, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now()
	for _, event := range []struct {
		kind string
		data tracker.CollaborationData
	}{
		{"scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", At: now}}},
		{"workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}},
	} {
		if err := appendNativeHistory(ctx, tx, scope, item, event.kind, event.data, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE attempt_usage SET cost_estimate=cost_estimate+1 WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		t.Fatal(err)
	}
	statements = statements[:0]
	f.service.database.refreshAnalyticsRollups(ctx, tx)
	traced := slices.Clone(statements)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	explained := 0
	for _, statement := range traced {
		head := strings.ToUpper(strings.TrimSpace(statement.query))
		if strings.HasPrefix(head, "SAVEPOINT") || strings.HasPrefix(head, "RELEASE") || strings.HasPrefix(head, "ROLLBACK") {
			continue
		}
		explained++
		tables := sqlTableAliases(statement.query)
		for _, detail := range explainQueryPlan(t, f.service.database.db, statement.query, statement.args) {
			if violation := analyticsCommitPlanViolation(detail, tables, scanned); violation != "" {
				t.Errorf("commit-path statement %s: %s\nstatement: %s", violation, detail, statement.query)
			}
		}
	}
	if explained == 0 {
		t.Fatal("no commit-path statements were traced")
	}
}

func analyticsCommitPlanViolation(detail string, tables map[string]string, scanned []string) string {
	kind, rest, ok := strings.Cut(detail, " ")
	if !ok || (kind != "SCAN" && kind != "SEARCH") {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	if !slices.Contains(scanned, tables[strings.ToLower(name)]) {
		return ""
	}
	if kind == "SCAN" {
		return "scans a hot table"
	}
	open, close := strings.LastIndex(rest, "("), strings.LastIndex(rest, ")")
	if open < 0 || close < open {
		return "searches without an index constraint"
	}
	for _, term := range strings.Split(rest[open+1:close], " AND ") {
		column := strings.TrimSpace(strings.FieldsFunc(term, func(r rune) bool { return r == '=' || r == '<' || r == '>' })[0])
		if column != "organization_id" && column != "project_id" {
			return ""
		}
	}
	return "reads a whole organization or project"
}

// seedProductionShapedRollups adds the rollup population of the production
// tenant whose Reports timed out: thousands of attempts with runtime
// payloads, usage rows and tens of thousands of transition events spread
// over ninety days.
func seedProductionShapedRollups(t *testing.T, s *Service, scope nativeScope, attempts, usage, events int) {
	t.Helper()
	now := s.config.now().UTC().Format(usagePeriodLayout)
	statements := []struct {
		query string
		args  []any
	}{
		{`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?-1),
items AS (SELECT native_id,row_number() OVER (ORDER BY native_id)-1 AS k,count(*) OVER () AS total FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL),
shaped AS (SELECT i,strftime('%Y-%m-%dT%H:%M:%SZ',?,'-'||(i*7776000/?)||' seconds') AS at FROM n)
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'bench_event_'||i,?,?,(SELECT native_id FROM items WHERE k=i%total),1000000+i,'workflow.transitioned',1,'{}',json_object('from_state',CASE WHEN i%5=0 THEN 'Done' ELSE 'Todo' END,'to_state','In Progress'),at FROM shaped`,
			[]any{events, scope.organization, scope.project, now, events, scope.organization, scope.project}},
		{analyticsSourceEvents + `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events WHERE organization_id=? AND project_id=? AND dimension LIKE 'bench_event_%'`,
			[]any{scope.organization, scope.project}},
		{`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?-1),
items AS (SELECT native_id,row_number() OVER (ORDER BY native_id)-1 AS k,count(*) OVER () AS total FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL),
shaped AS (SELECT i,strftime('%Y-%m-%dT%H:%M:%SZ',?,'-'||(i*7776000/?)||' seconds') AS at FROM n)
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
SELECT ?,?,unixepoch(at)/3600*3600,'attempt','bench_attempt_'||i,1,json_object('id','bench_attempt_'||i,'work_item_id',(SELECT native_id FROM items WHERE k=i%total),'lease_id','bench_lease_'||i,
 'status',CASE WHEN i%7=0 THEN 'failed' ELSE 'succeeded' END,'started_at',at,'updated_at',strftime('%Y-%m-%dT%H:%M:%SZ',at,'+30 minutes'),
 'data_json',json_object('runtime',json_object('identity',json_object('role','code'),'phases',json_array(json_object('name','code','padding',hex(zeroblob(1500)))))))
FROM shaped`,
			[]any{attempts, scope.organization, scope.project, now, attempts, scope.organization, scope.project}},
		{`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?-1),
shaped AS (SELECT i,'bench_attempt_'||(i%?) AS attempt,strftime('%Y-%m-%dT%H:00:00Z',?,'-'||(i*7776000/?)||' seconds') AS period FROM n)
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
SELECT ?,?,unixepoch(period),'usage',json_array(attempt,period,'openai','gpt-6-astra','USD'),0.25,json_object('attempt_id',attempt,'period',period,'provider','openai','model','gpt-6-astra','input',1000,'cached_input',0,'output',200,'cost_estimate',0.25,'currency','USD','updated_at',period)
FROM shaped`,
			[]any{usage, attempts, now, usage, scope.organization, scope.project}},
	}
	for _, statement := range statements {
		if _, err := s.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed production-shaped rollups: %v\n%s", err, statement.query)
		}
	}
}

type explainedPlanLine struct {
	id, parent int
	detail     string
}

func explainPlanTree(t *testing.T, db *sql.DB, statement string, args []any) []explainedPlanLine {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	defer rows.Close()
	var plan []explainedPlanLine
	for rows.Next() {
		var line explainedPlanLine
		var unused int
		if err := rows.Scan(&line.id, &line.parent, &unused, &line.detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// rollupRepeatedReadViolations reports the reads of analytics_hourly_rollups
// that run once per outer row, in a join's inner loop or a correlated
// subquery, without a key narrower than the project's metric. Each such read
// walks the whole metric of the project for every outer row.
func rollupRepeatedReadViolations(statement string, plan []explainedPlanLine) []string {
	tables := sqlTableAliases(statement)
	byID := make(map[int]explainedPlanLine, len(plan))
	for _, line := range plan {
		byID[line.id] = line
	}
	var violations []string
	accessed := make(map[int]bool)
	for _, line := range plan {
		kind, rest, ok := strings.Cut(line.detail, " ")
		if !ok || (kind != "SCAN" && kind != "SEARCH") {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		name = strings.ToLower(name)
		isRollup := name == "analytics_hourly_rollups" || tables[name] == "analytics_hourly_rollups"
		inner := accessed[line.parent]
		accessed[line.parent] = true
		for parent := line.parent; !inner && parent != 0; parent = byID[parent].parent {
			inner = strings.HasPrefix(byID[parent].detail, "CORRELATED")
		}
		if !isRollup || !inner {
			continue
		}
		if kind == "SCAN" {
			violations = append(violations, line.detail)
			continue
		}
		open, close := strings.LastIndex(rest, "("), strings.LastIndex(rest, ")")
		keyed := false
		if open >= 0 && close > open {
			for _, term := range strings.Split(rest[open+1:close], " AND ") {
				column := strings.TrimSpace(strings.FieldsFunc(term, func(r rune) bool { return r == '=' || r == '<' || r == '>' })[0])
				keyed = keyed || (column != "organization_id" && column != "project_id" && column != "metric")
			}
		}
		if !keyed {
			violations = append(violations, line.detail)
		}
	}
	return violations
}

// windowedRollupReads names the rollup reads bounded by the report window;
// each must reach its rows through the window, not the whole project.
var windowedRollupReads = []struct{ name, fragment, index string }{
	{"usage rows", "FROM analytics_attempt_usage u LEFT JOIN analytics_native_attempts a ON a.id = u.attempt_id", "analytics_usage_window"},
	{"attempts in window", "julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?=''", "analytics_attempt_updated"},
	{"residence transitions", "type='workflow.transitioned' AND julianday(recorded_at)>=julianday(?)", "analytics_event_window"},
}

func TestAnalyticsRollupReportPlans(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	seedProductionShapedRollups(t, f.service, scope, 5000, 3000, 20000)
	reader := f.service.database.reader
	for _, window := range []string{"7d", "30d"} {
		t.Run(window, func(t *testing.T) {
			var mu sync.Mutex
			var statements []tracedStatement
			timing := &requestTiming{trace: func(query string, args []driver.NamedValue) {
				values := make([]any, 0, len(args))
				for _, arg := range args {
					values = append(values, arg.Value)
				}
				mu.Lock()
				statements = append(statements, tracedStatement{query, values})
				mu.Unlock()
			}}
			ctx, cancel := context.WithTimeout(withRequestTiming(t.Context(), timing), 2*time.Minute)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, f.server.URL+browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+window, nil).WithContext(ctx)
			request.AddCookie(f.cookies["owner"])
			response := httptest.NewRecorder()
			started := time.Now()
			f.service.Handler().ServeHTTP(response, request)
			elapsed := time.Since(started)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d after %s: %s", response.Code, elapsed, response.Body.String())
			}
			t.Logf("reports %s on the production-shaped rollups took %s", window, elapsed)
			mu.Lock()
			defer mu.Unlock()
			windowed := make(map[string]bool)
			for _, statement := range statements {
				if !strings.Contains(statement.query, "analytics_hourly_rollups") && !strings.Contains(statement.query, "analytics_native_attempts") && !strings.Contains(statement.query, "analytics_attempt_usage") {
					continue
				}
				plan := explainPlanTree(t, reader, statement.query, statement.args)
				details := make([]string, 0, len(plan))
				for _, line := range plan {
					details = append(details, line.detail)
				}
				if violations := rollupRepeatedReadViolations(statement.query, plan); len(violations) > 0 {
					t.Errorf("rollup read repeats an unkeyed project read %v\nstatement: %s\nplan: %v", violations, statement.query, details)
				}
				normalized := strings.Join(strings.Fields(statement.query), " ")
				for _, read := range windowedRollupReads {
					if !strings.Contains(normalized, read.fragment) {
						continue
					}
					windowed[read.name] = true
					bounded := slices.ContainsFunc(details, func(detail string) bool {
						return strings.Contains(detail, " INDEX "+read.index+" (") && strings.Contains(detail, ">?")
					})
					if !bounded {
						t.Errorf("%s read does not use the window index %s\nstatement: %s\nplan: %v", read.name, read.index, statement.query, details)
					}
				}
			}
			for _, read := range windowedRollupReads {
				if !windowed[read.name] {
					t.Errorf("reports %s did not run the %s read", window, read.name)
				}
			}
		})
	}
}
