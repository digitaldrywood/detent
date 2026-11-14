package pgstoretest

import (
	"fmt"
	"strings"
	"testing"
)

var reportTestContract = ReportPlanContract{
	Relations: []string{"issues", "collaboration_events"},
	Indexes: map[string]ReportIndex{
		"issues_pkey":               {Unique: true, Columns: []string{"organization_id", "id"}},
		"collaboration_events_pkey": {Unique: true, Columns: []string{"organization_id", "id"}},
		"events_item":               {Columns: []string{"organization_id", "work_item_id", "type", "recorded_at"}},
		"events_window":             {Columns: []string{"organization_id", "recorded_at"}},
		"issues_state":              {Columns: []string{"organization_id", "state", "id"}},
	},
	Cap: 50000,
}

func reportTestNode(kind, extra string, children ...string) string {
	node := `{"Node Type":"` + kind + `"`
	if extra != "" {
		node += "," + extra
	}
	if len(children) > 0 {
		node += `,"Plans":[` + strings.Join(children, ",") + `]`
	}
	return node + "}"
}

func reportTestScan(kind, relation, alias, index, condition, rows, loops, extra string) string {
	fields := fmt.Sprintf(`"Relation Name":%q,"Alias":%q,"Index Name":%q,"Index Cond":%q,"Actual Rows":%s,"Actual Loops":%s`, relation, alias, index, condition, rows, loops)
	if extra != "" {
		fields += "," + extra
	}
	return reportTestNode(kind, fields)
}

func reportTestPlan(root string) string {
	return `[{"Plan":` + root + `}]`
}

const (
	reportTenantEvents = "(e.organization_id = 'org'::text)"
	reportTenantIssues = "(i.organization_id = 'org'::text)"
	reportItemEvents   = "((e.organization_id = 'org'::text) AND (e.work_item_id = c.id))"
	reportCandidates   = `"Function Call":"unnest('{1,2,3}'::bigint[])","Actual Rows":3,"Actual Loops":1`
)

