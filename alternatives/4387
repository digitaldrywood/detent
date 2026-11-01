package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type sourceInventory struct {
	Tables map[string]json.RawMessage `json:"tables"`
	Owners map[string]sourceOwner     `json:"owners"`
	Tests  map[string]bool            `json:"-"`
}

type sourceOwner struct {
	Operations   map[string]int `json:"operations"`
	Tables       []string       `json:"tables"`
	Declarations []string       `json:"-"`
}

func scanRepository(files fs.FS) (sourceInventory, error) {
	result := sourceInventory{Tables: make(map[string]json.RawMessage), Owners: make(map[string]sourceOwner), Tests: make(map[string]bool)}
	for _, source := range []struct{ name, file string }{
		{"hub", "internal/hubserver/testdata/hub_classification.json"},
		{"metrics", "internal/hubserver/testdata/metrics_classification.json"},
		{"registry", "internal/cloudentry/testdata/registry_classification.json"},
		{"auth", "internal/cloudentry/testdata/auth_classification.json"},
	} {
		data, err := fs.ReadFile(files, source.file)
		if err != nil {
			return result, fmt.Errorf("read table inventory: %w", err)
		}
		var tables map[string]json.RawMessage
		if err := json.Unmarshal(data, &tables); err != nil {
			return result, fmt.Errorf("decode table inventory: %w", err)
		}
		for table, classification := range tables {
			result.Tables[source.name+"/"+table] = classification
		}
	}
	for _, root := range []string{"internal", "cmd", "db"} {
		err := fs.WalkDir(files, root, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || name == "internal/testenv" {
					return fs.SkipDir
				}
				return nil
			}
			if path.Ext(name) != ".go" && path.Ext(name) != ".sql" {
				return nil
			}
			data, err := fs.ReadFile(files, name)
			if err != nil {
				return err
			}
			if strings.HasSuffix(name, "_test.go") {
				file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
				if err != nil {
					return err
				}
				for _, declaration := range file.Decls {
					if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
						result.Tests[name+"#"+function.Name.Name] = true
					}
				}
				return nil
			}
			owner, err := scanSource(name, data, result.Tables)
			if err != nil {
				return err
			}
			if len(owner.Operations) != 0 {
				result.Owners[name] = owner
			}
			return nil
		})
		if err != nil {
			return result, fmt.Errorf("scan storage sources: %w", err)
		}
	}
	return result, nil
}

var sqlStart = regexp.MustCompile(`(?is)^\s*(?:(?:--[^\n]*\n|/\*.*?\*/)\s*)*(?:SELECT\s|WITH\s|INSERT\s+(?:OR\s+\w+\s+)?INTO\s|REPLACE\s+INTO\s|UPDATE\s+\S+(?:\s+(?:AS\s+)?\w+)?\s+SET\s|DELETE\s+FROM\s|(?:CREATE|ALTER|DROP)\s+(?:(?:VIRTUAL|TEMP|TEMPORARY|UNIQUE)\s+)?(?:TABLE|INDEX|VIEW|TRIGGER|FUNCTION|SCHEMA|ROLE|DATABASE)\s|TRUNCATE\s|PRAGMA\s)`)
var sqlWord = regexp.MustCompile(`[A-Za-z_][A-Za-z_0-9]*`)
var namedQuery = regexp.MustCompile(`(?m)^\s*--\s*name:\s+(\w+)\s+:(\w+)`)

func scanSource(name string, data []byte, tables map[string]json.RawMessage) (sourceOwner, error) {
	owner := sourceOwner{Operations: make(map[string]int), Tables: []string{}}
	var statements []string
	if path.Ext(name) == ".sql" {
		owner.Operations["SQL"] = 1
		for _, query := range namedQuery.FindAllSubmatch(data, -1) {
			owner.Operations["query:"+string(query[1])+":"+string(query[2])]++
		}
		statements = append(statements, string(data))
	} else {
		file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
		if err != nil {
			return owner, fmt.Errorf("parse storage source: %w", err)
		}
		databasePackages := make(map[string]bool)
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return owner, fmt.Errorf("decode storage import: %w", err)
			}
			if strings.HasSuffix(importPath, "/pgquery") || strings.HasSuffix(importPath, "/store/sqlc") {
				owner.Operations["generated-package:"+importPath]++
			}
			if imported.Path.Value == `"database/sql"` {
				name := "sql"
				if imported.Name != nil {
					name = imported.Name.Name
				}
				databasePackages[name] = true
			}
		}
		if strings.HasSuffix(name, "/pgquery/db.go") || strings.HasSuffix(name, "/store/sqlc/db.go") {
			owner.Operations["generated-transaction-binding"]++
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				symbol := declaration.Name.Name
				if declaration.Recv != nil {
					symbol = receiverName(declaration.Recv.List[0].Type) + "." + symbol
				}
				owner.Declarations = append(owner.Declarations, symbol)
				inspectSQL(declaration, symbol, databasePackages, owner.Operations, &statements)
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						inspectSQL(value, value.Names[0].Name, databasePackages, owner.Operations, &statements)
					}
				}
			}
		}
	}
	words := make(map[string]bool)
	for _, statement := range statements {
		for _, word := range sqlWord.FindAllString(strings.ToLower(statement), -1) {
			words[word] = true
		}
	}
	for key := range tables {
		store, table, _ := strings.Cut(key, "/")
		if words[table] && sourceStore(name, store) {
			owner.Tables = append(owner.Tables, key)
		}
	}
	slices.Sort(owner.Tables)
	return owner, nil
}

func sourceStore(name, store string) bool {
	switch {
	case strings.HasPrefix(name, "internal/hubserver/"):
		if strings.HasPrefix(path.Base(name), "request_") {
			return store == "metrics"
		}
		return store == "hub"
	case strings.HasPrefix(name, "internal/cloudentry/"):
		return store == "registry" || store == "auth"
	default:
		return false
	}
}

func receiverName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverName(expression.X)
	case *ast.IndexExpr:
		return receiverName(expression.X)
	default:
		return "receiver"
	}
}

func inspectSQL(node ast.Node, symbol string, databasePackages map[string]bool, operations map[string]int, statements *[]string) {
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			name := node.Sel.Name
			if receiver, ok := node.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "queries" {
				operations[symbol+":generated:"+name]++
			}
			if name == "queries" {
				operations[symbol+":query-receiver"]++
			}
			if receiver, ok := node.X.(*ast.Ident); ok && databasePackages[receiver.Name] && (name == "Open" || name == "OpenDB") {
				operations[symbol+":"+name]++
			}
			switch name {
			case "Exec", "ExecContext", "Query", "QueryContext", "QueryRow", "QueryRowContext", "Prepare", "PrepareContext", "Stmt", "StmtContext", "Begin", "BeginTx", "execSQL", "querySQL", "queryRowSQL", "Xsqlite3_prepare_v2", "Xsqlite3_prepare_v3", "Xsqlite3_exec", "Xsqlite3_step", "Xsqlite3_open", "Xsqlite3_open_v2":
				operations[symbol+":"+name]++
			}
		case *ast.CallExpr:
			if function, ok := node.Fun.(*ast.Ident); ok && slices.Contains([]string{"execSQL", "querySQL", "queryRowSQL"}, function.Name) {
				operations[symbol+":"+function.Name]++
			}
		case *ast.BasicLit:
			if node.Kind == token.STRING {
				value, err := strconv.Unquote(node.Value)
				if err == nil && sqlStart.MatchString(value) {
					operations[symbol+":SQL"]++
					*statements = append(*statements, value)
				}
			}
		}
		return true
	})
}
