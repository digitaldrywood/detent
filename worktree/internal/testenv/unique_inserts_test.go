package testenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConstantUniqueInserts(t *testing.T) {
	db := testPostgresTemplate.Database(t)
	for _, tc := range []struct {
		name   string
		source string
		want   int
	}{
		{"constant unique argument", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(id) DO NOTHING", id, "Service principal")`, 1},
		{"constant unique SQL literal", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,'Service principal')", id)`, 1},
		{"named constant", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2) ON CONFLICT(id) DO NOTHING", id, principalName)`, 1},
		{"bound unique values", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?)", id, "Service principal "+id)`, 0},
		{"constant non-unique column", `db.ExecContext(ctx, "INSERT INTO principals(id,name,note) VALUES(?,?,'fixed')", id, name)`, 0},
		{"singleton upsert", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(id) DO NOTHING", "bootstrap", "Bootstrap")`, 0},
		{"conflict on the constant key", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET id=excluded.id", id, "Bootstrap")`, 0},
		{"composite key partly bound", `db.ExecContext(ctx, "INSERT INTO grants(org,label) VALUES(?,?)", org, "default")`, 0},
		{"composite key all constant", `db.ExecContext(ctx, "INSERT INTO grants(org,label) VALUES('org','default')")`, 1},
		{"numbered unique argument", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2)", id, "Service principal")`, 1},
		{"reordered numbered arguments", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($2,$1)", "Service principal", id)`, 1},
		{"reused numbered argument", `db.ExecContext(ctx, "INSERT INTO grants(org,label) VALUES($1,$1)", "default")`, 1},
		{"numbered cast", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2::text)", id, "Service principal")`, 1},
		{"numbered dynamic value", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2)", id, name)`, 0},
		{"numbered singleton", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2) ON CONFLICT(id) DO NOTHING", "bootstrap", "Bootstrap")`, 0},
		{"numbered spread", `db.ExecContext(ctx, "INSERT INTO principals(id,name) VALUES($1,$2)", args...)`, 0},
		{"included column is not a key", `db.ExecContext(ctx, "INSERT INTO covering(id,note) VALUES($1,$2)", id, "fixed")`, 1},
		{"constant included column", `db.ExecContext(ctx, "INSERT INTO covering(id,note) VALUES($1,$2)", "fixed", note)`, 0},
		{"conditional indexes", `db.ExecContext(ctx, "INSERT INTO conditional(id,note) VALUES($1,$2)", id, "fixed")`, 0},
		{"qualified table", `db.ExecContext(ctx, "INSERT INTO other.grants(org,label) VALUES($1,$2)", org, "default")`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := "package fixture\n\nconst principalName = \"Service principal\"\n\nfunc insert() {\n\t" + tc.source + "\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			violations, err := ConstantUniquePostgresInserts(t.Context(), db, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != tc.want {
				t.Fatalf("violations=%q want %d", violations, tc.want)
			}
		})
	}
}