func TestReportPlanControls(t *testing.T) {
	outer := `"Parent Relationship":"Outer"`
	inner := `"Parent Relationship":"Inner"`
	candidates := reportTestNode("Function Scan", outer+","+reportCandidates)
	for _, test := range []struct {
		name      string
		statement string
		args      []any
		plan      string
		want      string
	}{
		{"ordered candidate scan under its own limit", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT $2", []any{"org", 50001},
			reportTestNode("Limit", `"Actual Rows":50,"Actual Loops":1`, reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "50", "1", outer)), ""},
		{"tenant-wide retained-history scan", "SELECT count(*) FROM collaboration_events e WHERE e.organization_id=$1", []any{"org"},
			reportTestNode("Aggregate", `"Strategy":"Plain"`, reportTestScan("Index Only Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "40000", "1", outer)), "collaboration_events e (Index Only Scan) in scope \"\" has no proven bound"},
		{"time range with growing density and no cap", "SELECT id FROM collaboration_events e WHERE e.organization_id=$1 AND e.recorded_at>=$2 AND e.recorded_at<$3", []any{"org", 1, 2},
			reportTestScan("Index Scan", "collaboration_events", "e", "events_window", "((e.organization_id = 'org'::text) AND (e.recorded_at >= '2026-01-01'::timestamptz) AND (e.recorded_at < '2026-02-01'::timestamptz))", "9000", "1", ""), "has no proven bound"},
		{"limit above a consuming sort", "SELECT id FROM collaboration_events e WHERE e.organization_id=$1 ORDER BY e.type LIMIT 10", []any{"org"},
			reportTestNode("Limit", "", reportTestNode("Sort", outer, reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "9000", "1", outer))), "has no proven bound"},
		{"limit above a hashed aggregate", "SELECT e.type FROM collaboration_events e WHERE e.organization_id=$1 GROUP BY e.type LIMIT 10", []any{"org"},
			reportTestNode("Limit", "", reportTestNode("Aggregate", outer+`,"Strategy":"Hashed"`, reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "9000", "1", outer))), "has no proven bound"},
		{"limit above a window aggregate", "SELECT row_number() OVER () FROM collaboration_events e WHERE e.organization_id=$1 LIMIT 10", []any{"org"},
			reportTestNode("Limit", "", reportTestNode("WindowAgg", outer, reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "9000", "1", outer))), "has no proven bound"},
		{"limit above a hash build", "SELECT c.id FROM unnest($2::bigint[]) c(id) JOIN collaboration_events e ON e.organization_id=$1 AND e.work_item_id=c.id LIMIT 10", []any{"org", "{1,2,3}"},
			reportTestNode("Limit", "", reportTestNode("Hash Join", outer+`,"Join Type":"Inner"`, candidates, reportTestNode("Hash", inner, reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "9000", "1", outer)))), "has no proven bound"},
		{"hashed subplan reads its whole input", "SELECT id FROM issues i WHERE i.organization_id=$1 AND i.native_id IN (SELECT e.work_item_id FROM collaboration_events e WHERE e.organization_id=$1) LIMIT 5", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "5", "1", outer+`,"Filter":"(ANY ((i.native_id = (hashed SubPlan 1).col1)))"`+`,"Plans":[`+reportTestScan("Index Only Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "900", "1", `"Parent Relationship":"SubPlan","Subplan Name":"SubPlan 1"`)+`]`)), "collaboration_events e (Index Only Scan) in scope \"\" has no proven bound"},
		{"array subplan reads every match", "SELECT ARRAY(SELECT e.id FROM collaboration_events e WHERE e.organization_id=$1 AND e.work_item_id=c.id) FROM unnest($2::bigint[]) c(id)", []any{"org", "{1,2,3}"},
			reportTestNode("Function Scan", reportCandidates+`,"Output":["ARRAY(SubPlan 1)"],"Plans":[`+reportTestScan("Index Scan", "collaboration_events", "e", "events_item", reportItemEvents, "40", "3", `"Parent Relationship":"SubPlan","Subplan Name":"SubPlan 1"`)+`]`), "has no proven bound"},
		{"bitmap reads the full population before a small output", "SELECT id FROM collaboration_events e WHERE e.organization_id=$1 LIMIT 5", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Bitmap Heap Scan", "collaboration_events", "e", "", "", "5", "1", outer+`,"Recheck Cond":"(e.organization_id = 'org'::text)","Plans":[`+reportTestNode("Bitmap Index Scan", outer+`,"Index Name":"events_window","Index Cond":"(e.organization_id = 'org'::text)","Actual Rows":9000,"Actual Loops":1`)+`]`)), "built eagerly"},
		{"limit one with arbitrarily many rejected rows", "SELECT c.id,e.id FROM unnest($2::bigint[]) c(id) CROSS JOIN LATERAL (SELECT e.id FROM collaboration_events e WHERE e.organization_id=$1 AND e.work_item_id=c.id AND e.payload_bytes>0 LIMIT 1) e", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestNode("Limit", inner+`,"Actual Rows":1,"Actual Loops":3`, reportTestScan("Index Scan", "collaboration_events", "e", "events_item", reportItemEvents, "1", "3", outer+`,"Filter":"(e.payload_bytes > 0)","Rows Removed by Filter":40000`))), "residual filter"},
		{"late match keeps the residual filter unproven", "SELECT c.id,e.id FROM unnest($2::bigint[]) c(id) CROSS JOIN LATERAL (SELECT e.id FROM collaboration_events e WHERE e.organization_id=$1 AND e.work_item_id=c.id AND e.payload_bytes>0 LIMIT 1) e", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestNode("Limit", inner+`,"Actual Rows":1,"Actual Loops":3`, reportTestScan("Index Scan", "collaboration_events", "e", "events_item", reportItemEvents, "1", "3", outer+`,"Filter":"(e.payload_bytes > 0)","Rows Removed by Filter":0`))), "residual filter"},
		{"a sibling CTE cannot lend its limit", "WITH wide AS MATERIALIZED (SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT 50001), probe AS MATERIALIZED (SELECT id FROM collaboration_events e WHERE e.organization_id=$1) SELECT 1", []any{"org"},
			reportTestNode("Result", "", reportTestNode("Limit", `"Parent Relationship":"InitPlan","Subplan Name":"CTE wide"`, reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "10", "1", outer)), reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "510", "1", `"Parent Relationship":"InitPlan","Subplan Name":"CTE probe"`)), "collaboration_events e (Index Scan) in scope \"probe\" has no proven bound"},
		{"a nested subquery cannot lend its limit", "SELECT e.id,(SELECT i.id FROM issues i WHERE i.organization_id=$1 ORDER BY i.id LIMIT 50001) FROM collaboration_events e WHERE e.organization_id=$1", []any{"org"},
			reportTestScan("Index Scan", "collaboration_events", "e", "events_window", reportTenantEvents, "900", "1", `"Output":["(SubPlan 1)"],"Plans":[`+reportTestNode("Limit", `"Parent Relationship":"SubPlan","Subplan Name":"SubPlan 1"`, reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "1", "900", outer))+`]`), "collaboration_events e (Index Scan) in scope \"\" has no proven bound"},
		{"missing tenant predicate", "SELECT id FROM issues i WHERE i.id=$1 LIMIT 1", []any{5},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_pkey", "(i.id = 5)", "1", "1", outer)), "no tenant equality"},
		{"a tenant predicate on another relation is not borrowed", "SELECT e.id FROM collaboration_events e WHERE e.id=(SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.id=1) LIMIT 1", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "collaboration_events", "e", "collaboration_events_pkey", "(e.id = (InitPlan 1).col1)", "1", "1", outer+`,"Plans":[`+reportTestScan("Index Scan", "issues", "i", "issues_pkey", "((i.organization_id = 'org'::text) AND (i.id = 1))", "1", "1", `"Parent Relationship":"InitPlan","Subplan Name":"InitPlan 1"`)+`]`)), "collaboration_events e (Index Scan) in scope \"\" has no tenant equality"},
		{"a missing limit argument", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT $3", []any{"org", 1},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "1", "1", outer)), "missing argument $3"},
		{"a negative limit", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT -1", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "1", "1", outer)), "LIMIT -1 is not a validated count"},
		{"limit all", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT ALL", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "1", "1", outer)), "LIMIT ALL is not a validated count"},
		{"an oversized limit", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT $2", []any{"org", 100001},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "1", "1", outer)), "LIMIT $2 is not a validated count between 1 and 50001"},
		{"a nonzero offset", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT 5 OFFSET 20", []any{"org"},
			reportTestNode("Limit", "", reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "25", "1", outer)), "OFFSET 20"},
		{"a limit inside a quoted string is not a limit", "SELECT id FROM issues i WHERE i.organization_id=$1 AND i.title<>'LIMIT 5' ORDER BY id", []any{"org"},
			reportTestScan("Index Scan", "issues", "i", "issues_state", reportTenantIssues, "500", "1", ""), "has no proven bound"},
		{"spill to disk", "SELECT id FROM issues i WHERE i.organization_id=$1 ORDER BY id LIMIT 5", []any{"org"},
			reportTestNode("Limit", "", reportTestNode("Sort", outer+`,"Sort Space Type":"Disk"`, reportTestNode("Function Scan", outer+","+reportCandidates))), "spilled to disk"},
		{"zero-match keyed probes", "SELECT c.id FROM unnest($2::bigint[]) c(id) LEFT JOIN collaboration_events e ON e.organization_id=$1 AND e.id=c.id", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Left"`, candidates, reportTestScan("Index Scan", "collaboration_events", "e", "collaboration_events_pkey", "((e.organization_id = 'org'::text) AND (e.id = c.id))", "0", "3", inner)), ""},
		{"whole-row loop averages on PostgreSQL 17", "SELECT c.id FROM unnest($2::bigint[]) c(id) JOIN collaboration_events e ON e.organization_id=$1 AND e.id=c.id", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestScan("Index Scan", "collaboration_events", "e", "collaboration_events_pkey", "((e.organization_id = 'org'::text) AND (e.id = c.id))", "1", "3", inner)), ""},
		{"fractional loop averages on PostgreSQL 18", "SELECT c.id FROM unnest($2::bigint[]) c(id) JOIN collaboration_events e ON e.organization_id=$1 AND e.id=c.id", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestScan("Index Scan", "collaboration_events", "e", "collaboration_events_pkey", "((e.organization_id = 'org'::text) AND (e.id = c.id))", "0.67", "3", inner)), ""},
		{"a keyed probe above its allowance", "SELECT c.id FROM unnest($2::bigint[]) c(id) JOIN collaboration_events e ON e.organization_id=$1 AND e.id=c.id", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestScan("Index Scan", "collaboration_events", "e", "collaboration_events_pkey", "((e.organization_id = 'org'::text) AND (e.id = c.id))", "1", "3", inner+`,"Rows Removed by Filter":2`)), "above its keyed allowance"},
		{"exists probes stop at the first match", "SELECT c.id,EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=$1 AND e.work_item_id=c.id) FROM unnest($2::bigint[]) c(id)", []any{"org", "{1,2,3}"},
			reportTestNode("Function Scan", reportCandidates+`,"Output":["c.id","EXISTS(SubPlan 1)"],"Plans":[`+reportTestScan("Index Only Scan", "collaboration_events", "e", "events_item", reportItemEvents, "1", "3", `"Parent Relationship":"SubPlan","Subplan Name":"SubPlan 1"`)+`]`), ""},
		{"ordered keyed probes multiply by their candidates", "SELECT c.id,latest.id FROM unnest($2::bigint[]) c(id) CROSS JOIN LATERAL (SELECT e.id FROM collaboration_events e WHERE e.organization_id=$1 AND e.work_item_id=c.id ORDER BY e.recorded_at DESC LIMIT 1) latest", []any{"org", "{1,2,3}"},
			reportTestNode("Nested Loop", `"Join Type":"Inner"`, candidates, reportTestNode("Limit", inner+`,"Actual Rows":1,"Actual Loops":3`, reportTestScan("Index Scan", "collaboration_events", "e", "events_item", reportItemEvents, "1", "3", outer))), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := CheckReportPlan([]byte(reportTestPlan(test.plan)), test.statement, test.args, reportTestContract)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(result.Problems, "\n")
			if test.want == "" && problems != "" || test.want != "" && !strings.Contains(problems, test.want) {
				t.Fatalf("problems=%q want %q", problems, test.want)
			}
		})
	}
}

