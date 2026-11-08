package invariants

import (
	"encoding/json"
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
	Modules map[string]int            `json:"modules"`
}

var sqliteSQLRules = map[string]*regexp.Regexp{
	"sql_julianday":    regexp.MustCompile(`(?i)\bjulianday\s*\(`),
	"sql_datetime_now": regexp.MustCompile(`(?i)\bdatetime\s*\(\s*['"]now['"]`),
	"sql_schema":       regexp.MustCompile(`(?i)\bsqlite_(?:master|schema|temp_master|temp_schema)\b`),
	"sql_insert_or":    regexp.MustCompile(`(?i)\binsert\s+or\b`),
	"sql_pragma":       regexp.MustCompile(`(?i)\bpragma\s+`),
}

func TestRepositorySQLite(t *testing.T) {
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
	problems = append(problems, sqliteCountProblems("go.mod", current.Modules, policy.Modules)...)
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
	current := sqlitePolicy{Files: make(map[string]map[string]int), Modules: make(map[string]int)}
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
		if !strings.HasSuffix(file, ".go") {
			return nil
		}
		data, err := fs.ReadFile(root, file)
		if err != nil {
			return err
		}
		counts, err := sqliteSourceCounts(file, data)
		if err != nil {
			return err
		}
		if len(counts) > 0 {
			current.Files[file] = counts
		}
		return nil
	})
	if err != nil {
		return current, fmt.Errorf("scan Go sources: %w", err)
	}
	data, err := fs.ReadFile(root, "go.mod")
	if err != nil {
		return current, fmt.Errorf("read go.mod: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		for _, field := range strings.Fields(line) {
			module := strings.Trim(field, `"`)
			if strings.Contains(module, "/") && sqliteDriver(module) {
				current.Modules[module]++
			}
		}
	}
	return current, nil
}

func sqliteDriver(module string) bool {
	module = strings.ToLower(module)
	return strings.Contains(module, "sqlite") || strings.Contains(module, "sqlcipher") || strings.Contains(module, "libsql") || strings.HasPrefix(module, "turso.tech/database")
}

func sqliteSourceCounts(name string, data []byte) (map[string]int, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse Go source: %w", err)
	}
	counts := make(map[string]int)
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		module, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parse import: %w", err)
		}
		alias := path.Base(module)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = module
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
				for kind, pattern := range sqliteSQLRules {
					if count := len(pattern.FindAllStringIndex(text, -1)); count > 0 {
						counts[kind] += count
					}
				}
				return false
			}
		}
		return true
	})
	return counts, nil
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
		name, file, source, module, kind string
		baseline, moduleBaseline         int
		missing                          bool
		want                             string
	}{
		{name: "clean source", source: `package fixture; const query = "SELECT now()"`},
		{name: "new driver", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "existing driver", source: importSource, kind: "driver_import", baseline: 1},
		{name: "driver increase", source: importSource + "\nimport _ \"github.com/mattn/go-sqlite3\"", kind: "driver_import", baseline: 1, want: "exceeds frozen baseline 1"},
		{name: "driver removed", source: "package fixture", kind: "driver_import", baseline: 1, want: "shrink SQLite driver_import baseline"},
		{name: "file removed", kind: "driver_import", baseline: 1, missing: true, want: "shrink SQLite driver_import baseline"},
		{name: "test file included", file: "nested/new_test.go", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
		{name: "build excluded file included", file: "nested/new_windows.go", source: importSource, kind: "driver_import", want: "exceeds frozen baseline 0"},
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
		{name: "SQL increase", source: query("PRAGMA", "optimize;", "PRAGMA", "user_version"), kind: "sql_pragma", baseline: 1, want: "exceeds frozen baseline 1"},
		{name: "SQL reduction", source: query("PRAGMA", "optimize"), kind: "sql_pragma", baseline: 2, want: "shrink SQLite sql_pragma baseline from 2 to 1"},
		{name: "concatenated SQL", source: "package fixture; const query = " + strconv.Quote("INSERT ") + " + (" + strconv.Quote("OR ") + " + " + strconv.Quote("IGNORE INTO items VALUES (1)") + ")", kind: "sql_insert_or", want: "exceeds frozen baseline 0"},
		{name: "new module", module: "require github.com/mattn/go-sqlite3 v1.0.0", kind: "github.com/mattn/go-sqlite3", want: "exceeds frozen baseline 0"},
		{name: "existing module", module: "require " + driver + " v1.0.0", kind: driver, moduleBaseline: 1},
		{name: "removed module", kind: driver, moduleBaseline: 1, want: "shrink SQLite " + driver + " baseline"},
		{name: "replacement module", module: "replace example.org/driver => github.com/tursodatabase/libsql-client-go v1.0.0", kind: "github.com/tursodatabase/libsql-client-go", want: "exceeds frozen baseline 0"},
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
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/project\ngo 1.26.0\n"+tt.module+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			policy := sqlitePolicy{Files: make(map[string]map[string]int), Modules: make(map[string]int)}
			if tt.baseline > 0 {
				policy.Files[file] = map[string]int{tt.kind: tt.baseline}
			}
			if tt.moduleBaseline > 0 {
				policy.Modules[tt.kind] = tt.moduleBaseline
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
