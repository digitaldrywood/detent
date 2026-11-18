package pgstoretest

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

func assertActivityMetadataPlans(t *testing.T, db *sql.DB, organization string, statements []ReportStatement) {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, seed := range []string{
		`INSERT INTO organizations (organization_id,name,local,created_at)
		 SELECT $1||'-decoy-'||n,'Plan decoy '||n,false,now() FROM generate_series(1,1024) n`,
		`INSERT INTO projects (organization_id,id,name,profile)
		 SELECT $1||'-decoy-'||n,p.id,p.name,'native' FROM projects p CROSS JOIN generate_series(1,1024) n WHERE p.organization_id=$1`,
		`INSERT INTO project_workflow_states (organization_id,project_id,name,terminal,dispatchable,board_rank)
		 SELECT $1||'-decoy-'||n,w.project_id,w.name,w.terminal,w.dispatchable,w.board_rank FROM project_workflow_states w CROSS JOIN generate_series(1,1024) n WHERE w.organization_id=$1`,
		`INSERT INTO provider_reservations (organization_id,lease_id,requirement_role,requirement_backend,requirement_model,provider,backend,account_alias,billing,max_concurrent,availability,observed_at,reason)
		 SELECT organization_id,lease_id,'code','example','example','example','example','archive','example',1,'available',acquired_at,'retained fixture'
		 FROM leases WHERE organization_id=$1 AND released_at IS NOT NULL AND lease_id LIKE '%\_h%' LIMIT 10000`,
		`INSERT INTO report_event_facts (organization_id,event_id)
		 SELECT organization_id,id FROM collaboration_events WHERE organization_id=$1 AND id LIKE '%\_h1' LIMIT 10000 ON CONFLICT DO NOTHING`,
	} {
		result, err := tx.ExecContext(ctx, seed, organization)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := result.RowsAffected(); err != nil || n < 1024 {
			t.Fatalf("activity metadata fixture inserted %d rows: %v", n, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "ANALYZE projects,project_workflow_states,leases,provider_reservations,report_event_facts"); err != nil {
		t.Fatal(err)
	}
	indexes, err := LoadReportIndexes(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	checkActivityMetadataScans(t, tx, organization, statements, indexes)
	checkActivityMetadataScans(t, tx, organization, statements, indexes, "geqo_threshold=2", "geqo_pool_size=2", "geqo_generations=1", "geqo_seed=0.3529")
}

func checkActivityMetadataScans(t *testing.T, tx *sql.Tx, organization string, statements []ReportStatement, indexes map[string]ReportIndex, settings ...string) {
	t.Helper()
	required := map[string]bool{
		"projects": false, "project_workflow_states": false, "leases": false, "runner_lease_assignments": false,
		"provider_reservations": false, "report_attempt_facts": false, "report_event_facts": false,
	}
	for _, statement := range statements {
		var candidates int
		switch {
		case strings.Contains(statement.Query, "AS wanted(id)"):
			candidates = ReportCandidateCount(statement.Args[1:2])
		case strings.Contains(statement.Query, "AS recent(id)"):
			candidates = ReportCandidateCount(statement.Args[2:3])
		default:
			continue
		}
		var relations []string
		for relation := range required {
			if strings.Contains(statement.Query, relation) {
				relations = append(relations, relation)
			}
		}
		raw, err := explainReportPlanTx(t.Context(), tx, organization, statement, settings...)
		if err != nil {
			t.Fatal(err)
		}
		result, err := CheckReportPlan(raw, statement.Query, statement.Args, ReportPlanContract{Relations: relations, Indexes: indexes, Cap: ReportCandidateCap})
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range result.Problems {
			t.Errorf("activity metadata %v: %s", settings, problem)
		}
		probes := map[string]float64{}
		for _, scan := range result.Scans {
			required[scan.Relation] = true
			t.Logf("activity metadata %v: %s index=%s loops=%.0f candidates=%d", settings, scan.Relation, scan.Index, scan.Loops, candidates)
			probes[scan.Relation] += scan.Loops
			if !activityPrimaryKeyProbe(scan, indexes) || probes[scan.Relation] > float64(candidates) {
				t.Errorf("activity %s used %s %s with %.0f loops and %.0f rows examined for %d candidates", scan.Relation, scan.Class, scan.Node, probes[scan.Relation], scan.Work, candidates)
			}
		}
	}
	for relation, seen := range required {
		if !seen {
			t.Errorf("activity metadata plan did not exercise %s", relation)
		}
	}
}

func activityPrimaryKeyProbe(scan ReportPlanScan, indexes map[string]ReportIndex) bool {
	if scan.Node != "Index Scan" && scan.Node != "Index Only Scan" {
		return false
	}
	primary := indexes[scan.Relation+"_pkey"]
	chosen := indexes[scan.Index].Columns
	if !primary.Unique || len(primary.Columns) == 0 || len(chosen) < len(primary.Columns) || !slices.Equal(chosen[:len(primary.Columns)], primary.Columns) {
		return false
	}
	for _, column := range primary.Columns {
		if !strings.Contains(scan.indexCond, "("+scan.Alias+"."+column+" = ") {
			return false
		}
	}
	return !strings.Contains(scan.indexCond, "ANY (")
}