func TestReportRequestBudget(t *testing.T) {
	probe := func(loops float64, work float64, rounding float64) ReportPlanResult {
		return ReportPlanResult{Scans: []ReportPlanScan{{Probe: true, Loops: loops, Work: work, Bound: loops, rounding: rounding}}, Work: work, Ceiling: 50001}
	}
	discovery := func(work float64) ReportPlanResult {
		return ReportPlanResult{Scans: []ReportPlanScan{{Work: work, Bound: 50001}}, Work: work, Ceiling: 50001}
	}
	budget := ReportRequestBudget{Overhead: 100, Probes: 2, Allowance: 1, Branches: 1}
	for _, test := range []struct {
		name       string
		results    []ReportPlanResult
		candidates int
		want       string
	}{
		{"keyed probes multiplied by relevant candidates", []ReportPlanResult{discovery(500), probe(500, 500, 0), probe(500, 500, 0)}, 500, ""},
		{"PostgreSQL 17 whole-row rounding is an explicit allowance", []ReportPlanResult{discovery(500), probe(500, 750, 0.5), probe(500, 750, 0.5)}, 500, ""},
		{"many individually acceptable siblings", []ReportPlanResult{probe(5, 5, 0), probe(5, 5, 0), probe(5, 5, 0)}, 5, "3 keyed probe scans above its budget of 2"},
		{"excessive candidate loops", []ReportPlanResult{discovery(10), probe(5000, 5000, 0)}, 10, "examined 5010 rows above"},
		{"archive growth with fixed candidates", []ReportPlanResult{discovery(5000), probe(500, 500, 0)}, 500, "examined 5500 rows above"},
		{"quadratic fanout", []ReportPlanResult{discovery(50), {Scans: []ReportPlanScan{{Probe: true, Loops: 50, Work: 50}}, Work: 50, Ceiling: 50001.0 * 50001}}, 50, "proven worst case"},
	} {
		t.Run(test.name, func(t *testing.T) {
			problems := strings.Join(ReportRequestProblems(test.results, budget, test.candidates, 50000), "\n")
			if test.want == "" && problems != "" || test.want != "" && !strings.Contains(problems, test.want) {
				t.Fatalf("problems=%q want %q", problems, test.want)
			}
		})
	}
}
