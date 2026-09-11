// Package invariants holds repository policy tests registered with tools/invariantcheck.
package invariants

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

type sourcePolicy struct {
	Reasons []string          `json:"reasons"`
	Dynamic map[string]string `json:"dynamic_functions"`
}

func TestRepositorySources(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "internal/invariants/source_policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy sourcePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	pkgs, err := packages.Load(&packages.Config{Dir: root, Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports}, "./internal/...", "./cmd/...", "./tools/...")
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) != 0 {
		t.Fatal("load repository packages")
	}
	for _, pkg := range pkgs {
		for _, problem := range checkSources(pkg, root, policy) {
			t.Error(problem)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// Adapter implementations may forward tracker calls; application callers must
// use the ledger. Keep this list file-specific, including method values.
func laneWriterAllowed(file string) bool {
	switch file {
	case "internal/orchestrator/lane_ledger.go", "internal/connector/githublocal/githublocal.go", "internal/connector/github/intake.go", "internal/connector/memory/memory.go", "internal/hubclient/native_connector.go":
		return true
	}
	return false
}

func checkSources(pkg *packages.Package, root string, policy sourcePolicy) []string {
	var problems []string
	allowed := make(map[string]bool)
	for _, reason := range policy.Reasons {
		allowed[reason] = true
	}
	for _, file := range pkg.Syntax {
		name, err := filepath.Rel(root, pkg.Fset.Position(file.Pos()).Filename)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		name = filepath.ToSlash(name)
		directCalls := make(map[ast.Expr]bool)
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				directCalls[call.Fun] = true
			}
			return true
		})
		report := func(pos token.Pos, message string) {
			problems = append(problems, fmt.Sprintf("%s: %s", pkg.Fset.Position(pos), message))
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if _, checked := laneReasonArgument(n.Sel.Name); checked && !directCalls[n] {
					report(n.Pos(), "INV-3 indirect lane reason method reference requires review")
				}
				if selection := pkg.TypesInfo.Selections[n]; selection != nil && selection.Kind() != types.FieldVal && (selection.Obj().Name() == "UpdateIssueState" || selection.Obj().Name() == "SetIssueField") && !laneWriterAllowed(name) {
					report(n.Pos(), "INV-1 tracker lane method reference outside lane ledger/adapters")
				}
			case *ast.Ident:
				if retiredSymbol(n.Name) {
					report(n.Pos(), "INV-9 retired symbol "+n.Name)
				}
			case ast.Expr:
				value := pkg.TypesInfo.Types[n].Value
				if value != nil && value.Kind() == constant.String {
					text := constant.StringVal(value)
					if retiredSymbol(text) {
						report(n.Pos(), "INV-9 retired mechanism string")
					}
					if mutationRateLimit(text) {
						report(n.Pos(), "INV-9 rateLimit in mutation document")
					}
				}
			}
			return true
		})
		if !strings.HasSuffix(pkg.PkgPath, "/internal/orchestrator") {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			dynamic := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				index, checked := laneReasonArgument(sel.Sel.Name)
				if !checked || len(call.Args) <= index {
					return true
				}
				value := pkg.TypesInfo.Types[call.Args[index]].Value
				if value == nil {
					dynamic = true
					return true
				}
				if value.Kind() != constant.String || !allowed[constant.StringVal(value)] {
					report(call.Pos(), "INV-3 unapproved lane reason "+value.ExactString())
				}
				return true
			})
			if dynamic {
				var formatted bytes.Buffer
				if err := format.Node(&formatted, pkg.Fset, fn); err != nil {
					report(fn.Pos(), err.Error())
					continue
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(formatted.Bytes()))
				key := name + ":" + fn.Name.Name
				if policy.Dynamic[key] != digest {
					report(fn.Pos(), fmt.Sprintf("INV-3 dynamic reason function requires review: %q: %q", key, digest))
				}
			}
		}
	}
	return problems
}

func laneReasonArgument(name string) (int, bool) {
	switch name {
	case "updateIssueState":
		return 5, true
	case "updateIssueStateByID", "updateIssueStateByIDWithMetadata", "updateIssueStateByIDStrictWithMetadata", "updateIssueStateByIDWithMetadataMode":
		return 6, true
	case "recordLaneTransition":
		return 4, true
	case "prepareLaneWrite":
		return 3, true
	}
	return 0, false
}

func retiredSymbol(text string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(text))
	for _, forbidden := range []string{"workerlanerevocation", "indeterminatelanestop", "laneindeterminatestop", "stopindeterminatelane", "laneoriginindeterminate"} {
		if strings.Contains(normalized, forbidden) {
			return true
		}
	}
	return false
}

// Tokenize GraphQL names, ignoring comments and quoted strings. A mutation
// document must obtain rateLimit via a separate query, never mutation fields.
func mutationRateLimit(document string) bool {
	var names []string
	for i := 0; i < len(document); {
		c := document[i]
		if c == '#' {
			for i < len(document) && document[i] != '\n' {
				i++
			}
			continue
		}
		if c == '"' {
			if strings.HasPrefix(document[i:], `"""`) {
				i += 3
				for i < len(document) && !strings.HasPrefix(document[i:], `"""`) {
					i++
				}
				if i < len(document) {
					i += 3
				}
			} else {
				i++
				for i < len(document) {
					if document[i] == '\\' {
						i += 2
						continue
					}
					if document[i] == '"' {
						i++
						break
					}
					i++
				}
			}
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			start := i
			for i < len(document) && ((document[i] >= 'a' && document[i] <= 'z') || (document[i] >= 'A' && document[i] <= 'Z') || (document[i] >= '0' && document[i] <= '9') || document[i] == '_') {
				i++
			}
			names = append(names, document[start:i])
			continue
		}
		i++
	}
	mutation, rate := false, false
	for _, name := range names {
		mutation = mutation || name == "mutation"
		rate = rate || name == "rateLimit"
	}
	return mutation && rate
}
