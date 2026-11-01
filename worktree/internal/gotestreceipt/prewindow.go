package gotestreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var (
	environmentReaders = map[string]map[string]bool{
		"os":      {"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true},
		"syscall": {"Getenv": true, "Environ": true},
	}
	environmentListers = map[string]map[string]bool{
		"os":      {"Environ": true},
		"syscall": {"Environ": true},
	}
	unobservedEffects = map[string]map[string]bool{
		"os":            {"Open": true, "OpenFile": true, "ReadFile": true, "ReadDir": true, "Stat": true, "Lstat": true, "DirFS": true, "OpenRoot": true, "OpenInRoot": true, "Readlink": true, "StartProcess": true},
		"io/ioutil":     {"ReadFile": true, "ReadDir": true},
		"io/fs":         {"ReadFile": true, "ReadDir": true, "Stat": true, "Lstat": true, "Glob": true, "WalkDir": true, "ReadLink": true},
		"path/filepath": {"Glob": true, "Walk": true, "WalkDir": true, "EvalSymlinks": true},
		"syscall":       {"Exec": true, "ForkExec": true, "StartProcess": true},
	}
	unobservedPackages = map[string]bool{"os/exec": true}
)

type sourceFile struct {
	syntax  *ast.File
	imports map[string]string
}

type sourceFunc struct {
	decl *ast.FuncDecl
	file *sourceFile
}

func PreWindowInputs(dir string) ([]string, error) {
	files, err := parsePackageDir(dir)
	if err != nil {
		return nil, err
	}
	constants := make(map[string]string)
	functions := make(map[string][]sourceFunc)
	methods := make(map[string][]sourceFunc)
	for _, file := range files {
		for _, decl := range file.syntax.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body == nil {
					continue
				}
				if decl.Recv == nil {
					functions[decl.Name.Name] = append(functions[decl.Name.Name], sourceFunc{decl: decl, file: file})
				} else {
					methods[decl.Name.Name] = append(methods[decl.Name.Name], sourceFunc{decl: decl, file: file})
				}
			case *ast.GenDecl:
				if decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if i < len(value.Values) {
							if text, ok := stringLiteral(value.Values[i]); ok {
								constants[name.Name] = text
							}
						}
					}
				}
			}
		}
	}
	scan := &preWindowScan{constants: constants, functions: functions, methods: methods, visited: make(map[*ast.FuncDecl]bool), seen: make(map[string]bool)}
	for _, file := range files {
		if file.imports["."] != "" {
			return nil, errors.New("test package dot-imports an environment or file reader")
		}
		ast.Inspect(file.syntax, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && file.matches(selector, environmentListers) {
				scan.seen[environInput] = true
			}
			return true
		})
		for _, decl := range file.syntax.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil && decl.Body != nil && (decl.Name.Name == "init" || decl.Name.Name == "TestMain") {
					scan.function(sourceFunc{decl: decl, file: file})
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for _, expr := range value.Values {
							scan.initializer(expr, file)
						}
					}
				}
			}
		}
	}
	if scan.err != nil {
		return nil, scan.err
	}
	inputs := make([]string, 0, len(scan.seen))
	for input := range scan.seen {
		inputs = append(inputs, input)
	}
	slices.Sort(inputs)
	return inputs, nil
}

func parsePackageDir(dir string) ([]*sourceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list test package: %w", err)
	}
	fset := token.NewFileSet()
	var files []*sourceFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		syntax, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse test package: %w", err)
		}
		file := &sourceFile{syntax: syntax, imports: make(map[string]string)}
		for _, spec := range syntax.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			local := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == "." && environmentReaders[path] == nil && unobservedEffects[path] == nil && !unobservedPackages[path] {
				continue
			}
			file.imports[local] = path
		}
		files = append(files, file)
	}
	return files, nil
}

func (f *sourceFile) matches(selector *ast.SelectorExpr, table map[string]map[string]bool) bool {
	pkg, ok := selector.X.(*ast.Ident)
	return ok && table[f.imports[pkg.Name]][selector.Sel.Name]
}

type preWindowScan struct {
	constants map[string]string
	functions map[string][]sourceFunc
	methods   map[string][]sourceFunc
	locals    map[string]bool
	runner    string
	visited   map[*ast.FuncDecl]bool
	seen      map[string]bool
	err       error
}

