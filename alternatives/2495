package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "hub.db")
	seedHubDatabaseTemplate(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableLifecycleSchema(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT t.name,c.name FROM pragma_table_list() t
JOIN pragma_table_xinfo(t.name) c
WHERE t.schema='main' AND t.type IN ('table','virtual') ORDER BY t.name,c.cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	schema := make(map[string][]string)
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(table, "sqlite_") {
			schema[table] = append(schema[table], column)
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
		{name: "new history bundle column", statement: `ALTER TABLE native_attempt_events ADD COLUMN bundle BLOB`, want: "native_attempt_events.bundle: inline history payload"},
		{name: "uppercase payload column", statement: `ALTER TABLE native_attempt_events ADD COLUMN EXTRA_JSON TEXT`, want: "native_attempt_events.EXTRA_JSON: inline history payload"},
		{name: "existing debt cannot cover another column", statement: `ALTER TABLE native_attempts ADD COLUMN extra_json TEXT`, want: "native_attempts.extra_json: inline history payload"},
		{name: "new derived payload column", statement: `ALTER TABLE analytics_hourly_rollups ADD COLUMN extra_json TEXT`, want: "analytics_hourly_rollups.extra_json: inline history payload"},
		{name: "missing debt owner", mutate: func(p tableLifecyclePolicy) { p.LegacyPayloads["change_sources.bundle"] = "" }, want: "change_sources.bundle: legacy payload has no owning conversion family"},
		{name: "unlisted legacy payload", mutate: func(p tableLifecyclePolicy) { delete(p.LegacyPayloads, "change_sources.bundle") }, want: "change_sources.bundle: inline history payload"},
		{name: "stale debt must shrink", mutate: func(p tableLifecyclePolicy) { p.LegacyPayloads["change_sources.removed_json"] = "sources" }, want: "change_sources.removed_json: remove stale legacy payload debt"},
		{name: "unknown lifecycle", mutate: func(p tableLifecyclePolicy) { p.Tables["native_attempt_events"] = tableLifecycle{Class: "history"} }, want: "native_attempt_events: unknown lifecycle class"},
		{name: "derived table requires retention", mutate: func(p tableLifecyclePolicy) {
			p.Tables["analytics_hourly_rollups"] = tableLifecycle{Class: "derived_with_retention"}
		}, want: "analytics_hourly_rollups: derived table has no retention contract"},
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
