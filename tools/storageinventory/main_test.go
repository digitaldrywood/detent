package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRepositoryRouting(t *testing.T) {
	t.Parallel()
	files := os.DirFS("../..")
	inventory, err := scanRepository(files)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(files, "tools/storageinventory/routing.json")
	if err != nil {
		t.Fatal(err)
	}
	if problems := routingProblems(inventory, manifest); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func TestUnmappedStorageFailsClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(fstest.MapFS, *routingManifest)
		want   string
	}{
		{"mapped inventory", func(fstest.MapFS, *routingManifest) {}, ""},
		{"new classified table", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/testdata/hub_classification.json"] = &fstest.MapFile{Data: []byte(`{"projects":{},"unmapped":{}}`)}
		}, "hub/unmapped: unmapped table"},
		{"new target-only table", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/testdata/target_classification.json"] = &fstest.MapFile{Data: []byte(`{"public/new_table":{"ownership":"organization-owned","scope":"new_table.organization_id","inline_payloads":[]}}`)}
		}, "public/new_table: unmapped target table"},
		{"direct owner", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; func direct() { db.ExecContext(ctx, statement) }")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"prepared owner", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; func prepare() { db.PrepareContext(ctx, statement) }")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"aliased database opener", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; import database \"database/sql\"; var open = database.Open")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"prepared constant without execution", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; const prepared = `SELECT id FROM projects`")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"prepared SQL after block comment", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; const prepared = `/* query */ SELECT id FROM projects`")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"quoted prepared update", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; const prepared = `UPDATE \"projects\" SET name = ?`")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"DDL constant", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; const prepared = `CREATE UNIQUE INDEX example ON projects(id)`")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"generated owner", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/pgquery/new.sql.go"] = &fstest.MapFile{Data: []byte("package fixture; func generated() { q.db.QueryContext(ctx, generatedQuery) }")}
		}, "internal/hubserver/postgres/pgquery/new.sql.go: unmapped SQL owner"},
		{"generated query caller", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/new.go"] = &fstest.MapFile{Data: []byte("package fixture; func read() { s.queries.GetWorkItem(ctx, params) }")}
		}, "internal/hubserver/postgres/new.go: unmapped SQL owner"},
		{"indirect generated query binding", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/new.go"] = &fstest.MapFile{Data: []byte("package fixture; import \"example.invalid/project/pgquery\"; func read() { query := pgquery.New(tx); query.GetWorkItem(ctx, params) }")}
		}, "internal/hubserver/postgres/new.go: unmapped SQL owner"},
		{"generated transaction binding", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/pgquery/db.go"] = &fstest.MapFile{Data: []byte("package fixture; func New(db DBTX) *Queries { return &Queries{db: db} }")}
		}, "internal/hubserver/postgres/pgquery/db.go: unmapped SQL owner"},
		{"SQL query source", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/postgres/queries/new.sql"] = &fstest.MapFile{Data: []byte("-- name: New :one\nSELECT id FROM projects;")}
		}, "internal/hubserver/postgres/queries/new.sql: unmapped SQL owner"},
		{"migration source", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/migrations/new.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE example (id TEXT);")}
		}, "internal/hubserver/migrations/new.sql: unmapped SQL owner"},
		{"new method in mapped owner", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/fixture.go"].Data = append(files["internal/hubserver/fixture.go"].Data, []byte("\nfunc added() { db.QueryRowContext(ctx, statement) }")...)
		}, "internal/hubserver/fixture.go: SQL owner inventory changed"},
		{"new prepared constant in mapped owner", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/fixture.go"].Data = append(files["internal/hubserver/fixture.go"].Data, []byte("\nconst prepared = `SELECT id FROM projects`")...)
		}, "internal/hubserver/fixture.go: SQL owner inventory changed"},
		{"new named query in mapped SQL source", func(files fstest.MapFS, manifest *routingManifest) {
			name := "internal/hubserver/postgres/queries/existing.sql"
			files[name] = &fstest.MapFile{Data: []byte("-- name: Existing :one\nSELECT id FROM projects;\n-- name: Added :one\nSELECT id FROM projects;")}
			manifest.Owners[name] = ownerRoute{Families: []string{"core"}, Disposition: "target-adapter", Inventory: sourceOwner{
				Operations: map[string]int{"SQL": 1, "query:Existing:one": 1}, Tables: []string{"hub/projects"},
			}}
		}, "internal/hubserver/postgres/queries/existing.sql: SQL owner inventory changed"},
		{"method value", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; var execute = db.ExecContext")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"indirect SQL helper", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; func indirect() { querySQL(ctx, q, statement) }")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"SQLite native prepared query", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/new.go"] = &fstest.MapFile{Data: []byte("package fixture; func prepare() { sqlite3.Xsqlite3_prepare_v2(tls, db, statement, -1, out, 0) }")}
		}, "internal/hubserver/new.go: unmapped SQL owner"},
		{"stale owner", func(files fstest.MapFS, _ *routingManifest) {
			delete(files, "internal/hubserver/fixture.go")
		}, "internal/hubserver/fixture.go: stale SQL owner route"},
		{"stale table", func(_ fstest.MapFS, manifest *routingManifest) {
			manifest.Tables["hub/stale"] = tableRoute{Family: "core", Disposition: "target", Contract: "Preserve ID"}
		}, "hub/stale: stale table route"},
		{"invalid destination", func(_ fstest.MapFS, manifest *routingManifest) {
			manifest.Tables["hub/projects"] = tableRoute{Family: "unknown", Disposition: "target", Contract: "Preserve ID"}
		}, "hub/projects: incomplete table route"},
		{"missing atomic group", func(_ fstest.MapFS, manifest *routingManifest) {
			owner := manifest.Owners["internal/hubserver/fixture.go"]
			owner.AtomicGroups = []string{"missing"}
			manifest.Owners["internal/hubserver/fixture.go"] = owner
		}, "internal/hubserver/fixture.go: unknown atomic group missing"},
		{"missing serving table family", func(_ fstest.MapFS, manifest *routingManifest) {
			manifest.Families["other"] = "Another family"
			owner := manifest.Owners["internal/hubserver/fixture.go"]
			owner.Families = []string{"other"}
			manifest.Owners["internal/hubserver/fixture.go"] = owner
		}, "internal/hubserver/fixture.go: missing table family core"},
		{"stale parity test", func(_ fstest.MapFS, manifest *routingManifest) {
			manifest.AtomicGroups = map[string]atomicGroup{"example": {
				Boundary: "internal/hubserver/fixture.go#read", Families: []string{"core"},
				Tables: []string{"hub/projects"}, Contract: "Commit together", ParityTests: []string{"missing_test.go#TestRemoved"},
			}}
		}, "example: missing parity test missing_test.go#TestRemoved"},
		{"stale transaction boundary", func(_ fstest.MapFS, manifest *routingManifest) {
			manifest.AtomicGroups = map[string]atomicGroup{"example": {
				Boundary: "internal/hubserver/fixture.go#removed", Families: []string{"core"},
				Tables: []string{"hub/projects"}, Contract: "Commit together", ParityTests: []string{"existing"},
			}}
		}, "example: missing atomic member internal/hubserver/fixture.go#removed"},
		{"excluded test fixture", func(files fstest.MapFS, _ *routingManifest) {
			files["internal/hubserver/example_test.go"] = &fstest.MapFile{Data: []byte("package fixture; func TestExample() { db.ExecContext(ctx, statement) }")}
			for _, name := range []string{"internal/hubserver/testdata/example.go", "internal/testenv/example.go"} {
				files[name] = &fstest.MapFile{Data: []byte("invalid fixture")}
			}
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files, manifest := routingFixture(t)
			test.change(files, &manifest)
			inventory, err := scanRepository(files)
			if err != nil {
				t.Fatal(err)
			}
			problems := routingProblems(inventory, manifest)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatal(problems)
				}
			} else if !slices.ContainsFunc(problems, func(problem string) bool { return strings.HasPrefix(problem, test.want) }) {
				t.Fatalf("got %v; want %q", problems, test.want)
			}
		})
	}
}

