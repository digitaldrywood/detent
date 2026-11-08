package invariants

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type sqlitePolicy struct {
	Files   map[string]map[string]int `json:"files"`
	Modules map[string]map[string]int `json:"modules"`
}

var sqliteEngine = regexp.MustCompile(`(?mi)(?:^|[{},])\s*(?:-\s*)?["']?engine["']?\s*:\s*["']?sqlite["']?(?:\s*(?:#.*)?$|\s*[,}])`)

var sqliteGoose = regexp.MustCompile(`(?mi)^\s*(?:(?:override|export)\s+)?GOOSE_DRIVER\s*(?::|\?|\+)?=\s*["']?sqlite3?["']?(?:\s|$)`)

type sqliteSource struct {
	name    string
	imports []string
	counts  map[string]int
}

func TestRepositorySQLite(t *testing.T) {
	if *sqliteShrink {
		if err := shrinkSQLitePolicy(repositoryRoot(t)); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Parallel()
	root := os.DirFS(repositoryRoot(t))
	data, err := fs.ReadFile(root, "internal/invariants/sqlite_policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy sqlitePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkSQLitePolicy(root, policy) {
		t.Error(problem)
	}
}

func checkSQLitePolicy(root fs.FS, policy sqlitePolicy) []string {
	current, err := scanSQLite(root)
	if err != nil {
		return []string{fmt.Sprintf("SQLite policy: %v", err)}
	}
	var problems []string
	for file, counts := range current.Files {
		problems = append(problems, sqliteCountProblems(file, counts, policy.Files[file])...)
	}
	for file, counts := range policy.Files {
		if len(counts) == 0 {
			problems = append(problems, file+": remove empty SQLite baseline entry")
		} else if _, exists := current.Files[file]; !exists {
			problems = append(problems, sqliteCountProblems(file, nil, counts)...)
		}
	}
	for _, file := range []string{"go.mod", "go.sum"} {
		problems = append(problems, sqliteCountProblems(file, current.Modules[file], policy.Modules[file])...)
	}
	for file, counts := range policy.Modules {
		if file != "go.mod" && file != "go.sum" {
			problems = append(problems, file+": unknown SQLite module baseline file")
		} else if len(counts) == 0 {
			problems = append(problems, file+": remove empty SQLite module baseline entry")
		}
	}
	slices.Sort(problems)
	return problems
}

func sqliteCountProblems(file string, current, baseline map[string]int) []string {
	var problems []string
	for kind, count := range current {
		if count > baseline[kind] {
			problems = append(problems, fmt.Sprintf("%s: SQLite %s count %d exceeds frozen baseline %d", file, kind, count, baseline[kind]))
		}
	}
	for kind, count := range baseline {
		if count <= 0 || current[kind] < count {
			problems = append(problems, fmt.Sprintf("%s: shrink SQLite %s baseline from %d to %d; remove clean entries", file, kind, count, current[kind]))
		}
	}
	return problems
}

func scanSQLite(root fs.FS) (sqlitePolicy, error) {
	current := sqlitePolicy{Files: make(map[string]map[string]int), Modules: make(map[string]map[string]int)}
	moduleName := ""
	for _, file := range []string{"go.mod", "go.sum"} {
		data, err := fs.ReadFile(root, file)
		if file == "go.sum" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return current, fmt.Errorf("read %s: %w", file, err)
		}
		counts := make(map[string]int)
		for line := range strings.SplitSeq(string(data), "\n") {
			line, _, _ = strings.Cut(line, "//")
			fields := strings.Fields(line)
			if file == "go.mod" && len(fields) == 2 && fields[0] == "module" {
				moduleName = strings.Trim(fields[1], `"`)
			}
			if file == "go.sum" && len(fields) > 0 {
				fields = fields[:1]
			}
			for _, field := range fields {
				module := strings.Trim(field, `"`)
				if strings.Contains(module, "/") && sqliteDriver(module) {
					counts[module]++
				}
			}
		}
		if len(counts) > 0 {
			current.Modules[file] = counts
		}
	}
	var sources []sqliteSource
	sqlFiles := make(map[string][]byte)
	migrationDirs := []string{"internal/store/migrations", "internal/hubserver/migrations", "internal/hubserver/migration_steps", "internal/hubserver/testdata/migrations", "internal/artifact/migrations"}
	err := fs.WalkDir(root, ".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "tmp", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		extension := path.Ext(file)
		makefile := path.Base(file) == "Makefile" || path.Base(file) == "GNUmakefile" || extension == ".mk"
		if !makefile && !slices.Contains([]string{".go", ".sql", ".yaml", ".yml", ".json"}, extension) {
			return nil
		}
		data, err := fs.ReadFile(root, file)
		if err != nil {
			return err
		}
		counts := make(map[string]int)
		switch {
		case extension == ".go":
			var imports []string
			counts, imports, err = sqliteSourceCounts(file, data)
			if err != nil {
				return err
			}
			sources = append(sources, sqliteSource{name: file, imports: imports, counts: counts})
		case extension == ".sql":
			sqlFiles[file] = data
			return nil
		case makefile:
			if count := len(sqliteGoose.FindAllIndex(data, -1)); count > 0 {
				counts["goose_driver"] = count
			}
		default:
			if count := len(sqliteEngine.FindAllIndex(data, -1)); count > 0 {
				counts["sqlc_engine"] = count
				migrationDirs = append(migrationDirs, sqliteSchemaPaths(file, string(data))...)
			}
		}
		if len(counts) > 0 {
			current.Files[file] = counts
		}
		return nil
	})
	if err != nil {
		return current, fmt.Errorf("scan Go sources: %w", err)
	}
	for file, data := range sqlFiles {
		counts := make(map[string]int)
		sqliteSQLCounts(string(data), counts)
		for _, directory := range migrationDirs {
			if file == directory || strings.HasPrefix(file, directory+"/") {
				counts["sqlite_migration"] = 1
				break
			}
		}
		if len(counts) > 0 {
			current.Files[file] = counts
		}
	}
	sqliteWrapperCounts(moduleName, sources, current.Files)
	return current, nil
}

