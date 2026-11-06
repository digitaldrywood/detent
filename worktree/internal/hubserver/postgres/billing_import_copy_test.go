package postgres_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type importCopyColumn struct {
	Ordinal int
	Name    string
	Type    string
	NotNull int
	Default any
	Primary int
	Hidden  int
}

type billingImportCopy struct {
	path string
	db   *sql.DB
}

func newBillingImportCopy(t *testing.T, catalog storagecatalog.Catalog, tables ...string) billingImportCopy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub-copy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	copy := billingImportCopy{path: path, db: db}
	for _, name := range tables {
		definition, ok := catalog.Tables["hub/"+name]
		if !ok {
			t.Fatalf("missing catalog table %s", name)
		}
		columns := make([]importCopyColumn, 0, len(definition.Columns))
		for _, column := range definition.Columns {
			var parsed importCopyColumn
			var fields []json.RawMessage
			if err := json.Unmarshal([]byte(column.Source), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 7 {
				t.Fatalf("source signature fields=%d", len(fields))
			}
			for index, field := range fields {
				var err error
				switch index {
				case 0:
					err = json.Unmarshal(field, &parsed.Ordinal)
				case 1:
					err = json.Unmarshal(field, &parsed.Name)
				case 2:
					err = json.Unmarshal(field, &parsed.Type)
				case 3:
					err = json.Unmarshal(field, &parsed.NotNull)
				case 4:
					err = json.Unmarshal(field, &parsed.Default)
				case 5:
					err = json.Unmarshal(field, &parsed.Primary)
				case 6:
					err = json.Unmarshal(field, &parsed.Hidden)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			columns = append(columns, parsed)
		}
		for left := range columns {
			for right := left + 1; right < len(columns); right++ {
				if columns[right].Ordinal < columns[left].Ordinal {
					columns[left], columns[right] = columns[right], columns[left]
				}
			}
		}
		var ddl, primary []string
		for _, column := range columns {
			statement := `"` + strings.ReplaceAll(column.Name, `"`, `""`) + `" ` + column.Type
			if column.NotNull == 1 {
				statement += " NOT NULL"
			}
			if column.Default != nil {
				statement += " DEFAULT " + fmt.Sprint(column.Default)
			}
			ddl = append(ddl, statement)
			if column.Primary > 0 {
				if len(primary) < column.Primary {
					primary = append(primary, make([]string, column.Primary-len(primary))...)
				}
				primary[column.Primary-1] = `"` + strings.ReplaceAll(column.Name, `"`, `""`) + `"`
			}
		}
		if len(primary) == 1 && strings.HasPrefix(columns[primaryPosition(columns, primary[0])].Type, "INTEGER") && columns[primaryPosition(columns, primary[0])].NotNull == 0 {
			ddl[primaryPosition(columns, primary[0])] += " PRIMARY KEY"
		} else if len(primary) > 0 {
			ddl = append(ddl, "PRIMARY KEY ("+strings.Join(primary, ",")+")")
		}
		if _, err := db.Exec(`CREATE TABLE "` + strings.ReplaceAll(name, `"`, `""`) + `" (` + strings.Join(ddl, ",") + ")"); err != nil {
			t.Fatalf("create %s source table: %v", name, err)
		}
	}
	t.Cleanup(func() { _ = copy.db.Close() })
	return copy
}

func primaryPosition(columns []importCopyColumn, quoted string) int {
	name := strings.Trim(quoted, `"`)
	for i, column := range columns {
		if column.Name == name {
			return i
		}
	}
	return 0
}

func (c billingImportCopy) seal(t *testing.T) string {
	t.Helper()
	if err := c.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.path, 0o400); err != nil {
		t.Fatal(err)
	}
	return c.path
}