func routingFixture(t *testing.T) (fstest.MapFS, routingManifest) {
	t.Helper()
	files := fstest.MapFS{
		"cmd": {Mode: fs.ModeDir}, "db": {Mode: fs.ModeDir}, "internal/hubserver/postgres/migrations": {Mode: fs.ModeDir},
		"internal/hubserver/fixture.go": {Data: []byte("package fixture; func read() { db.QueryContext(ctx, `SELECT id FROM projects`) }")},
	}
	for _, name := range []string{"internal/hubserver/testdata/hub", "internal/hubserver/testdata/metrics", "internal/cloudentry/testdata/registry", "internal/cloudentry/testdata/auth"} {
		files[name+"_classification.json"] = &fstest.MapFile{Data: []byte(`{}`)}
	}
	files["internal/hubserver/testdata/hub_classification.json"].Data = []byte(`{"projects":{}}`)
	files["internal/hubserver/postgres/testdata/target_classification.json"] = &fstest.MapFile{Data: []byte(`{}`)}
	inventory, err := scanRepository(files)
	if err != nil {
		t.Fatal(err)
	}
	manifest := routingManifest{
		Families:     map[string]string{"core": "Tenant core records"},
		Tables:       map[string]tableRoute{"hub/projects": {Family: "core", Disposition: "target", Contract: "Preserve project identity"}},
		TargetTables: map[string]tableRoute{},
		Owners:       map[string]ownerRoute{"internal/hubserver/fixture.go": {Families: []string{"core"}, Disposition: "serving", Inventory: inventory.Owners["internal/hubserver/fixture.go"]}},
	}
	return files, manifest
}

func TestInvalidSourcesFailClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, source, want string }{
		{"Go syntax", "internal/hubserver/new.go", "parse storage source"},
		{"classification", "internal/hubserver/testdata/hub_classification.json", "decode table inventory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files, _ := routingFixture(t)
			files[test.source] = &fstest.MapFile{Data: []byte("invalid")}
			if _, err := scanRepository(files); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v; want %q", err, test.want)
			}
		})
	}
}

func TestTableReferencesStayInTheirStore(t *testing.T) {
	t.Parallel()
	tables := map[string]json.RawMessage{"hub/organizations": nil, "registry/organizations": nil, "metrics/tenant": nil}
	for _, test := range []struct {
		file, source string
		want         []string
	}{
		{"internal/hubserver/owner.go", "package fixture; const q = `SELECT * FROM organizations`", []string{"hub/organizations"}},
		{"internal/cloudentry/owner.go", "package fixture; const q = `SELECT * FROM organizations`", []string{"registry/organizations"}},
		{"internal/hubserver/request_metrics.go", "package fixture; const q = `SELECT * FROM tenant`", []string{"metrics/tenant"}},
	} {
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			owner, err := scanSource(test.file, []byte(test.source), tables)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(owner.Tables, test.want) {
				t.Fatalf("got %v; want %v", owner.Tables, test.want)
			}
		})
	}
}
