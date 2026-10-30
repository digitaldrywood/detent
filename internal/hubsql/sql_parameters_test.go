package hubsql

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openSQLParameterTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parameters.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestSQLParameterRelations(t *testing.T) {
	db := openSQLParameterTestDatabase(t)
	if _, err := db.ExecContext(t.Context(), `CREATE TEMP TABLE parameter_entities(organization TEXT,project TEXT,id TEXT,PRIMARY KEY(organization,project,id));
INSERT INTO parameter_entities VALUES('scope_a','project_a','first'),('scope_a','project_a','second'),('scope_a','project_b','first'),('scope_a','project_b','second'),('scope_b','project_a','first'),('scope_b','project_a','second');`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, operation string
		values          []string
		want            []string
	}{
		{"two entities", "IN", []string{"first", "second"}, []string{"first", "second"}},
		{"nil selection", "IN", nil, nil},
		{"empty selection", "IN", []string{}, nil},
		{"empty exclusions", "NOT IN", []string{}, []string{"first", "second"}},
		{"complete exclusion set", "NOT IN", []string{"second", "outside"}, []string{"first"}},
		{"duplicates retain membership semantics", "IN", []string{"second", "second"}, []string{"second"}},
		{"quoted input cannot escape scope", "IN", []string{"first') OR 1=1 --"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			list, err := List(test.values)
			if err != nil {
				t.Fatal(err)
			}
			statement := "SELECT id FROM parameter_entities WHERE organization=? AND project=? AND id " + test.operation + " (SELECT value FROM sql_rows(?)) ORDER BY id"
			rows, err := Query(t.Context(), db, statement, "scope_a", "project_a", list)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("entities=%v want=%v", got, test.want)
			}
			expanded, args, err := Bind(statement, []any{"scope_a", "project_a", list})
			if err != nil {
				t.Fatal(err)
			}
			plan := explainQueryPlan(t, db, expanded, args)
			if !strings.Contains(strings.Join(plan, "\n"), "SEARCH parameter_entities USING COVERING INDEX") {
				t.Fatalf("scope index lost: %v", plan)
			}
		})
	}
	tuples, err := TypedRows([]string{"id", "cutoff"}, [][]any{{"first", int64(11)}, {"second", int64(29)}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Query(t.Context(), db, "SELECT e.id,c.cutoff FROM sql_rows(?) c JOIN parameter_entities e ON e.organization=? AND e.project=? AND e.id=c.id ORDER BY c.key", tuples, "scope_a", "project_a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var cutoff int64
		if err := rows.Scan(&id, &cutoff); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d", id, cutoff))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"first:11", "second:29"}) {
		t.Fatalf("typed tuples=%v", got)
	}
}

func TestSQLParameterBinding(t *testing.T) {
	for _, test := range []struct {
		name, statement string
		args            []any
		wantArgs        []any
		wantSQL         string
		invalid         bool
	}{
		{name: "numbered reuse", statement: "SELECT ?2,?1,?2", args: []any{"first", "second"}, wantArgs: []any{"second", "first", "second"}, wantSQL: "SELECT ?,?,?"},
		{name: "quoted and commented markers", statement: "SELECT 'sql_rows(?)',? /* ? */ -- ?\n", args: []any{int64(2)}, wantArgs: []any{int64(2)}, wantSQL: "SELECT 'sql_rows(?)',? /* ? */ -- ?\n"},
		{name: "missing parameter", statement: "SELECT ?", invalid: true},
		{name: "unused parameter", statement: "SELECT 1", args: []any{1}, invalid: true},
		{name: "zero parameter index", statement: "SELECT ?0", args: []any{1}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			statement, args, err := Bind(test.statement, test.args)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid parameters accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if statement != test.wantSQL || !reflect.DeepEqual(args, test.wantArgs) {
				t.Fatalf("bound statement=%q args=%v", statement, args)
			}
		})
	}
	if _, err := List(make([]string, MaxParameters+1)); err == nil {
		t.Fatal("unbounded list accepted")
	}
	list, err := List(make([]int64, MaxParameters))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Bind("SELECT ? FROM sql_rows(?)", []any{1, list}); err == nil {
		t.Fatal("combined parameters exceed the statement bound")
	}
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
