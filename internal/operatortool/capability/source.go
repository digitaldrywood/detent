// Package capability inventories dashboard operations without adding runtime authority.
package capability

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Candidate is a source site, not a capability. Several sites can implement one operation.
// Definition is retained so drift reports can be reviewed without opaque hashes.
type Candidate struct {
	Source       string `json:"source"`
	Kind         string `json:"kind"`
	Definition   string `json:"definition"`
	Occurrence   int    `json:"occurrence"`
	Line         int    `json:"line"`
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	Handler      string `json:"handler,omitempty"`
	ResolvedPath string `json:"resolved_path,omitempty"`
}

func (c Candidate) Key() string {
	return fmt.Sprintf("%s|%s|%s|%d", c.Source, c.Kind, c.Definition, c.Occurrence)
}

// Discover independently walks production route and frontend sources. It never reads the matrix.
// Generated bundles/templates and tests are not authoritative definitions.
func Discover(root fs.FS) ([]Candidate, error) {
	var result []Candidate
	constants := map[string]map[string]ast.Expr{}
	err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != "." || entry.Name() == "node_modules" || entry.Name() == "dist" || entry.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		catalog := path == "internal/operatortool/catalog.go"
		backend := strings.HasPrefix(path, "internal/web/") || strings.HasPrefix(path, "internal/hubserver/") || strings.HasPrefix(path, "internal/cloudentry/")
		frontend := strings.HasPrefix(path, "web/conversation/src/") || strings.HasPrefix(path, "static/js/") || backend && ext == ".templ"
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "_templ.go") || strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") {
			return nil
		}
		if !catalog && !(backend && ext == ".go") && !(frontend && (ext == ".templ" || ext == ".tsx" || ext == ".ts" || ext == ".js" || ext == ".html")) {
			return nil
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		var sites []Candidate
		if catalog {
			sites, err = tools(path, data)
		} else if ext == ".go" {
			sites, err = routes(path, data)
			if err == nil {
				err = collectConstants(path, data, constants)
			}
		} else {
			sites = requests(path, string(data))
		}
		if err != nil {
			return err
		}
		result = append(result, sites...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range result {
		if result[i].Kind != "route" {
			continue
		}
		expr, err := parser.ParseExpr(result[i].Path)
		if err != nil {
			continue
		}
		if resolved, ok := resolveConstant(expr, constants[filepath.Dir(result[i].Source)], map[string]bool{}); ok {
			result[i].ResolvedPath = resolved
			result[i].Definition += " [route=" + strconv.Quote(resolved) + "]"
		}
	}
	counts := map[string]int{}
	for i := range result {
		key := result[i].Source + "|" + result[i].Kind + "|" + result[i].Definition
		counts[key]++
		result[i].Occurrence = counts[key]
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result, nil
}

func nodeText(set *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, set, n); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func routes(path string, data []byte) ([]Candidate, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, data, 0)
	if err != nil {
		return nil, err
	}
	receivers := map[string]bool{"s.echo": true}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Field:
			if nodeText(set, v.Type) == "*echo.Echo" || nodeText(set, v.Type) == "*echo.Group" {
				for _, name := range v.Names {
					receivers[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, right := range v.Rhs {
				if i >= len(v.Lhs) {
					break
				}
				text := nodeText(set, right)
				if text == "s.echo" || strings.HasPrefix(text, "echo.New(") {
					receivers[nodeText(set, v.Lhs[i])] = true
				}
				if call, ok := right.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Group" && receivers[nodeText(set, sel.X)] {
						receivers[nodeText(set, v.Lhs[i])] = true
					}
				}
			}
		}
		return true
	})
	var result []Candidate
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// Recognized Echo receivers include parameters, local aliases and groups. Do not mistake slog.Any or sql calls for registrations.
		receiver := nodeText(set, sel.X)
		if !receivers[receiver] {
			return true
		}
		switch sel.Sel.Name {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "Any", "Add", "Match", "Static", "File", "Group":
		default:
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		method := sel.Sel.Name
		routeIndex := 0
		handlerIndex := 1
		if method == "Add" || method == "Match" {
			routeIndex = 1
			handlerIndex = 2
		}
		if routeIndex >= len(call.Args) {
			return true
		}
		route := nodeText(set, call.Args[routeIndex])
		handler := ""
		if len(call.Args) > handlerIndex {
			handler = nodeText(set, call.Args[handlerIndex])
		}
		result = append(result, Candidate{Source: path, Kind: "route", Definition: nodeText(set, call), Line: set.Position(call.Pos()).Line, Method: method, Path: route, Handler: handler})
		return true
	})
	return result, nil
}