func sqliteSchemaPaths(file, data string) []string {
	entry := regexp.MustCompile(`(?m)^\s*-\s+(?:engine|schema|queries)\s*:`)
	starts := entry.FindAllStringIndex(data, -1)
	var blocks []string
	if len(starts) == 0 {
		blocks = []string{data}
	} else {
		for i, start := range starts {
			end := len(data)
			if i+1 < len(starts) {
				end = starts[i+1][0]
			}
			blocks = append(blocks, data[start[0]:end])
		}
	}
	var paths []string
	for _, block := range blocks {
		if !sqliteEngine.MatchString(block) {
			continue
		}
		lines := strings.Split(block, "\n")
		for i, line := range lines {
			key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-")), ":")
			if !ok || strings.TrimSpace(key) != "schema" {
				continue
			}
			if strings.TrimSpace(value) == "" {
				for _, next := range lines[i+1:] {
					if !strings.HasPrefix(strings.TrimSpace(next), "-") {
						break
					}
					value += "," + strings.TrimPrefix(strings.TrimSpace(next), "-")
				}
			}
			value, _, _ = strings.Cut(value, "#")
			for _, item := range strings.FieldsFunc(value, func(r rune) bool { return strings.ContainsRune(" ,[]\"'\t\r", r) }) {
				paths = append(paths, path.Clean(path.Join(path.Dir(file), item)))
			}
		}
	}
	return paths
}

