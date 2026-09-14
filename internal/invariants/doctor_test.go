package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// Keep both the runtime registration and public check name in the invariant
// gate. Behavioral evidence is separately required by invariants/policy.json.
func TestDoctorInvariantRegistration(t *testing.T) {
	root := repositoryRoot(t)
	for _, tt := range []struct {
		file, function, callee string
		args                   map[int]string
	}{
		{"doctor_invariant_evidence.go", "checkDoctorInvariantEvidence", "doctorInvariantAdmissionCheck", nil},
		{"doctor_invariant_admission.go", "doctorInvariantAdmissionCheck", "doctorInvariantCheck", map[int]string{1: "INV-11", 2: "human scope approval"}},
	} {
		t.Run(tt.function, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "internal/cli", tt.file), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != tt.function {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					ident, ok := call.Fun.(*ast.Ident)
					if !ok || ident.Name != tt.callee {
						return true
					}
					for index, want := range tt.args {
						if index >= len(call.Args) {
							return true
						}
						lit, ok := call.Args[index].(*ast.BasicLit)
						if !ok {
							return true
						}
						got, err := strconv.Unquote(lit.Value)
						if err != nil || got != want {
							return true
						}
					}
					found = true
					return true
				})
			}
			if !found {
				t.Fatalf("INV-11: %s must call %s with its registered check name", tt.function, tt.callee)
			}
		})
	}
}
