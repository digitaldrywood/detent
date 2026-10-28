package hubserver

import (
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

	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var hotReadTables = []string{"native_attempts", "collaboration_events", "change_requests", "attempt_usage", "analytics_hourly_rollups", "analytics_native_attempts", "analytics_attempt_usage", "analytics_issues", "analytics_leases"}

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
		if slices.Contains(hotReadTables, tables[strings.ToLower(name)]) || slices.Contains(hotReadTables, strings.ToLower(name)) {
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
	var item string
	if err := reader.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE project_id=? AND native_id IS NOT NULL LIMIT 1", f.project).Scan(&item); err != nil {
		t.Fatal(err)
	}
	known := make(map[string]bool)
	for _, read := range []struct {
		name string
		path string
	}{
		{"activity", organization + "/activity"},
		{"newest issue history", project + "/work-items/" + item + "/history?direction=newest&view=timeline&limit=100"},
		{"newest issue attempts", project + "/work-items/" + item + "/attempts?direction=newest&limit=100"},
		{"newest issue comments", project + "/work-items/" + item + "/comments?direction=newest&limit=100"},
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
			ctx := testenv.WaitContext(t, withRequestTiming(t.Context(), timing))
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
	if err := f.service.database.projectAnalyticsBatch(ctx, tx, f.service.config.now(), analyticsBackfillBatch); err != nil {
		t.Fatal(err)
	}
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
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,analytics_from_state,analytics_to_state,analytics_projection_version)
SELECT 'bench_event_'||i,?,?,(SELECT native_id FROM items WHERE k=i%total),1000000+i,'workflow.transitioned',1,'{}',json_object('from_state',CASE WHEN i%5=0 THEN 'Done' ELSE 'Todo' END,'to_state','In Progress'),at,CASE WHEN i%5=0 THEN 'Done' ELSE 'Todo' END,'In Progress',1 FROM shaped`,
			[]any{events, scope.organization, scope.project, now, events, scope.organization, scope.project}},
		{analyticsSourceEvents + `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,projection_version) SELECT organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,1 FROM analytics_source_events WHERE organization_id=? AND project_id=? AND dimension LIKE 'bench_event_%'`,
			[]any{scope.organization, scope.project}},
		{`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<?-1),
items AS (SELECT native_id,row_number() OVER (ORDER BY native_id)-1 AS k,count(*) OVER () AS total FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL),
shaped AS (SELECT i,strftime('%Y-%m-%dT%H:%M:%SZ',?,'-'||(i*7776000/?)||' seconds') AS at FROM n)
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
SELECT ?,?,unixepoch(at)/3600*3600,'attempt','bench_attempt_'||i,1,json_object('id','bench_attempt_'||i,'work_item_id',(SELECT native_id FROM items WHERE k=i%total),'lease_id','bench_lease_'||i,
 'status',CASE WHEN i%7=0 THEN 'failed' ELSE 'succeeded' END,'started_at',at,'updated_at',strftime('%Y-%m-%dT%H:%M:%SZ',at,'+30 minutes'),
 'failed_at',CASE WHEN i%7=0 THEN strftime('%Y-%m-%dT%H:%M:%SZ',at,'+30 minutes') END,
 'runtime',json_object('identity',json_object('role','code'),'phases',json_array(json_object('name','code','padding',hex(zeroblob(1500))))))
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
	projectTestChangeColumns(t, s)
	convertTestAnalyticsProjections(t, s)
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
// each must reach its rows through its index, not the whole project. A read
// ordered by its index carries the window in the index's own columns, so it
// reads no payload outside the window and has no bound in its search.
var windowedRollupReads = []struct{ name, fragment, index, bound string }{
	{"usage rows", "FROM analytics_attempt_usage u LEFT JOIN analytics_native_attempts a ON a.id = u.attempt_id", "analytics_usage_window", ">?"},
	{"attempts in window", "julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?=''", "analytics_attempt_started", ""},
	{"failed attempts in window", "WHERE metric='attempt' AND failed_at IS NOT NULL", "analytics_attempt_failed", ">?"},
	{"residence transitions", "type='workflow.transitioned' AND julianday(recorded_at)>=julianday(?)", "analytics_event_order", ">?"},
}