func sqliteWrapperCounts(module string, sources []sqliteSource, files map[string]map[string]int) {
	reverse := make(map[string][]string)
	tainted := make(map[string]bool)
	var pending []string
	for _, source := range sources {
		if strings.HasSuffix(source.name, "_test.go") {
			continue
		}
		owner := path.Join(module, path.Dir(source.name))
		if len(source.counts) > 0 && !tainted[owner] {
			tainted[owner] = true
			pending = append(pending, owner)
		}
		for _, imported := range source.imports {
			reverse[imported] = append(reverse[imported], owner)
		}
	}
	for i := 0; i < len(pending); i++ {
		for _, importer := range reverse[pending[i]] {
			if !tainted[importer] {
				tainted[importer] = true
				pending = append(pending, importer)
			}
		}
	}
	for _, source := range sources {
		for _, imported := range source.imports {
			if tainted[imported] && !sqliteDriver(imported) {
				if files[source.name] == nil {
					files[source.name] = make(map[string]int)
				}
				files[source.name]["wrapper_import"]++
			}
		}
	}
}

func sqliteDriver(module string) bool {
	module = strings.ToLower(module)
	return strings.Contains(module, "sqlite") || strings.Contains(module, "sqlcipher") || strings.Contains(module, "libsql") || strings.HasPrefix(module, "turso.tech/database")
}

func sqliteSourceCounts(name string, data []byte) (map[string]int, []string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, fmt.Errorf("parse Go source: %w", err)
	}
	counts := make(map[string]int)
	imports := make(map[string]string)
	var imported []string
	for _, spec := range file.Imports {
		module, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("parse import: %w", err)
		}
		alias := path.Base(module)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = module
		imported = append(imported, module)
		if sqliteDriver(module) {
			counts["driver_import"]++
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.Ident:
			switch n.Name {
			case "BackendSQLite":
				counts["backend_sqlite"]++
			case "DatabasePath":
				counts["database_path"]++
			}
			if strings.HasPrefix(strings.ToLower(n.Name), "opensqlite") {
				counts["sqlite_open"]++
			}
		case *ast.SelectorExpr:
			if qualifier, ok := n.X.(*ast.Ident); ok {
				module := imports[qualifier.Name]
				if (sqliteDriver(module) && strings.HasPrefix(n.Sel.Name, "Open")) || (module == "github.com/digitaldrywood/detent/internal/store" && n.Sel.Name == "Open") {
					counts["sqlite_open"]++
				}
			}
		case *ast.CallExpr:
			if selector, ok := n.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Open" && len(n.Args) > 0 {
				if qualifier, ok := selector.X.(*ast.Ident); ok && imports[qualifier.Name] == "database/sql" {
					if driver, ok := sqliteString(n.Args[0]); ok && sqliteDriver(driver) {
						counts["sqlite_open"]++
					}
				}
			}
		}
		if expression, ok := node.(ast.Expr); ok {
			if text, ok := sqliteString(expression); ok {
				sqliteSQLCounts(text, counts)
				return false
			}
		}
		return true
	})
	return counts, imported, nil
}

func sqliteString(expression ast.Expr) (string, bool) {
	switch n := expression.(type) {
	case *ast.BasicLit:
		if n.Kind == token.STRING {
			text, err := strconv.Unquote(n.Value)
			return text, err == nil
		}
	case *ast.BinaryExpr:
		if n.Op == token.ADD {
			left, leftOK := sqliteString(n.X)
			right, rightOK := sqliteString(n.Y)
			return left + right, leftOK && rightOK
		}
	case *ast.ParenExpr:
		return sqliteString(n.X)
	}
	return "", false
}