var attribute = regexp.MustCompile(`\b(hx-(?:get|post|put|patch|delete)|action|formAction|onSubmit|href|data-(?:[a-z-]*(?:url|endpoint)))\s*=\s*`)
var network = regexp.MustCompile(`\b(?:fetch|doFetch|fetchImpl|send|sendMultipart|read|post|submit|EventSource|WebSocket|htmx\.ajax)\s*(?:<[^\n;{}]+>)?\s*\(`)
var openingTag = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9_.:-]*\b`)
var form = regexp.MustCompile(`<form\b`)
var clientRoute = regexp.MustCompile(`<Route\b`)
var actionElement = regexp.MustCompile(`<(?:input|button|option)\b`)

func requests(path, source string) []Candidate {
	var result []Candidate
	var spans [][2]int
	for _, match := range openingTag.FindAllStringIndex(source, -1) {
		spans = append(spans, [2]int{match[0], tagEnd(source, match[1])})
	}
	add := func(kind string, start, end int) {
		definition := strings.Join(strings.Fields(source[start:end]), " ")
		result = append(result, Candidate{Source: path, Kind: kind, Definition: definition, Line: strings.Count(source[:start], "\n") + 1})
	}
	for _, m := range attribute.FindAllStringSubmatchIndex(source, -1) {
		start := m[0]
		inside := false
		for _, span := range spans {
			if start > span[0] && start < span[1] {
				inside = true
				break
			}
		}
		if !inside {
			continue
		}
		if m[1] >= len(source) || !strings.ContainsRune("{\"'`", rune(source[m[1]])) {
			continue
		}
		end := valueEnd(source, m[1])
		if end <= m[1] {
			continue
		}
		// action is a component prop too: inventory it conservatively, with an explicit local-UI decision when appropriate.
		add("attribute", start, end)
	}
	for _, m := range network.FindAllStringIndex(source, -1) {
		if inComment(source, m[0]) || strings.HasSuffix(strings.TrimSpace(source[max(0, m[0]-20):m[0]]), "function") {
			continue
		}
		end := balancedEnd(source, m[1]-1)
		if end <= m[1] {
			continue
		}
		args := splitArguments(source[m[1] : end-1])

		definition := source[m[0]:m[1]] + strings.Join(args, ",") + ")"
		result = append(result, Candidate{Source: path, Kind: "request", Definition: strings.Join(strings.Fields(definition), " "), Line: strings.Count(source[:m[0]], "\n") + 1})
	}
	for _, m := range append(append(form.FindAllStringIndex(source, -1), clientRoute.FindAllStringIndex(source, -1)...), actionElement.FindAllStringIndex(source, -1)...) {
		// Track even forms with no URL or onSubmit, so a new browser-only action cannot vanish from coverage.
		end := tagEnd(source, m[1])
		if end < len(source) {
			tag := source[m[0] : end+1]
			if strings.HasPrefix(tag, "<input") || strings.HasPrefix(tag, "<button") || strings.HasPrefix(tag, "<option") {
				if strings.Contains(tag, `name="action"`) || strings.Contains(tag, `name="operation"`) {
					add("action", m[0], end+1)
				}
			} else {
				add("form", m[0], end+1)
			}
		}
	}
	return result
}

func inComment(s string, pos int) bool {
	line := strings.LastIndex(s[:pos], "\n") + 1
	prefix := strings.TrimSpace(s[line:pos])
	return strings.HasPrefix(prefix, "//") || strings.HasPrefix(prefix, "*")
}

func valueEnd(s string, pos int) int {
	if pos >= len(s) {
		return pos
	}
	switch s[pos] {
	case '{', '(':
		return balancedEnd(s, pos)
	case '"', '\'', '`':
		return quotedEnd(s, pos)
	}
	end := pos
	for end < len(s) && !strings.ContainsRune(" \n\t>", rune(s[end])) {
		end++
	}
	return end
}
func quotedEnd(s string, pos int) int {
	q := s[pos]
	for i := pos + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			return i + 1
		}
	}
	return len(s)
}
func balancedEnd(s string, pos int) int {
	if pos >= len(s) {
		return pos
	}
	stack := []byte{s[pos]}
	for i := pos + 1; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '`':
			i = quotedEnd(s, i) - 1
		case '(', '{', '[':
			stack = append(stack, s[i])
		case ')', '}', ']':
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}
func splitArguments(s string) []string {
	var args []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '`':
			i = quotedEnd(s, i) - 1
		case '(', '{', '[':
			i = balancedEnd(s, i) - 1
		case ',':
			args = append(args, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if v := strings.TrimSpace(s[start:]); v != "" {
		args = append(args, v)
	}
	return args
}

// collectConstants reads source declarations, not a copied route prefix list.
func collectConstants(path string, data []byte, all map[string]map[string]ast.Expr) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if all[dir] == nil {
		all[dir] = map[string]ast.Expr{}
	}
	for _, decl := range file.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			for i, name := range v.Names {
				if i < len(v.Values) {
					all[dir][name.Name] = v.Values[i]
				}
			}
		}
	}
	return nil
}
func resolveConstant(expr ast.Expr, constants map[string]ast.Expr, visiting map[string]bool) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			v, err := strconv.Unquote(e.Value)
			return v, err == nil
		}
	case *ast.Ident:
		if !visiting[e.Name] {
			if v, ok := constants[e.Name]; ok {
				visiting[e.Name] = true
				result, ok := resolveConstant(v, constants, visiting)
				delete(visiting, e.Name)
				return result, ok
			}
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			left, lok := resolveConstant(e.X, constants, visiting)
			right, rok := resolveConstant(e.Y, constants, visiting)
			return left + right, lok && rok
		}
	case *ast.ParenExpr:
		return resolveConstant(e.X, constants, visiting)
	}
	return "", false
}

func tools(path string, data []byte) ([]Candidate, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, data, 0)
	if err != nil {
		return nil, err
	}
	var result []Candidate
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "definition" {
			return true
		}
		result = append(result, Candidate{Source: path, Kind: "existing_tool", Definition: nodeText(set, call), Line: set.Position(call.Pos()).Line, Handler: nodeText(set, call.Args[0])})
		return true
	})
	return result, nil
}

func tagEnd(source string, start int) int {
	for end := start; end < len(source); end++ {
		switch source[end] {
		case '>':
			return end
		case '{':
			end = balancedEnd(source, end) - 1
		case '\'', '"':
			end = quotedEnd(source, end) - 1
		}
	}
	return len(source)
}
