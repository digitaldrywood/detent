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
	{"analytics review evidence sort", "FROM change_evidence e JOIN change_requests c ON c.id=e.change_id", hotReadIssue + "976"},
}

const hotReadIssue = "prj_6d4919bebd73446798e6cd807feda10e#"

var sqlTableReference = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_]*)(?:\s+(?:AS\s+)?([a-z_][a-z0-9_]*))?`)

var sqlReservedAfterTable = map[string]bool{"where": true, "join": true, "left": true, "inner": true, "cross": true, "on": true, "order": true, "group": true, "limit": true, "using": true, "natural": true, "indexed": true, "not": true, "union": true, "except": true, "intersect": true, "having": true, "window": true, "as": true, "outer": true, "full": true, "right": true, "returning": true}

// hotReadPlanViolations reports the plan lines that break the hot read rules:
// a full scan of a large table, or a temporary B-tree built for the ORDER BY
// of a statement that reads a large table.
func hotReadPlanViolations(statement string, plan []string) []string {
	tables := make(map[string]string)
	readsHotTable := false
	for _, match := range sqlTableReference.FindAllStringSubmatch(statement, -1) {
		table := strings.ToLower(match[1])
		tables[table] = table
		readsHotTable = readsHotTable || slices.Contains(hotReadTables, table)
		if alias := strings.ToLower(match[2]); alias != "" && !sqlReservedAfterTable[alias] {
			tables[alias] = table
		}
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