func TestSQLitePolicyViolations(t *testing.T) {
	t.Parallel()
	query := func(words ...string) string {
		return "package fixture\nconst query = " + strconv.Quote(strings.Join(words, " "))
	}
	driver := "modernc.org/sqlite"
	importSource := "package fixture\nimport _ " + strconv.Quote(driver)
	for _, tt := range []struct {
		name, file, source, module, sum, kind string
		files                                 map[string]string
		allow                                 map[string]map[string]int
		allowModules                          map[string]map[string]int
		baseline, moduleBaseline, sumBaseline int
		missing                               bool
		want                                  string
	}{
		{name: "clean source", source: `package fixture; const query = "SELECT now()"`},
		{name: "new driver", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "aliased driver import", source: `package fixture; import renamed "modernc.org/sqlite"`, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "dot driver import", source: `package fixture; import . "modernc.org/sqlite"`, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "existing driver", source: importSource, kind: "driver_import", baseline: 1},
		{name: "driver increase", source: importSource + "\nimport _ \"github.com/mattn/go-sqlite3\"", kind: "driver_import", baseline: 1, want: "exceeds frozen baseline 1"},
		{name: "driver removed", source: "package fixture", kind: "driver_import", baseline: 1, want: "shrink SQLite driver_import baseline"},
		{name: "file removed", kind: "driver_import", baseline: 1, missing: true, want: "shrink SQLite driver_import baseline"},
		{name: "test file included", file: "nested/new_test.go", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "build excluded file included", file: "nested/new_windows.go", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "build tag included", source: "//go:build ignored_sqlguard_fixture\n\n" + importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "generated code included", file: "nested/query_generated.go", source: "// Code generated by fixture. DO NOT EDIT.\n" + query("INSERT", "OR", "REPLACE INTO items VALUES (1)"), kind: "sql_insert_or", want: "exceeds frozen baseline 0"},
		{name: "other driver", source: `package fixture; import _ "github.com/ncruces/go-sqlite3/driver"`, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "encrypted driver", source: `package fixture; import _ "github.com/mutecomm/go-sqlcipher/v4"`, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "backend selector", source: "package fixture; var backend = store.BackendSQLite", kind: "backend_sqlite", want: "exceeds frozen baseline 0"},
		{name: "backend local", source: "package store; const BackendSQLite = 0", kind: "backend_sqlite", want: "exceeds frozen baseline 0"},
		{name: "hub config", source: `package fixture; var config = hubserver.Config{DatabasePath: "hub.db"}`, kind: "database_path", want: "exceeds frozen baseline 0"},
		{name: "config field", source: "package hubserver; type Config struct { DatabasePath string }", kind: "database_path", want: "exceeds frozen baseline 0"},
		{name: "SQL open alias", source: `package fixture; import db "database/sql"; var _, _ = db.Open("sqlite", ":memory:")`, kind: "sqlite_open", want: "exceeds frozen baseline 0"},
		{name: "store open alias", source: `package fixture; import db "github.com/digitaldrywood/detent/internal/store"; var open = db.Open`, kind: "sqlite_open", want: "exceeds frozen baseline 0"},
		{name: "driver open alias", source: `package fixture; import db "zombiezen.com/go/sqlite"; var open = db.OpenConn`, kind: "sqlite_open", want: "exceeds frozen baseline 0"},
		{name: "local SQLite open", source: "package fixture; func openSQLite() {}", kind: "sqlite_open", want: "exceeds frozen baseline 0"},
		{name: "julianday", source: query("SELECT", "julianday", "('now')"), kind: "sql_julianday", want: "exceeds frozen baseline 0"},
		{name: "datetime now", source: query("SELECT", "DaTeTiMe", "(", "'now'", ")"), kind: "sql_datetime_now", want: "exceeds frozen baseline 0"},
		{name: "schema lookup", source: query("SELECT name FROM", strings.Join([]string{"sqlite", "master"}, "_")), kind: "sql_schema", want: "exceeds frozen baseline 0"},
		{name: "schema alias", source: query("SELECT name FROM", strings.Join([]string{"sqlite", "schema"}, "_")), kind: "sql_schema", want: "exceeds frozen baseline 0"},
		{name: "conflict insert", source: query("INSERT", "OR", "IGNORE INTO items VALUES (1)"), kind: "sql_insert_or", want: "exceeds frozen baseline 0"},
		{name: "pragma", source: query("PRAGMA", "table_info(items)"), kind: "sql_pragma", want: "exceeds frozen baseline 0"},
		{name: "pragma index list table", source: query("SELECT * FROM", "pragma_index_list", "('items')"), kind: "sql_pragma_table", want: "exceeds frozen baseline 0"},
		{name: "pragma index xinfo table", source: query("SELECT * FROM", "pragma_index_xinfo", "('items')"), kind: "sql_pragma_table", want: "exceeds frozen baseline 0"},
		{name: "other pragma table", source: query("SELECT * FROM", "pragma_table_info", "('items')"), kind: "sql_pragma_table", want: "exceeds frozen baseline 0"},
		{name: "SQL comments cannot hide function", source: query("SELECT", "julianday", "/* comment */", "('now')"), kind: "sql_julianday", want: "exceeds frozen baseline 0"},
		{name: "SQL increase", source: query("PRAGMA", "optimize;", "PRAGMA", "user_version"), kind: "sql_pragma", baseline: 1, want: "exceeds frozen baseline 1"},
		{name: "SQL reduction", source: query("PRAGMA", "optimize"), kind: "sql_pragma", baseline: 2, want: "shrink SQLite sql_pragma baseline from 2 to 1"},
		{name: "concatenated SQL", source: "package fixture; const query = " + strconv.Quote("INSERT ") + " + (" + strconv.Quote("OR ") + " + " + strconv.Quote("IGNORE INTO items VALUES (1)") + ")", kind: "sql_insert_or", want: "exceeds frozen baseline 0"},
		{name: "PostgreSQL conflict clause", source: query("INSERT INTO items (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET id = EXCLUDED.id RETURNING id")},
		{name: "PostgreSQL timestamps", source: query("SELECT NOW(), CURRENT_TIMESTAMP, EXTRACT(EPOCH FROM now()), date_trunc('day', now()), 'now'::timestamptz")},
		{name: "PostgreSQL literal", source: query("SELECT 'PRAGMA optimize' AS description")},
		{name: "PostgreSQL quoted identifiers", source: query(`SELECT "PRAGMA optimize", "julianday(" FROM items`)},
		{name: "PostgreSQL dollar literal", source: query("SELECT $body$PRAGMA optimize; SELECT julianday('now') FROM sqlite_master$body$")},
		{name: "PostgreSQL numbered dollar tag", source: query("SELECT $body1$PRAGMA optimize; SELECT julianday('now') FROM sqlite_master$body1$")},
		{name: "PostgreSQL escape literal", source: query(`SELECT E'escaped\' PRAGMA optimize; julianday(1)'`)},
		{name: "SQL ordinary backslash literal", source: query(`SELECT '\',`, "julianday", "(1)"), kind: "sql_julianday", want: "exceeds frozen baseline 0"},
		{name: "PostgreSQL SQL comments", source: query("SELECT 1 /* PRAGMA optimize */ -- SELECT julianday('now')\n")},
		{name: "PostgreSQL column names", source: query("SELECT pragma, sqlite_master FROM items")},
		{name: "PostgreSQL qualified function", source: query("SELECT analytics.julianday(created_at) FROM items")},
		{name: "PostgreSQL pragma columns", source: query("SELECT pragma_index_list, pragma_index_xinfo FROM example")},
		{name: "PostgreSQL pragma text", source: query("SELECT 'pragma_index_list(1)', $$pragma_index_xinfo('items')$$")},
		{name: "PostgreSQL pragma quoted name", source: query(`SELECT "pragma_index_xinfo"(id) FROM example`)},
		{name: "PostgreSQL pragma qualified name", source: query("SELECT example.pragma_index_xinfo(id) FROM example")},
		{name: "PostgreSQL catalog table", source: query("SELECT indexname FROM pg_catalog.pg_indexes WHERE tablename = $1")},
		{
			name: "new file cannot borrow removed allowance", source: importSource, kind: "driver_import", want: "internal/example/new.go: SQLite driver_import count 1 exceeds frozen baseline 0",
			files: map[string]string{"internal/example/old.go": "package fixture"},
			allow: map[string]map[string]int{"internal/example/old.go": {"driver_import": 1}},
		},
		{
			name: "new kind cannot borrow removed allowance", source: query("PRAGMA", "optimize"), kind: "sql_pragma", want: "exceeds frozen baseline 0",
			allow: map[string]map[string]int{"internal/example/new.go": {"driver_import": 1}},
		},
		{
			name: "wrapper chain import", source: `package fixture; import renamed "example.org/project/internal/outer"; var open = renamed.Open`, kind: "wrapper_import", want: "internal/example/new.go: SQLite wrapper_import count 1 exceeds frozen baseline 0",
			files: map[string]string{
				"internal/driver/driver.go": importSource,
				"internal/outer/outer.go":   `package outer; import _ "example.org/project/internal/driver"`,
			},
			allow: map[string]map[string]int{"internal/driver/driver.go": {"driver_import": 1}, "internal/outer/outer.go": {"wrapper_import": 1}},
		},
		{
			name: "test dependency does not taint production wrapper", source: `package fixture; import _ "example.org/project/internal/driver"`,
			files: map[string]string{"internal/driver/driver.go": "package driver", "internal/driver/driver_test.go": importSource},
			allow: map[string]map[string]int{"internal/driver/driver_test.go": {"driver_import": 1}},
		},
		{
			name: "zero baseline with PostgreSQL and clean wrapper", source: `package fixture; import _ "example.org/project/internal/driver"`, module: "require github.com/jackc/pgx/v5 v5.0.0", sum: "github.com/jackc/pgx/v5 v5.0.0 h1:example\n",
			files: map[string]string{"internal/driver/driver.go": query("SELECT NOW()")},
		},
		{name: "new module", module: "require github.com/mattn/go-sqlite3 v1.0.0", kind: "github.com/mattn/go-sqlite3", want: "exceeds frozen baseline 0"},
		{name: "existing module", module: "require " + driver + " v1.0.0", kind: driver, moduleBaseline: 1},
		{name: "removed module", kind: driver, moduleBaseline: 1, want: "shrink SQLite " + driver + " baseline"},
		{name: "replacement module", module: "replace example.org/driver => github.com/tursodatabase/libsql-client-go v1.0.0", kind: "github.com/tursodatabase/libsql-client-go", want: "exceeds frozen baseline 0"},
		{name: "indirect module", module: "require (\n github.com/mattn/go-sqlite3 v1.0.0 // indirect\n)", kind: "github.com/mattn/go-sqlite3", want: "exceeds frozen baseline 0"},
		{name: "transitive checksum only", sum: "github.com/mattn/go-sqlite3 v1.0.0 h1:example\n", kind: "github.com/mattn/go-sqlite3", want: "go.sum: SQLite github.com/mattn/go-sqlite3 count 1 exceeds frozen baseline 0"},
		{name: "transitive module checksum", sum: "modernc.org/sqlite v1.0.0/go.mod h1:example\n", kind: driver, want: "go.sum: SQLite modernc.org/sqlite count 1 exceeds frozen baseline 0"},
		{name: "existing checksum", sum: driver + " v1.0.0 h1:example\n", kind: driver, sumBaseline: 1},
		{name: "removed checksum", kind: driver, sumBaseline: 1, want: "go.sum: shrink SQLite " + driver + " baseline from 1 to 0"},
		{name: "empty checksum entry rejected", allowModules: map[string]map[string]int{"go.sum": {}}, want: "remove empty SQLite module baseline entry"},
		{
			name: "sqlc SQLite engine", files: map[string]string{"sqlc/sqlc.yaml": "version: 2\nsql:\n  - engine: sqlite\n    schema: ../internal/store/migrations\n"}, kind: "sqlc_engine", want: "sqlc/sqlc.yaml: SQLite sqlc_engine count 1 exceeds frozen baseline 0",
		},
		{
			name: "Goose SQLite driver", files: map[string]string{"Makefile": "GOOSE_DRIVER ?= sqlite3\n"}, kind: "goose_driver", want: "Makefile: SQLite goose_driver count 1 exceeds frozen baseline 0",
		},
		{
			name: "SQL source query", files: map[string]string{"db/queries/new.sql": strings.Join([]string{"SELECT", "julianday", "('now')"}, " ")}, kind: "sql_julianday", want: "db/queries/new.sql: SQLite sql_julianday count 1 exceeds frozen baseline 0",
		},
		{
			name: "new SQLite migration even with portable SQL", files: map[string]string{"internal/store/migrations/new.sql": "CREATE TABLE example (id INTEGER PRIMARY KEY);"}, kind: "sqlite_migration", want: "internal/store/migrations/new.sql: SQLite sqlite_migration count 1 exceeds frozen baseline 0",
		},
		{
			name: "SQLite migration schema discovered from sqlc", kind: "sqlite_migration", want: "schemas/new.sql: SQLite sqlite_migration count 1 exceeds frozen baseline 0",
			files: map[string]string{"sqlc.yaml": "version: 2\nsql:\n  - engine: sqlite\n    schema: schemas\n", "schemas/new.sql": "CREATE TABLE example (id INTEGER PRIMARY KEY);"},
			allow: map[string]map[string]int{"sqlc.yaml": {"sqlc_engine": 1}},
		},
		{
			name: "PostgreSQL input files", files: map[string]string{
				"Makefile": "GOOSE_DRIVER ?= postgres\n", "sqlc.yaml": "version: 2\nsql:\n  - engine: postgresql\n    schema: internal/hubserver/postgres/migrations\n",
				"internal/hubserver/postgres/migrations/new.sql": "CREATE TABLE example (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY);",
				"db/queries/new.sql":                             "INSERT INTO example DEFAULT VALUES ON CONFLICT DO NOTHING RETURNING id;",
			},
		},
		{
			name: "mixed sqlc engines preserve PostgreSQL migrations", files: map[string]string{
				"sqlc.yaml": "version: 2\nsql:\n  - engine: sqlite\n    schema: legacy\n  - engine: postgresql\n    schema: postgres\n", "postgres/new.sql": "CREATE TABLE example (id bigint);",
			}, allow: map[string]map[string]int{"sqlc.yaml": {"sqlc_engine": 1}},
		},
		{name: "module comment ignored", module: "require example.org/driver v1.0.0 // modernc.org/sqlite"},
		{name: "invalid Go fails closed", source: "package", want: "parse Go source"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			file := tt.file
			if file == "" {
				file = "internal/example/new.go"
			}
			if !tt.missing {
				name := filepath.Join(root, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				source := tt.source
				if source == "" {
					source = "package fixture"
				}
				if err := os.WriteFile(name, []byte(source), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for file, source := range tt.files {
				name := filepath.Join(root, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(source), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/project\ngo 1.26.0\n"+tt.module+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.sum != "" {
				if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(tt.sum), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			policy := sqlitePolicy{Files: make(map[string]map[string]int), Modules: make(map[string]map[string]int)}
			for file, counts := range tt.allow {
				policy.Files[file] = counts
			}
			for file, counts := range tt.allowModules {
				policy.Modules[file] = counts
			}
			if tt.baseline > 0 {
				policy.Files[file] = map[string]int{tt.kind: tt.baseline}
			}
			if tt.moduleBaseline > 0 {
				policy.Modules["go.mod"] = map[string]int{tt.kind: tt.moduleBaseline}
			}
			if tt.sumBaseline > 0 {
				policy.Modules["go.sum"] = map[string]int{tt.kind: tt.sumBaseline}
			}
			problems := checkSQLitePolicy(os.DirFS(root), policy)
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("unexpected violations: %v", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(problem string) bool {
				return strings.Contains(problem, tt.kind) && strings.Contains(problem, tt.want)
			}) {
				t.Fatalf("got %v, want %s violation containing %q", problems, tt.kind, tt.want)
			}
		})
	}
}
