package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

type tableLifecycle struct {
	Class     string `json:"class"`
	Retention string `json:"retention,omitempty"`
}

type tableLifecyclePolicy struct {
	Tables         map[string]tableLifecycle `json:"tables"`
	LegacyPayloads map[string]string         `json:"legacy_payloads"`
}

func readTableLifecyclePolicy(t *testing.T) tableLifecyclePolicy {
	t.Helper()
	data, err := os.ReadFile("../invariants/table_lifecycle_policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy tableLifecyclePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func tableLifecycleDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return postgresStartupTemplate.Database(t)
}

func tableLifecycleSchema(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT CASE WHEN n.nspname='public' THEN c.relname ELSE n.nspname || '.' || c.relname END,a.attname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
AND c.relkind IN ('r','p','f','m') ORDER BY n.nspname,c.relname,a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	schema := make(map[string][]string)
	for rows.Next() {
		var table string
		var column sql.NullString
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		if _, exists := schema[table]; !exists {
			schema[table] = nil
		}
		if column.Valid {
			schema[table] = append(schema[table], column.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return schema
}

func checkTableLifecycles(schema map[string][]string, policy tableLifecyclePolicy) []string {
	var problems []string
	debt := make(map[string]bool)
	for table, columns := range schema {
		lifecycle, declared := policy.Tables[table]
		if !declared {
			problems = append(problems, table+": undeclared application table")
			continue
		}
		switch lifecycle.Class {
		case "hot_state", "index":
			continue
		case "derived_with_retention":
			if strings.TrimSpace(lifecycle.Retention) == "" {
				problems = append(problems, table+": derived table has no retention contract")
			}
		case "immutable_payload":
		default:
			problems = append(problems, fmt.Sprintf("%s: unknown lifecycle class %q", table, lifecycle.Class))
			continue
		}
		for _, column := range columns {
			name := strings.ToLower(column)
			if !strings.HasSuffix(name, "_json") && name != "text" && name != "bundle" && name != "body" && name != "patch" && name != "artifact_ref" {
				continue
			}
			key := table + "." + column
			family, legacy := policy.LegacyPayloads[key]
			if !legacy {
				problems = append(problems, key+": inline history payload is not on the frozen legacy debt list")
				continue
			}
			debt[key] = true
			if strings.TrimSpace(family) == "" {
				problems = append(problems, key+": legacy payload has no owning conversion family")
			}
		}
	}
	for table := range policy.Tables {
		if _, exists := schema[table]; !exists {
			problems = append(problems, table+": remove stale lifecycle declaration")
		}
	}
	for key := range policy.LegacyPayloads {
		if !debt[key] {
			problems = append(problems, key+": remove stale legacy payload debt")
		}
	}
	slices.Sort(problems)
	return problems
}

func TestINV20HubTableLifecycles(t *testing.T) {
	t.Parallel()
	db := tableLifecycleDatabase(t)
	for _, problem := range checkTableLifecycles(tableLifecycleSchema(t, db), readTableLifecyclePolicy(t)) {
		t.Error(problem)
	}
}

func TestTableLifecycleViolations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		statement string
		mutate    func(tableLifecyclePolicy)
		want      string
	}{
		{name: "unknown JSON table", statement: `CREATE TABLE example_history(id INTEGER, payload_json TEXT)`, want: "example_history: undeclared application table"},
		{name: "empty table", statement: `CREATE TABLE example_empty()`, want: "example_empty: undeclared application table"},
		{name: "platform table", statement: `CREATE TABLE platform.example_state(id INTEGER)`, want: "platform.example_state: undeclared application table"},
		{name: "SQLite prefix is not an exemption", statement: `CREATE TABLE sqlite_example(id INTEGER)`, want: "sqlite_example: undeclared application table"},
		{name: "unknown typed table", statement: `CREATE TABLE example_state(id INTEGER)`, want: "example_state: undeclared application table"},
		{name: "FTS prefix is not an exemption", statement: `CREATE TABLE native_work_fts_example(id INTEGER)`, want: "native_work_fts_example: undeclared application table"},
		{name: "declaring history does not allow inline payloads", statement: `CREATE TABLE example_history(id INTEGER, payload_json TEXT)`, mutate: func(p tableLifecyclePolicy) {
			p.Tables["example_history"] = tableLifecycle{Class: "immutable_payload"}
		}, want: "example_history.payload_json: inline history payload"},
		{name: "new object reference table", statement: `CREATE TABLE example_history(id INTEGER, object_key TEXT, sha256 TEXT)`, mutate: func(p tableLifecyclePolicy) {
			p.Tables["example_history"] = tableLifecycle{Class: "immutable_payload"}
		}},
		{name: "new history JSON column", statement: `ALTER TABLE native_attempt_events ADD COLUMN payload_json TEXT`, want: "native_attempt_events.payload_json: inline history payload"},
		{name: "new history text column", statement: `ALTER TABLE native_attempt_events ADD COLUMN text TEXT`, want: "native_attempt_events.text: inline history payload"},
		{name: "new history bundle column", statement: `ALTER TABLE native_attempt_events ADD COLUMN bundle BYTEA`, want: "native_attempt_events.bundle: inline history payload"},
		{name: "uppercase payload column", statement: `ALTER TABLE native_attempt_events ADD COLUMN "EXTRA_JSON" TEXT`, want: "native_attempt_events.EXTRA_JSON: inline history payload"},
		{name: "existing debt cannot cover another column", statement: `ALTER TABLE native_attempts ADD COLUMN extra_json TEXT`, want: "native_attempts.extra_json: inline history payload"},
		{name: "new derived payload column", statement: `ALTER TABLE report_attempt_facts ADD COLUMN extra_json TEXT`, want: "report_attempt_facts.extra_json: inline history payload"},
		{name: "missing debt owner", mutate: func(p tableLifecyclePolicy) { p.LegacyPayloads["conversation_messages.text"] = "" }, want: "conversation_messages.text: legacy payload has no owning conversion family"},
		{name: "unlisted legacy payload", mutate: func(p tableLifecyclePolicy) { delete(p.LegacyPayloads, "conversation_messages.text") }, want: "conversation_messages.text: inline history payload"},
		{name: "stale debt must shrink", mutate: func(p tableLifecyclePolicy) { p.LegacyPayloads["conversation_messages.removed_json"] = "sources" }, want: "conversation_messages.removed_json: remove stale legacy payload debt"},
		{name: "unknown lifecycle", mutate: func(p tableLifecyclePolicy) { p.Tables["native_attempt_events"] = tableLifecycle{Class: "history"} }, want: "native_attempt_events: unknown lifecycle class"},
		{name: "derived table requires retention", mutate: func(p tableLifecyclePolicy) {
			p.Tables["report_attempt_facts"] = tableLifecycle{Class: "derived_with_retention"}
		}, want: "report_attempt_facts: derived table has no retention contract"},
		{name: "stale declaration", mutate: func(p tableLifecyclePolicy) { p.Tables["removed_table"] = tableLifecycle{Class: "hot_state"} }, want: "removed_table: remove stale lifecycle declaration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db := tableLifecycleDatabase(t)
			if test.statement != "" {
				if _, err := db.ExecContext(t.Context(), test.statement); err != nil {
					t.Fatal(err)
				}
			}
			policy := readTableLifecyclePolicy(t)
			if test.mutate != nil {
				test.mutate(policy)
			}
			problems := checkTableLifecycles(tableLifecycleSchema(t, db), policy)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("unexpected lifecycle violations: %v", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], test.want) {
				t.Fatalf("lifecycle violations = %v, want %q", problems, test.want)
			}
		})
	}
}
