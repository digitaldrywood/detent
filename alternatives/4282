package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var insertStatement = regexp.MustCompile(`(?is)\bINSERT\s+(?:OR\s+\w+\s+)?INTO\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)\s*VALUES\s*\(`)
var conflictTarget = regexp.MustCompile(`(?is)\bON\s+CONFLICT\s*\(([^)]*)\)`)

// ConstantUniqueInserts reports INSERT statements in the non-test Go files of
// dir that write a constant into every column of a UNIQUE or PRIMARY KEY index
// of db. Such a statement can only ever succeed once per database, so every
// later caller fails or is silently ignored. An ON CONFLICT clause naming
// exactly that index's columns, or naming a key that is itself all constant,
// marks a deliberate singleton row and is allowed.
func ConstantUniqueInserts(ctx context.Context, db *sql.DB, dir string) ([]string, error) {
	indexes, err := uniqueIndexes(ctx, db)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read source directory: %w", err)
	}
	var violations []string
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		constants := packageConstants(file)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			for i, arg := range call.Args {
				query, ok := constantString(arg, constants)
				if !ok {
					continue
				}
				for _, violation := range constantInsertViolations(query, call.Args[i+1:], call.Ellipsis.IsValid(), constants, indexes) {
					violations = append(violations, fmt.Sprintf("%s: %s", files.Position(arg.Pos()), violation))
				}
				break
			}
			return true
		})
	}
	return violations, nil
}

func uniqueIndexes(ctx context.Context, db *sql.DB) (map[string][][]string, error) {
	tables, err := queryNames(ctx, db, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	indexes := map[string][][]string{}
	for _, table := range tables {
		names, err := queryNames(ctx, db, `SELECT name FROM pragma_index_list(?) WHERE "unique"=1 AND partial=0`, table)
		if err != nil {
			return nil, fmt.Errorf("list indexes: %w", err)
		}
		for _, name := range names {
			columns, err := queryNames(ctx, db, `SELECT coalesce(name, '') FROM pragma_index_info(?) ORDER BY seqno`, name)
			if err != nil {
				return nil, fmt.Errorf("read index columns: %w", err)
			}
			if len(columns) == 0 || slices.Contains(columns, "") {
				continue
			}
			for i := range columns {
				columns[i] = strings.ToLower(columns[i])
			}
			indexes[strings.ToLower(table)] = append(indexes[strings.ToLower(table)], columns)
		}
	}
	return indexes, nil
}

func queryNames(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func constantInsertViolations(query string, args []ast.Expr, spread bool, constants map[string]ast.Expr, indexes map[string][][]string) []string {
	var violations []string
	for _, match := range insertStatement.FindAllStringSubmatchIndex(query, -1) {
		table := strings.ToLower(query[match[2]:match[3]])
		columns := splitTopLevel(query[match[4]:match[5]])
		values, end := valuesList(query, match[1])
		if values == nil || len(values) != len(columns) {
			continue
		}
		placeholder := strings.Count(query[:match[1]], "?")
		constant := map[string]bool{}
		for i, value := range values {
			value = strings.TrimSpace(value)
			column := strings.ToLower(strings.TrimSpace(columns[i]))
			switch {
			case value == "?":
				if !spread || placeholder < len(args)-1 {
					if placeholder < len(args) {
						if _, ok := constantValue(args[placeholder], constants); ok {
							constant[column] = true
						}
					}
				}
			case strings.EqualFold(value, "NULL"):
			case constantSQL(value):
				constant[column] = true
			}
			placeholder += strings.Count(value, "?")
		}
		statement := query[match[0]:]
		if next := insertStatement.FindStringIndex(query[end:]); next != nil {
			statement = query[match[0] : end+next[0]]
		}
		targets := conflictTargets(statement)
		if slices.ContainsFunc(targets, func(target []string) bool { return allConstant(target, constant) }) {
			continue
		}
		for _, index := range indexes[table] {
			if !allConstant(index, constant) || slices.ContainsFunc(targets, func(target []string) bool { return sameColumns(target, index) }) {
				continue
			}
			violations = append(violations, fmt.Sprintf("INSERT INTO %s writes a constant into unique key (%s)", table, strings.Join(index, ",")))
		}
	}
	return violations
}

func valuesList(query string, open int) ([]string, int) {
	depth := 1
	inString := false
	for i := open; i < len(query); i++ {
		switch c := query[i]; {
		case c == '\'':
			inString = !inString
		case inString:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return splitTopLevel(query[open:i]), i + 1
			}
		}
	}
	return nil, len(query)
}

func splitTopLevel(list string) []string {
	var parts []string
	depth, start := 0, 0
	inString := false
	for i := range len(list) {
		switch c := list[i]; {
		case c == '\'':
			inString = !inString
		case inString:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, list[start:i])
			start = i + 1
		}
	}
	return append(parts, list[start:])
}

func constantSQL(value string) bool {
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return true
	}
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}

func allConstant(index []string, constant map[string]bool) bool {
	for _, column := range index {
		if !constant[column] {
			return false
		}
	}
	return true
}

func conflictTargets(statement string) [][]string {
	var targets [][]string
	for _, match := range conflictTarget.FindAllStringSubmatch(statement, -1) {
		var target []string
		for _, column := range splitTopLevel(match[1]) {
			target = append(target, strings.ToLower(strings.TrimSpace(column)))
		}
		targets = append(targets, target)
	}
	return targets
}

func sameColumns(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func packageConstants(file *ast.File) map[string]ast.Expr {
	constants := map[string]ast.Expr{}
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if i < len(value.Values) {
					constants[name.Name] = value.Values[i]
				}
			}
		}
	}
	return constants
}

func constantString(expr ast.Expr, constants map[string]ast.Expr) (string, bool) {
	value, ok := constantValue(expr, constants)
	if !ok {
		return "", false
	}
	text, err := strconv.Unquote(value)
	if err != nil || !strings.Contains(strings.ToUpper(text), "INSERT") {
		return "", false
	}
	return text, true
}

func constantValue(expr ast.Expr, constants map[string]ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Value, true
	case *ast.ParenExpr:
		return constantValue(e.X, constants)
	case *ast.Ident:
		value, ok := constants[e.Name]
		if !ok {
			return "", false
		}
		delete(constants, e.Name)
		defer func() { constants[e.Name] = value }()
		return constantValue(value, constants)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := constantValue(e.X, constants)
		if !ok {
			return "", false
		}
		right, ok := constantValue(e.Y, constants)
		if !ok {
			return "", false
		}
		l, lerr := strconv.Unquote(left)
		r, rerr := strconv.Unquote(right)
		if lerr != nil || rerr != nil {
			return "", false
		}
		return strconv.Quote(l + r), true
	}
	return "", false
}
