package pgstoretest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

const ReportNeighborOrganization = "org_report_neighbor"

type reportNeighborTable struct {
	relation, where string
	keys            map[string]string
}

var reportNeighborTables = []reportNeighborTable{
	{"projects", "id=$3", nil},
	{"project_workflow_states", "project_id=$3", nil},
	{"issues", "project_id=$3 AND native_id=$4", nil},
	{"change_requests", "project_id=$3", map[string]string{"id": "suffix", "work_item_id": "=$4", "current_version_id": "suffix", "landed_version_id": "suffix"}},
	{"change_versions", "change_id IN (SELECT id FROM change_requests WHERE organization_id=$1 AND project_id=$3)", map[string]string{"id": "suffix", "change_id": "suffix", "run_id": "suffix", "attempt_id": "suffix"}},
	{"report_landing_attempts", "change_id IN (SELECT id FROM change_requests WHERE organization_id=$1 AND project_id=$3)", map[string]string{"change_id": "suffix"}},
	{"report_issue_state_entries", "project_id=$3", map[string]string{"work_item_id": "=$4", "state": "=state||'_'||work_item_id||'_n'||neighbor_copy"}},
}

func FreezeReportPlanner(t testing.TB, db *sql.DB, relations []string) {
	t.Helper()
	joined := strings.Join(relations, ",")
	statements := make([]string, 0, len(relations)+3)
	for _, relation := range relations {
		statements = append(statements, "ALTER TABLE "+relation+" SET (autovacuum_enabled=false, toast.autovacuum_enabled=false)")
	}
	statements = append(statements, "VACUUM (FREEZE, INDEX_CLEANUP ON) "+joined, "VACUUM (ANALYZE) "+joined, `DO $$DECLARE lagging text; BEGIN SELECT string_agg(relname||' '||relallvisible||'/'||relpages,',') INTO lagging FROM pg_class WHERE oid=ANY(string_to_array('`+joined+`',',')::regclass[]) AND relallvisible<relpages; IF lagging IS NOT NULL THEN RAISE EXCEPTION 'planner inputs not all-visible after VACUUM: %', lagging; END IF; END$$`)
	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func SeedNeighborReportRows(t testing.TB, db *sql.DB, organization string, copies int) {
	t.Helper()
	if err := seedNeighborReportRows(t.Context(), db, organization, copies); err != nil {
		t.Fatal(err)
	}
}

func seedNeighborReportRows(ctx context.Context, db *sql.DB, organization string, copies int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin neighbor report rows: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return fmt.Errorf("skip neighbor report triggers: %w", err)
	}
	var project, item string
	if err := tx.QueryRowContext(ctx, `SELECT project_id,native_id FROM issues WHERE organization_id=$1 ORDER BY id LIMIT 1`, organization).Scan(&project, &item); err != nil {
		return fmt.Errorf("read neighbor report source: %w", err)
	}
	for _, table := range reportNeighborTables {
		columns, values, err := reportNeighborColumns(ctx, tx, table)
		if err != nil {
			return err
		}
		repeat := 1
		if table.keys != nil {
			repeat = copies
		}
		statement := fmt.Sprintf(`INSERT INTO %s(%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s CROSS JOIN generate_series(1,%d) neighbor_copy WHERE organization_id=$1 AND $3::text<>'' AND $4::text<>'' AND %s`, table.relation, strings.Join(columns, ","), strings.Join(values, ","), table.relation, repeat, table.where)
		if _, err := tx.ExecContext(ctx, statement, organization, ReportNeighborOrganization, project, item); err != nil {
			return fmt.Errorf("copy neighbor %s rows: %w", table.relation, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO change_evidence(organization_id,project_id,change_id,sequence,kind,source_key,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT c.organization_id,c.project_id,c.id,1,'discussion',c.id,c.created_at,c.payload_object_key,c.payload_sha256,c.payload_bytes,c.payload_content_bytes FROM change_requests c WHERE c.organization_id=$1`, ReportNeighborOrganization); err != nil {
		return fmt.Errorf("insert neighbor change_evidence rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO report_escape_facts(organization_id,change_id,evidence_sequence,occurrence_id,kind,cause,observed_at)
SELECT e.organization_id,e.change_id,e.sequence,e.change_id,'reopened','missing_criterion',e.created_at FROM change_evidence e WHERE e.organization_id=$1`, ReportNeighborOrganization); err != nil {
		return fmt.Errorf("insert neighbor report_escape_facts rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit neighbor report rows: %w", err)
	}
	return nil
}

func reportNeighborColumns(ctx context.Context, tx *sql.Tx, table reportNeighborTable) ([]string, []string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND is_generated='NEVER' ORDER BY ordinal_position`, table.relation)
	if err != nil {
		return nil, nil, fmt.Errorf("read neighbor %s columns: %w", table.relation, err)
	}
	defer rows.Close()
	var columns, values []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, fmt.Errorf("scan neighbor %s column: %w", table.relation, err)
		}
		value := name
		switch {
		case name == "organization_id":
			value = "$2"
		case name == "payload_object_key":
			value = "'orgs/'||$2||'/blobs/sha256/'||substr(payload_sha256,1,2)||'/'||payload_sha256"
		case name == "landed_object_key":
			value = "CASE WHEN landed_object_key='' THEN '' ELSE 'orgs/'||$2||'/blobs/sha256/'||substr(landed_sha256,1,2)||'/'||landed_sha256 END"
		case strings.HasPrefix(table.keys[name], "="):
			value = strings.TrimPrefix(table.keys[name], "=")
		case table.keys[name] == "suffix":
			value = fmt.Sprintf("CASE WHEN %s IS NULL OR %s='' THEN %s ELSE %s||'_n'||neighbor_copy END", name, name, name, name)
		}
		columns, values = append(columns, name), append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read neighbor %s columns: %w", table.relation, err)
	}
	return columns, values, nil
}
