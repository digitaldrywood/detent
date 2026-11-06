package postgres_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func accessSQLiteCopy(t *testing.T, source postgres.AccessImportSource) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub-copy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range accessFixtureTables {
		type column struct {
			ordinal    int
			name       string
			kind       string
			ddl        string
			primary    int
			useDefault bool
		}
		var columns []column
		for name, definition := range source.Catalog.Tables["hub/"+table].Columns {
			var fields []any
			if err := json.Unmarshal([]byte(definition.Source), &fields); err != nil || len(fields) != 7 {
				t.Fatalf("invalid fixture column %s.%s", table, name)
			}
			kind := fields[2].(string)
			ddl := pgx.Identifier{name}.Sanitize() + " " + kind
			if fields[3].(float64) != 0 {
				ddl += " NOT NULL"
			}
			if fields[4] != nil {
				ddl += " DEFAULT " + fields[4].(string)
			}
			columns = append(columns, column{int(fields[0].(float64)), name, kind, ddl, int(fields[5].(float64)), fields[3].(float64) != 0 && fields[4] != nil})
		}
		slices.SortFunc(columns, func(a, b column) int { return a.ordinal - b.ordinal })
		var definitions, names, placeholders []string
		var primary []column
		for _, c := range columns {
			definitions = append(definitions, c.ddl)
			names = append(names, pgx.Identifier{c.name}.Sanitize())
			placeholders = append(placeholders, "?")
			if c.primary > 0 {
				primary = append(primary, c)
			}
		}
		slices.SortFunc(primary, func(a, b column) int { return a.primary - b.primary })
		var keys []string
		for _, c := range primary {
			keys = append(keys, pgx.Identifier{c.name}.Sanitize())
		}
		if len(keys) > 0 {
			definitions = append(definitions, "PRIMARY KEY ("+strings.Join(keys, ",")+")")
		}
		if _, err := db.ExecContext(t.Context(), "CREATE TABLE "+pgx.Identifier{table}.Sanitize()+" ("+strings.Join(definitions, ",")+")"); err != nil {
			t.Fatal(err)
		}
		rows, err := source.DB.QueryContext(t.Context(), "SELECT "+strings.Join(names, ",")+" FROM "+pgx.Identifier{source.Source.Schema}.Sanitize()+"."+pgx.Identifier{table}.Sanitize())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for n := range values {
				pointers[n] = &values[n]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for n, value := range values {
				if data, ok := value.([]byte); ok && columns[n].kind != "BLOB" {
					values[n] = string(data)
				}
			}
			var insertNames, insertPlaceholders []string
			var insertValues []any
			for n, value := range values {
				if value == nil && columns[n].useDefault {
					continue
				}
				insertNames = append(insertNames, names[n])
				insertPlaceholders = append(insertPlaceholders, placeholders[n])
				insertValues = append(insertValues, value)
			}
			if _, err := db.ExecContext(t.Context(), fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", pgx.Identifier{table}.Sanitize(), strings.Join(insertNames, ","), strings.Join(insertPlaceholders, ",")), insertValues...); err != nil {
				t.Fatal(err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}
