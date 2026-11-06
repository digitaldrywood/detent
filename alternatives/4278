package testenv

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

type SQLiteTableClassification struct {
	Ownership      string   `json:"ownership"`
	Scope          string   `json:"scope"`
	InlinePayloads []string `json:"inline_payloads"`
}

func SQLiteSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	inventory := make(map[string]string)
	for _, table := range sqliteInventoryRows(t, db, `SELECT name,type,ncol,wr,strict FROM pragma_table_list WHERE schema='main' ORDER BY name`) {
		name := fmt.Sprint(table[0])
		inventory["table/"+name] = sqliteInventoryJSON(t, table)
		for _, query := range []struct{ kind, sql string }{
			{"column", `SELECT cid,name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?) ORDER BY cid`},
			{"foreign_key", `SELECT id,seq,"table","from","to",on_update,on_delete,match FROM pragma_foreign_key_list(?) ORDER BY id,seq`},
			{"index", `SELECT name,"unique",origin,partial FROM pragma_index_list(?) ORDER BY name`},
		} {
			for _, row := range sqliteInventoryRows(t, db, query.sql, name) {
				key := fmt.Sprint(row[0])
				if query.kind == "column" {
					key = fmt.Sprint(row[1])
				}
				if query.kind == "foreign_key" {
					key += "/" + fmt.Sprint(row[1])
				}
				inventory[query.kind+"/"+name+"/"+key] = sqliteInventoryJSON(t, row)
				if query.kind == "index" {
					inventory["index_columns/"+name+"/"+key] = sqliteInventoryJSON(t, sqliteInventoryRows(t, db, `SELECT seqno,cid,name,desc,coll,key FROM pragma_index_xinfo(?) ORDER BY seqno`, key))
				}
			}
		}
	}
	for _, row := range sqliteInventoryRows(t, db, `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`) {
		inventory["definition/"+fmt.Sprint(row[0])+"/"+fmt.Sprint(row[1])] = sqliteInventoryJSON(t, row)
	}
	return inventory
}

func SQLiteStorageClasses(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, table := range sqliteInventoryRows(t, db, `SELECT name FROM pragma_table_list WHERE schema='main' AND type IN ('table','virtual') ORDER BY name`) {
		name := fmt.Sprint(table[0])
		for _, column := range sqliteInventoryRows(t, db, `SELECT name FROM pragma_table_xinfo(?) ORDER BY cid`, name) {
			field := fmt.Sprint(column[0])
			query := `SELECT typeof(` + sqliteIdentifier(field) + `),count(*) FROM ` + sqliteIdentifier(name) + ` GROUP BY 1 ORDER BY 1`
			result[name+"/"+field] = sqliteInventoryJSON(t, sqliteInventoryRows(t, db, query))
		}
	}
	return result
}

func CheckSQLiteInventory(t *testing.T, db *sql.DB, prefix string) {
	t.Helper()
	schema := SQLiteSchema(t, db)
	var classifications map[string]SQLiteTableClassification
	readSQLiteInventoryJSON(t, prefix+"_classification.json", &classifications)
	if problems := SQLiteClassificationProblems(schema, classifications); len(problems) != 0 {
		t.Error(strings.Join(problems, "\n"))
	}
	for _, snapshot := range []struct {
		name string
		got  map[string]string
	}{
		{"schema", schema},
		{"storage_classes", SQLiteStorageClasses(t, db)},
	} {
		var want map[string]string
		readSQLiteInventoryJSON(t, prefix+"_"+snapshot.name+".json", &want)
		for _, problem := range SQLiteInventoryDifferences(snapshot.got, want) {
			t.Errorf("%s: %s", snapshot.name, problem)
		}
	}
}

func SQLiteInventoryDifferences(got, want map[string]string) []string {
	var problems []string
	for key, value := range got {
		previous, ok := want[key]
		switch {
		case !ok:
			problems = append(problems, key+": unclassified schema or sample")
		case value != previous:
			problems = append(problems, fmt.Sprintf("%s: got %s; classified as %s", key, value, previous))
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			problems = append(problems, key+": stale classification")
		}
	}
	slices.Sort(problems)
	return problems
}

func SQLiteClassificationProblems(schema map[string]string, classifications map[string]SQLiteTableClassification) []string {
	var problems []string
	for key := range schema {
		name, table := strings.CutPrefix(key, "table/")
		if !table {
			continue
		}
		classification, ok := classifications[name]
		if !ok {
			problems = append(problems, name+": unclassified table")
			continue
		}
		if !slices.Contains([]string{"organization-owned", "global", "platform"}, classification.Ownership) || strings.TrimSpace(classification.Scope) == "" || classification.InlinePayloads == nil {
			problems = append(problems, name+": incomplete ownership or payload classification")
		}
		for _, column := range classification.InlinePayloads {
			if _, ok := schema["column/"+name+"/"+column]; !ok {
				problems = append(problems, name+"/"+column+": stale inline payload classification")
			}
		}
	}
	for name := range classifications {
		if _, ok := schema["table/"+name]; !ok {
			problems = append(problems, name+": stale table classification")
		}
	}
	slices.Sort(problems)
	return problems
}

func sqliteInventoryRows(t *testing.T, db *sql.DB, query string, args ...any) [][]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([][]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func sqliteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func sqliteInventoryJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readSQLiteInventoryJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