func (s *preWindowScan) function(fn sourceFunc) {
	if s.visited[fn.decl] {
		return
	}
	s.visited[fn.decl] = true
	s.runner = testingMain(fn)
	s.locals = declaredNames(fn.decl)
	s.node(fn.decl.Body, fn.file, false)
	s.runner, s.locals = "", nil
}

func declaredNames(decl *ast.FuncDecl) map[string]bool {
	names := make(map[string]bool)
	ast.Inspect(decl, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						names[ident.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, ident := range node.Names {
				names[ident.Name] = true
			}
		case *ast.Field:
			for _, ident := range node.Names {
				names[ident.Name] = true
			}
		}
		return true
	})
	return names
}

func (s *preWindowScan) runsTests(selector *ast.SelectorExpr) bool {
	receiver, ok := selector.X.(*ast.Ident)
	return ok && s.runner != "" && receiver.Name == s.runner && selector.Sel.Name == "Run"
}

func testingMain(fn sourceFunc) string {
	params := fn.decl.Type.Params
	if fn.decl.Name.Name != "TestMain" || fn.decl.Recv != nil || params == nil || len(params.List) != 1 || len(params.List[0].Names) != 1 {
		return ""
	}
	pointer, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "M" {
		return ""
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || fn.file.imports[pkg.Name] != "testing" {
		return ""
	}
	name := params.List[0].Names[0].Name
	shadowed := false
	ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok && ident.Name == name {
						shadowed = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, ident := range node.Names {
				shadowed = shadowed || ident.Name == name
			}
		case *ast.Field:
			for _, ident := range node.Names {
				shadowed = shadowed || ident.Name == name
			}
		}
		return !shadowed
	})
	if shadowed {
		return ""
	}
	return name
}

func (s *preWindowScan) initializer(expr ast.Expr, file *sourceFile) {
	if _, ok := expr.(*ast.FuncLit); ok {
		return
	}
	s.node(expr, file, true)
}

func (s *preWindowScan) node(node ast.Node, file *sourceFile, initializer bool) {
	calls := make(map[*ast.SelectorExpr]*ast.CallExpr)
	called := make(map[ast.Node]bool)
	selected := make(map[*ast.Ident]bool)
	var local []string
	var following []sourceFunc
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			called[node.Fun] = true
			if selector, ok := node.Fun.(*ast.SelectorExpr); ok {
				calls[selector] = node
			}
		case *ast.FuncLit:
			if initializer && !called[node] {
				return false
			}
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok {
				selected[key] = true
			}
		case *ast.Ident:
			if _, ok := s.functions[node.Name]; ok && !selected[node] {
				local = append(local, node.Name)
			}
		case *ast.SelectorExpr:
			selected[node.Sel] = true
			if called[node] && !s.runsTests(node) {
				following = append(following, s.methods[node.Sel.Name]...)
			}
			if pkg, ok := node.X.(*ast.Ident); ok && unobservedPackages[file.imports[pkg.Name]] && s.err == nil {
				s.err = errors.New("test package starts processes before the test log starts")
			}
			if file.matches(node, unobservedEffects) && s.err == nil {
				s.err = errors.New("test package reads files or starts processes before the test log starts")
			}
			if file.matches(node, environmentReaders) {
				s.environment(node, calls[node])
			}
		}
		return true
	})
	for _, name := range local {
		following = append(following, s.functions[name]...)
	}
	for _, fn := range following {
		s.function(fn)
	}
}

func (s *preWindowScan) environment(selector *ast.SelectorExpr, call *ast.CallExpr) {
	if call == nil || selector.Sel.Name == "Environ" || selector.Sel.Name == "ExpandEnv" || len(call.Args) != 1 {
		s.seen[environInput] = true
		return
	}
	name, ok := stringLiteral(call.Args[0])
	if !ok {
		if ident, isIdent := call.Args[0].(*ast.Ident); isIdent && !s.locals[ident.Name] {
			name, ok = s.constants[ident.Name]
		}
	}
	if !ok || name == "" || strings.ContainsAny(name, "\t\r\n\x00=/\\:") {
		s.seen[environInput] = true
		return
	}
	s.seen["env\t"+name] = true
}

func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func SourceDigest(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("list test package: %w", err)
	}
	hash := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		contents := sha256.New()
		if err := copyFile(contents, filepath.Join(dir, entry.Name())); err != nil {
			return "", fmt.Errorf("hash test package source: %w", err)
		}
		fmt.Fprintf(hash, "%d %s %x\n", len(entry.Name()), entry.Name(), contents.Sum(nil))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
