package hubserver

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

func TestPortableTimeIndexMigration(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	f.create(t, "retained history")
	now := formatHubTime(time.Now())
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at) VALUES(?,'session','actor','actor','','action','','',200,?,?,?)`, f.project.OrganizationID, now, now, now); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("migrations/20261021143500_portable_time_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(raw), "-- +goose Down")
	if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM hub_schema_version WHERE version_id=20261021143500`); err != nil {
		t.Fatal(err)
	}

	for _, migration := range []string{"20261006173000_health_findings.sql", "20261007220536_hosted_audit_retention.sql"} {
		raw, err := migrationFiles.ReadFile("migrations/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		for _, statement := range strings.Split(up, ";") {
			statement = strings.TrimSpace(strings.TrimPrefix(statement, "-- +goose Up"))
			if strings.HasPrefix(statement, "CREATE INDEX health_decision_window") || strings.HasPrefix(statement, "CREATE INDEX health_landing_window") || strings.HasPrefix(statement, "CREATE INDEX hosted_audit_retention_idx") {
				if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at)
 SELECT ?,'session','actor','actor','','action','','',200,?,?,? FROM n`, commandReceiptPruneBatch, f.project.OrganizationID, now, now, now); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var statements []string
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) { statements = append(statements, query) }})
	if _, err := runMigrations(ctx, f.service.database.db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		upper := strings.ToUpper(statement)
		if strings.Contains(upper, "CREATE INDEX") && (strings.Contains(upper, " ON COLLABORATION_EVENTS(") || strings.Contains(upper, " ON HOSTED_AUDIT(")) {
			t.Fatalf("startup indexes populated owner: %s", statement)
		}
		if (strings.HasPrefix(upper, "UPDATE ") || strings.HasPrefix(upper, "INSERT ") || strings.HasPrefix(upper, "SELECT ")) && (strings.Contains(upper, "COLLABORATION_EVENTS") || strings.Contains(upper, "HOSTED_AUDIT")) {
			t.Fatalf("startup scans or populates owner: %s", statement)
		}
	}
	var after, health, audit, progress, typed int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM collaboration_events),(SELECT count(*) FROM health_event_times),(SELECT count(*) FROM hosted_audit_times),(SELECT count(*) FROM time_index_progress),(SELECT count(*) FROM hosted_audit WHERE recorded_at_ns IS NOT NULL)`).Scan(&after, &health, &audit, &progress, &typed); err != nil {
		t.Fatal(err)
	}
	if before != after || health != 0 || audit != 0 || progress != 0 || typed != 0 {
		t.Fatalf("startup populated existing rows: before=%d after=%d health=%d audit=%d progress=%d typed=%d", before, after, health, audit, progress, typed)
	}
	if err := f.service.database.populateTimeIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times`).Scan(&audit); err != nil || audit != commandReceiptPruneBatch {
		t.Fatalf("audit population=%d err=%v", audit, err)
	}
	var complete, old int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT complete FROM time_index_progress WHERE owner='audit'),(SELECT count(*) FROM sqlite_schema WHERE name='hosted_audit_retention_idx')`).Scan(&complete, &old); err != nil || complete != 0 || old != 1 {
		t.Fatalf("partial population complete=%d old index=%d err=%v", complete, old, err)
	}
	if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.populateTimeIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_audit_times),(SELECT complete FROM time_index_progress WHERE owner='audit'),(SELECT count(*) FROM sqlite_schema WHERE name IN ('hosted_audit_retention_idx','health_decision_window','health_landing_window'))`).Scan(&audit, &complete, &old); err != nil || audit != commandReceiptPruneBatch+1 || complete != 1 || old != 0 {
		t.Fatalf("resumed population audit=%d complete=%d old indexes=%d err=%v", audit, complete, old, err)
	}
}
