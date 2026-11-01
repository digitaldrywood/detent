package testenv

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sqliteInventoryFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `
PRAGMA foreign_keys=ON;
CREATE TABLE organizations(id TEXT PRIMARY KEY);
CREATE TABLE items(id INTEGER PRIMARY KEY,organization_id TEXT REFERENCES organizations(id),value,enabled BOOLEAN,created_at DATETIME);
CREATE INDEX items_value ON items(value COLLATE NOCASE DESC) WHERE value IS NOT NULL;
CREATE TRIGGER items_insert AFTER INSERT ON items BEGIN SELECT NEW.id; END;
CREATE TABLE "quoted""table"("quoted""column" TEXT);
INSERT INTO "quoted""table" VALUES ('example');
INSERT INTO organizations VALUES ('org_example'),('org_other');
INSERT INTO items VALUES (1,'org_example',NULL,1,'2026-01-02T03:04:05Z'),
(2,'org_other',7,0,'2026-01-02T03:04:05.123456789Z'),
(3,'org_example',1.5,1,1767323045),(4,'org_other','example',0,NULL),(5,'org_example',x'0102',1,NULL);
`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteInventoryRejectsUnclassifiedSchema(t *testing.T) {
	t.Parallel()
	for _, mutation := range []struct{ name, sql, key string }{
		{"table", `CREATE TABLE fixture(id INTEGER)`, "table/fixture"},
		{"column", `ALTER TABLE items ADD COLUMN fixture TEXT`, "column/items/fixture"},
		{"generated column", `ALTER TABLE items ADD COLUMN fixture TEXT GENERATED ALWAYS AS (organization_id) VIRTUAL`, "column/items/fixture"},
		{"index", `CREATE INDEX fixture ON items(value DESC) WHERE value IS NOT NULL`, "index/items/fixture"},
		{"trigger", `CREATE TRIGGER fixture AFTER INSERT ON items BEGIN UPDATE items SET enabled=1 WHERE id=NEW.id; END`, "definition/trigger/fixture"},
		{"changed index", `DROP INDEX items_value; CREATE INDEX items_value ON items(value COLLATE BINARY ASC) WHERE value IS NOT NULL`, "index_columns/items/items_value"},
		{"changed trigger", `DROP TRIGGER items_insert; CREATE TRIGGER items_insert AFTER INSERT ON items BEGIN SELECT NEW.enabled; END`, "definition/trigger/items_insert"},
		{"foreign key", `ALTER TABLE items ADD COLUMN fixture TEXT REFERENCES organizations(id)`, "foreign_key/items/0/0"},
		{"removed column", `DROP INDEX items_value; ALTER TABLE items DROP COLUMN value`, "column/items/value"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			db := sqliteInventoryFixture(t)
			before := SQLiteSchema(t, db)
			if _, err := db.ExecContext(t.Context(), mutation.sql); err != nil {
				t.Fatal(err)
			}
			problems := SQLiteInventoryDifferences(SQLiteSchema(t, db), before)
			if !strings.Contains(strings.Join(problems, "\n"), mutation.key+":") {
				t.Fatalf("missing rejection for %s: %v", mutation.key, problems)
			}
		})
	}
}

func TestSQLiteInventoryMetadata(t *testing.T) {
	t.Parallel()
	schema := SQLiteSchema(t, sqliteInventoryFixture(t))
	for _, fixture := range []struct{ key, want string }{
		{"column/items/value", `[2,"value","",0,null,0,0]`},
		{"column/items/enabled", `[3,"enabled","BOOLEAN",0,null,0,0]`},
		{"column/items/created_at", `[4,"created_at","DATETIME",0,null,0,0]`},
		{"foreign_key/items/0/0", `[0,0,"organizations","organization_id","id","NO ACTION","NO ACTION","NONE"]`},
		{"index/items/items_value", `["items_value",0,"c",1]`},
		{"index_columns/items/items_value", `[[0,2,"value",1,"NOCASE",1],[1,-1,null,0,"BINARY",0]]`},
		{"definition/trigger/items_insert", `["trigger","items_insert","items","CREATE TRIGGER items_insert AFTER INSERT ON items BEGIN SELECT NEW.id; END"]`},
	} {
		t.Run(fixture.key, func(t *testing.T) {
			if got := schema[fixture.key]; got != fixture.want {
				t.Fatalf("metadata = %s; want %s", got, fixture.want)
			}
		})
	}
}

func TestSQLiteStorageClasses(t *testing.T) {
	t.Parallel()
	db := sqliteInventoryFixture(t)
	got := SQLiteStorageClasses(t, db)
	for _, sample := range []struct{ column, want string }{
		{"items/value", `[["blob",1],["integer",1],["null",1],["real",1],["text",1]]`},
		{"items/enabled", `[["integer",5]]`},
		{"items/created_at", `[["integer",1],["null",2],["text",2]]`},
		{"organizations/id", `[["text",2]]`},
		{"quoted\"table/quoted\"column", `[["text",1]]`},
	} {
		t.Run(sample.column, func(t *testing.T) {
			if got[sample.column] != sample.want {
				t.Fatalf("storage classes = %s; want %s", got[sample.column], sample.want)
			}
		})
	}
}

func TestSQLiteClassificationProblems(t *testing.T) {
	t.Parallel()
	schema := map[string]string{"table/items": "", "column/items/value": ""}
	complete := SQLiteTableClassification{Ownership: "organization-owned", Scope: "items.organization_id", InlinePayloads: []string{"value"}}
	for _, fixture := range []struct {
		name   string
		policy map[string]SQLiteTableClassification
		want   []string
	}{
		{"complete", map[string]SQLiteTableClassification{"items": complete}, nil},
		{"unknown table", nil, []string{"items: unclassified table"}},
		{"missing ownership", map[string]SQLiteTableClassification{"items": {Scope: "items.organization_id", InlinePayloads: []string{}}}, []string{"items: incomplete ownership or payload classification"}},
		{"missing payload decision", map[string]SQLiteTableClassification{"items": {Ownership: "global", Scope: "shared"}}, []string{"items: incomplete ownership or payload classification"}},
		{"stale payload", map[string]SQLiteTableClassification{"items": {Ownership: "platform", Scope: "internal", InlinePayloads: []string{"removed"}}}, []string{"items/removed: stale inline payload classification"}},
		{"stale table", map[string]SQLiteTableClassification{"items": complete, "removed": complete}, []string{"removed: stale table classification"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if got := SQLiteClassificationProblems(schema, fixture.policy); !reflect.DeepEqual(got, fixture.want) {
				t.Fatalf("problems = %v; want %v", got, fixture.want)
			}
		})
	}
}