// TestAnalyticsRollupReportPlans reads Reports over production-shaped
// tenants: the rollup volume of the largest project and the payload sizes of
// the project whose Reports took 4 to 17 seconds. Every statement keeps the
// hot read plan rules, reads its window through an index and never repeats
// an unkeyed project read. The logged times are the production-sized
// benchmark for the 500 ms Reports budget.
func TestAnalyticsRollupReportPlans(t *testing.T) {
	for _, tenant := range []struct {
		name string
		seed func(*testing.T, *Service, nativeScope)
	}{
		{"rollup volume", func(t *testing.T, s *Service, scope nativeScope) {
			seedProductionShapedRollups(t, s, scope, 5000, 3000, 20000)
		}},
		{"attempt payloads", func(t *testing.T, s *Service, scope nativeScope) { seedProductionProject(t, s, scope, pyroApexShape) }},
	} {
		t.Run(tenant.name, func(t *testing.T) {
			f := newUsageHostedFixture(t)
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
			tenant.seed(t, f.service, scope)
			projectTestChangeColumns(t, f.service)
			reader := f.service.database.reader
			for _, window := range []string{"24h", "7d", "30d"} {
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
					ctx := testenv.WaitContext(t, withRequestTiming(t.Context(), timing))
					request := httptest.NewRequest(http.MethodGet, f.server.URL+browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+window, nil).WithContext(ctx)
					request.AddCookie(f.cookies["owner"])
					response := httptest.NewRecorder()
					started := time.Now()
					f.service.Handler().ServeHTTP(response, request)
					elapsed := time.Since(started)
					if response.Code != http.StatusOK {
						t.Fatalf("HTTP %d after %s: %s", response.Code, elapsed, response.Body.String())
					}
					timing.start = started
					t.Logf("reports %s on %s took %s: %s", window, tenant.name, elapsed, timing.header(started.Add(elapsed)))
					mu.Lock()
					defer mu.Unlock()
					windowed := make(map[string]bool)
					for _, statement := range statements {
						head := strings.ToUpper(strings.TrimSpace(statement.query))
						if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") {
							continue
						}
						plan := explainPlanTree(t, reader, statement.query, statement.args)
						details := make([]string, 0, len(plan))
						for _, line := range plan {
							details = append(details, line.detail)
						}
						normalized := strings.Join(strings.Fields(statement.query), " ")
						known := slices.ContainsFunc(knownSlowHotReads, func(entry struct{ name, fragment, issue string }) bool {
							return strings.Contains(normalized, entry.fragment)
						})
						if violations := hotReadPlanViolations(statement.query, details); len(violations) > 0 && !known {
							t.Errorf("reports read violates the plan rules %v\nstatement: %s\nplan: %v", violations, statement.query, details)
						}
						if violations := rollupRepeatedReadViolations(statement.query, plan); len(violations) > 0 {
							t.Errorf("rollup read repeats an unkeyed project read %v\nstatement: %s\nplan: %v", violations, statement.query, details)
						}
						for _, read := range windowedRollupReads {
							if !strings.Contains(normalized, read.fragment) {
								continue
							}
							windowed[read.name] = true
							indexed := slices.ContainsFunc(details, func(detail string) bool {
								return strings.Contains(detail, " INDEX "+read.index+" (") && strings.Contains(detail, read.bound)
							})
							if !indexed {
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
		})
	}
}

// productionProjectShape is the row population of one production project.
type productionProjectShape struct {
	issues, attempts, usage, leases, comments int
	days                                      int
}

// pyroApexShape matches the production project whose Reports took 4 to 17
// seconds after its rollup indexes were fixed: 594 issues imported over two
// days, 469 attempts whose payloads average 48 KB with 19 KB activity
// profiles and 64 KB validation output, 262 usage rows, 1682 leases and
// 12.6k events.
var pyroApexShape = productionProjectShape{issues: 594, attempts: 469, usage: 262, leases: 1682, comments: 7000}

// seedProductionProject writes a production-shaped project through the raw
// tables and rebuilds its rollups the way the backfill does, so every
// derived rollup has production's row counts and payload sizes.
func seedProductionProject(t testing.TB, s *Service, scope nativeScope, shape productionProjectShape) {
	t.Helper()
	now := formatHubTime(s.config.now().UTC())
	org, project := string(scope.organization), string(scope.project)
	days := shape.days
	if days == 0 {
		days = 2
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at)
SELECT ?,name,name,terminal,dispatchable,?,? FROM (SELECT 'Backlog' AS name,0 AS terminal,0 AS dispatchable UNION ALL SELECT 'Todo',0,1 UNION ALL SELECT 'In Progress',0,0
 UNION ALL SELECT 'Rework',0,1 UNION ALL SELECT 'Merging',0,0 UNION ALL SELECT 'Done',1,0 UNION ALL SELECT 'Cancelled',1,0) AS wanted
WHERE NOT EXISTS(SELECT 1 FROM workflow_states ws WHERE ws.project_id=? AND ws.detent_state=wanted.name)`, []any{project, now, now, project}},
		{`WITH RECURSIVE n(k) AS (SELECT 0 UNION ALL SELECT k+1 FROM n WHERE k<?-1),
shaped AS (SELECT k,strftime('%Y-%m-%dT%H:%M:%SZ',?,'-'||(?-k*?/?)||' seconds') AS created,
 CASE WHEN k%11<4 THEN 'Done' WHEN k%11=4 THEN 'Cancelled' WHEN k%11<9 THEN 'Backlog' WHEN k%11=9 THEN 'Todo' ELSE 'In Progress' END AS state FROM n)
INSERT INTO issues(workflow_state_id,native_id,organization_id,project_id,number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,native_created_at,native_updated_at)
SELECT (SELECT min(id) FROM workflow_states WHERE project_id=? AND detent_state=state),'wi_shaped_'||k,?,?,100000+k,'Shaped issue '||k,'https://example.test/'||k,'open','v1',created,created,created,created,created,created FROM shaped`,
			[]any{shape.issues, now, days * 86400, days*86400 - 28800, shape.issues, project, org, project}},
		{`WITH steps(state,seq,kind,from_state,to_state,offset) AS (VALUES
 ('Done',2,'workflow.transitioned','Backlog','Todo',600),('Done',3,'scheduler.decision','','',900),('Done',4,'workflow.transitioned','Todo','In Progress',1200),
 ('Done',5,'workflow.transitioned','In Progress','Merging',7200),('Done',6,'workflow.transitioned','Merging','Rework',7800),('Done',7,'scheduler.decision','','',8000),
 ('Done',8,'workflow.transitioned','Rework','In Progress',8100),('Done',9,'workflow.transitioned','In Progress','Merging',8400),('Done',10,'workflow.transitioned','Merging','Done',9000),
 ('Cancelled',2,'workflow.transitioned','Backlog','Cancelled',1800),('Todo',2,'workflow.transitioned','Backlog','Todo',600),
 ('In Progress',2,'workflow.transitioned','Backlog','Todo',600),('In Progress',3,'scheduler.decision','','',900),('In Progress',4,'workflow.transitioned','Todo','In Progress',1200)),
items AS (SELECT i.native_id,i.created_at,ws.detent_state AS state FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id LIKE 'wi_shaped_%'),
events AS (SELECT native_id,1 AS seq,'issue.created' AS kind,'' AS from_state,'' AS to_state,created_at AS at FROM items
 UNION ALL SELECT native_id,seq,kind,from_state,to_state,strftime('%Y-%m-%dT%H:%M:%SZ',created_at,'+'||offset||' seconds') FROM items JOIN steps USING(state))
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'ev_'||native_id||'_'||seq,?,?,native_id,seq,kind,1,'{}',
 CASE kind WHEN 'scheduler.decision' THEN json_object('decision',json_object('source','native_claim','outcome','claimed','at',at)) WHEN 'issue.created' THEN '{}' ELSE json_object('from_state',from_state,'to_state',to_state) END,at FROM events`,
			[]any{org, project, org, project}},
		{`WITH RECURSIVE n(c) AS (SELECT 0 UNION ALL SELECT c+1 FROM n WHERE c<?-1),
items AS (SELECT native_id,created_at,row_number() OVER (ORDER BY native_id)-1 AS k,count(*) OVER () AS total FROM issues WHERE organization_id=? AND project_id=? AND native_id LIKE 'wi_shaped_%')
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'ev_comment_'||c,?,?,native_id,100+c/total,'comment.created',1,'{}',json_object('body',hex(zeroblob(100))),strftime('%Y-%m-%dT%H:%M:%SZ',created_at,'+'||(10800+c/total*60)||' seconds')
FROM n JOIN items ON items.k=n.c%items.total`,
			[]any{shape.comments, org, project, org, project}},
		{`UPDATE issues SET event_sequence=(SELECT max(sequence) FROM collaboration_events e WHERE e.organization_id=issues.organization_id AND e.work_item_id=issues.native_id) WHERE organization_id=? AND project_id=? AND native_id LIKE 'wi_shaped_%'`, []any{org, project}},
		{`WITH RECURSIVE n(a) AS (SELECT 0 UNION ALL SELECT a+1 FROM n WHERE a<?-1),
worked AS (SELECT i.id,i.created_at,row_number() OVER (ORDER BY i.native_id)-1 AS k,count(*) OVER () AS total FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=? AND i.project_id=? AND i.native_id LIKE 'wi_shaped_%' AND ws.detent_state IN ('Done','In Progress')),
shaped AS (SELECT a,a<total AS first,worked.id AS issue,strftime('%Y-%m-%dT%H:%M:%SZ',created_at,'+'||(1200+a/total*1800)||' seconds') AS started FROM n JOIN worked ON worked.k=n.a%worked.total)
INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at)
SELECT 'lease_shaped_'||a,issue,'machine_browser_preview_runner','session_shaped_'||a,strftime('%Y-%m-%dT%H:%M:%SZ',started,'+1500 seconds'),started,started,
 CASE WHEN first AND a<? AND a%15=1 THEN NULL ELSE strftime('%Y-%m-%dT%H:%M:%SZ',started,'+1500 seconds') END,started,started FROM shaped`,
			[]any{shape.leases, org, project, shape.attempts}},
		{`WITH RECURSIVE n(a) AS (SELECT 0 UNION ALL SELECT a+1 FROM n WHERE a<?-1),
shaped AS (SELECT a,l.lease_id,l.fencing_token,i.native_id,l.acquired_at AS started,strftime('%Y-%m-%dT%H:%M:%SZ',l.acquired_at,'+1500 seconds') AS updated,
 CASE a%3 WHEN 0 THEN 'code' WHEN 1 THEN 'validator' ELSE 'merge' END AS role,
 CASE WHEN l.released_at IS NULL THEN 'running' WHEN a%14=0 THEN 'failed' ELSE 'succeeded' END AS status,
 substr(hex(randomblob(20)),1,40) AS sha
 FROM n JOIN leases l ON l.lease_id='lease_shaped_'||a JOIN issues i ON i.id=l.issue_id)
INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at)
SELECT 'attempt_shaped_'||a,?,?,native_id,lease_id,fencing_token,'run_shaped_'||a,1,status,json_object('run_id','run_shaped_'||a,'attempt_id','attempt_shaped_'||a,'outcome',status,'policy_id',hex(randomblob(35)),
 'runtime',json_object('identity',json_object('role',role),'phase',role,'phases',json_array(json_object('name',role,'started_at',started,'finished_at',updated)),'phases_dropped',0,'heartbeat_at',updated,
  'activity',json_object('schema',1,'stage',role,'started_at',started,'as_of',updated,'status','completed','coverage','complete','dropped_events',0,'unpaired_events',0,
   'spans',(SELECT json_group_array(json_object('id','span_'||s,'parent_id','','kind','command','evidence',hex(zeroblob(1400)),'attribution','agent','started_at',started,'finished_at',updated,'outcome','succeeded','repeat',1))
    FROM (SELECT 0 AS s UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5))),
  'validation',CASE WHEN role='validator' THEN json_object('command','make check','head_sha',sha,'started_at',started,'finished_at',updated,'output',hex(zeroblob(32768))) END,
  'landing',CASE WHEN role='merge' THEN json_object('landed',json('true'),'change_id','change_'||native_id,'version_id','version_'||native_id,'head_sha',sha,'merge_sha',sha,'observed_at',updated) END)),
 started,updated FROM shaped`,
			[]any{shape.attempts, org, project}},
		{`WITH RECURSIVE n(u) AS (SELECT 0 UNION ALL SELECT u+1 FROM n WHERE u<?-1)
INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at)
SELECT a.id,?,?,strftime('%Y-%m-%dT%H:00:00Z',a.started_at),'openai','gpt-6-astra',100000,40000,8000,0.25,'USD',a.updated_at FROM n JOIN native_attempts a ON a.id='attempt_shaped_'||(n.u*?/?)`,
			[]any{shape.usage, org, project, shape.attempts, shape.usage}},
		{`INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json)
