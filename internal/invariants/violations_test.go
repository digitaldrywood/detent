package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestSourceViolations(t *testing.T) {
	for _, tt := range []struct{ name, file, body, want string }{
		{"ledger", "internal/orchestrator/lane_ledger.go", `func f(c Tracker) { c.UpdateIssueState() }`, ""},
		{"caller", "internal/runner/rogue.go", `func f(c Tracker) { c.UpdateIssueState() }`, "INV-1"},
		{"method value", "internal/runner/rogue.go", `func f(c Tracker) { write := c.UpdateIssueState; write() }`, "INV-1"},
		{"field writer", "internal/runner/rogue.go", `func f(c Tracker) { c.SetIssueField() }`, "INV-1"},
		{"capability flag", "internal/web/capability.go", `var c struct { UpdateIssueState bool }; var enabled = c.UpdateIssueState`, ""},
		{"known reason", "internal/orchestrator/new.go", `func f(o Owner) { o.updateIssueState(0,0,0,0,0,"dispatch_start") }`, ""},
		{"new reason", "internal/orchestrator/new.go", `func f(o Owner) { o.updateIssueState(0,0,0,0,0,"new_brake") }`, "INV-3"},
		{"constant reason", "internal/orchestrator/new.go", `const reason = "new_"+"brake"; func f(o Owner) { o.updateIssueState(0,0,0,0,0,reason) }`, "INV-3"},
		{"saved reason method", "internal/orchestrator/new.go", `func f(o Owner) { write := o.updateIssueState; write(0,0,0,0,0,"new_brake") }`, "INV-3"},
		{"dynamic reason", "internal/orchestrator/new.go", `func f(o Owner, reason string) { o.updateIssueState(0,0,0,0,0,reason) }`, "INV-3"},
		{"revocation", "internal/runner/new.go", `const reason = "worker_lane_revocation"`, "INV-9"},
		{"split revocation", "internal/runner/new.go", `const reason = "worker_lane_"+"revocation"`, "INV-9"},
		{"lane stop", "internal/runner/new.go", `func stopIndeterminateLane() {}`, "INV-9"},
		{"mutation", "internal/connector/github/new.go", "const document = `mutation { updateIssue { id } rateLimit { cost } }`", "INV-9"},
		{"query", "internal/connector/github/new.go", "const document = `query { rateLimit { cost } }`", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fset := token.NewFileSet()
			source := "package fixture\ntype Tracker interface { UpdateIssueState(); SetIssueField() }; type Owner interface { updateIssueState(int,int,int,int,int,string) };\n" + tt.body
			file, err := parser.ParseFile(fset, filepath.Join(root, tt.file), source, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
			cfg := types.Config{}
			pkg, err := cfg.Check("fixture", fset, []*ast.File{file}, info)
			if err != nil {
				t.Fatal(err)
			}
			loaded := &packages.Package{PkgPath: "fixture/internal/orchestrator", Types: pkg, Fset: fset, Syntax: []*ast.File{file}, TypesInfo: info}
			problems := checkSources(loaded, root, sourcePolicy{Reasons: []string{"dispatch_start"}})
			if tt.want == "" && len(problems) != 0 {
				t.Fatalf("unexpected: %v", problems)
			}
			if tt.want != "" && !strings.Contains(strings.Join(problems, "\n"), tt.want) {
				t.Fatalf("got %v, want %s failure", problems, tt.want)
			}
		})
	}
}

func TestMutationDocuments(t *testing.T) {
	for _, tt := range []struct {
		name, document string
		forbidden      bool
	}{
		{"mutation root", `mutation M { updateIssue { id } rateLimit { cost } }`, true},
		{"alias", `mutation M { budget: rateLimit { cost } }`, true},
		{"fragment", `mutation M { ...Budget } fragment Budget on Mutation { rateLimit { cost } }`, true},
		{"query", `query { rateLimit { cost } }`, false},
		{"comment", "mutation { updateIssue { id } } # rateLimit", false},
		{"quoted", `mutation { updateIssue(body:"rateLimit") { id } }`, false},
		{"block string", `mutation { updateIssue(body:"""rateLimit""") { id } }`, false},
		{"escaped quote", `mutation { updateIssue(body:"a\"rateLimit") { id } }`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := mutationRateLimit(tt.document); got != tt.forbidden {
				t.Fatalf("got %t, want %t", got, tt.forbidden)
			}
		})
	}
}
