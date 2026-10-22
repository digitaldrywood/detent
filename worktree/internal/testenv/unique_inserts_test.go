package testenv

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestConstantUniqueInserts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `
CREATE TABLE principals (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, note TEXT);
CREATE TABLE grants (org TEXT NOT NULL, label TEXT NOT NULL, UNIQUE(org, label));`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source string
		want   int
	}{
		{"constant unique argument", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(id) DO NOTHING", id, "Service principal")`, 1},
		{"constant unique SQL literal", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,'Service principal')", id)`, 1},
		{"named constant", `db.ExecContext(ctx, "INSERT OR IGNORE INTO principals(id,name) VALUES(?,?)", id, principalName)`, 1},
		{"bound unique values", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?)", id, "Service principal "+id)`, 0},
		{"constant non-unique column", `db.ExecContext(ctx, "INSERT INTO principals(id,name,note) VALUES(?,?,'fixed')", id, name)`, 0},
		{"singleton upsert", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(id) DO NOTHING", "bootstrap", "Bootstrap")`, 0},
		{"conflict on the constant key", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET id=excluded.id", id, "Bootstrap")`, 0},
		{"composite key partly bound", `db.ExecContext(ctx, "INSERT INTO grants(org,label) VALUES(?,?)", org, "default")`, 0},
		{"composite key all constant", `db.ExecContext(ctx, "INSERT INTO grants(org,label) VALUES('org','default')")`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := "package fixture\n\nconst principalName = \"Service principal\"\n\nfunc insert() {\n\t" + tc.source + "\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			violations, err := ConstantUniqueInserts(t.Context(), db, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != tc.want {
				t.Fatalf("violations=%q want %d", violations, tc.want)
			}
		})
	}
}