SELECT 'change_'||i.native_id,i.organization_id,i.project_id,i.native_id,json_object('landed',json_object('landed',json('true'),'change_id','change_'||i.native_id,'version_id','version_'||i.native_id,
 'head_sha',substr('0000000000000000000000000000000000000000'||i.number,-40),'merge_sha',substr('1111111111111111111111111111111111111111'||i.number,-40),
 'landed_at',strftime('%Y-%m-%dT%H:%M:%SZ',i.created_at,'+9000 seconds'),'observed_at',strftime('%Y-%m-%dT%H:%M:%SZ',i.created_at,'+9000 seconds')))
FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id LIKE 'wi_shaped_%' AND ws.detent_state='Done' AND i.number%11<3`,
			[]any{org, project}},
		{`INSERT INTO change_versions(id,change_id,number,record_json)
SELECT 'version_'||work_item_id,id,1,json_object('head_sha',json_extract(record_json,'$.landed.head_sha')) FROM change_requests WHERE organization_id=? AND project_id=? AND id LIKE 'change_wi_shaped_%'`,
			[]any{org, project}},
		{`INSERT OR REPLACE INTO native_board_items(work_item_id,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json)
SELECT i.native_id,i.organization_id,i.project_id,ws.detent_state,0,ws.terminal,0,0,0,0,'',i.project_id||'#'||i.number,
 CASE WHEN ws.terminal=1 THEN strftime('%Y-%m-%dT%H:%M:%S.000000000Z',i.created_at,CASE ws.detent_state WHEN 'Done' THEN '+9000 seconds' ELSE '+1800 seconds' END) ELSE '' END,'','{}'
FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id LIKE 'wi_shaped_%'`,
			[]any{org, project}},
		{`DELETE FROM native_board_dirty WHERE work_item_id LIKE 'wi_shaped_%'`, nil},
	}
	for _, statement := range statements {
		if _, err := s.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed production project: %v\n%s", err, statement.query)
		}
	}
	projectTestChangeColumns(t, s)
	rebuildTestAnalyticsRollups(t, s, scope)
}
