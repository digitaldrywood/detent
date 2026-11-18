package invariants

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

var tenancyBaseline = flag.Bool("tenancy-baseline", false, "local convenience: rewrite the tenancy policy from the scan; landing protection is scripts/check-tenancy-policy.py")

var tenancyScratchPaths = []string{"tmp", "node_modules", "web/conversation/node_modules"}

const tenancyPolicyFile = "internal/invariants/tenancy_policy.json"

type tenancyCounts map[string]map[string]int

type tenancyPolicy struct {
	TestSupport []string      `json:"test_support"`
	Exceptions  tenancyCounts `json:"exceptions"`
	Debt        tenancyCounts `json:"debt"`
}

var tenancyHostedTrees = []string{"internal/cloudentry/", "internal/hubserver/"}

const tenancyPoolOwner = "internal/pgtransport/"

var tenancyProcessImports = []string{"os/exec", "github.com/digitaldrywood/detent/internal/procgroup"}

var tenancyProcessSymbols = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"os":      {"StartProcess"},
	"syscall": {"Exec", "ForkExec", "StartProcess"},
}

var tenancyPoolConstructors = map[string][]string{
	"github.com/jackc/pgx/v5/pgxpool": {"New", "NewWithConfig"},
	"github.com/jackc/pgx/v5/stdlib":  {"OpenDB", "OpenDBFromPool"},
	"github.com/jackc/pgx/v5":         {"Connect", "ConnectConfig"},
	"database/sql":                    {"Open", "OpenDB"},
}

var tenancyPoolFactories = map[string][]string{
	"github.com/digitaldrywood/detent/internal/pgtransport":        {"Open"},
	"github.com/digitaldrywood/detent/internal/hubserver/postgres": {"Open"},
}

var tenancyTimerSymbols = map[string][]string{
	"time": {"NewTicker", "Tick", "NewTimer", "AfterFunc", "After"},
}

var tenancyOrganizationFields = []string{"hostedOrganization", "localOrganization", "appOrganizationID", "accessOrganization"}

var tenancyProcessConfigs = []string{"config", "cfg", "deps"}

func TestRepositoryTenancy(t *testing.T) {
	root := repositoryRoot(t)
	if *tenancyBaseline {
		if err := rewriteTenancyPolicy(root); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Parallel()
	policy, err := readTenancyPolicy(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkTenancyPolicy(os.DirFS(root), policy) {
		t.Error(problem)
	}
}

func readTenancyPolicy(root fs.FS) (tenancyPolicy, error) {
	var policy tenancyPolicy
	data, err := fs.ReadFile(root, tenancyPolicyFile)
	if err != nil {
		return policy, err
	}
	err = json.Unmarshal(data, &policy)
	return policy, err
}

func checkTenancyPolicy(root fs.FS, policy tenancyPolicy) []string {
	exceptions, debt, err := scanTenancy(root, policy.TestSupport)
	if err != nil {
		return []string{fmt.Sprintf("tenancy policy: %v", err)}
	}
	var problems []string
	seen := map[string]bool{}
	for _, directory := range policy.TestSupport {
		if seen[directory] {
			problems = append(problems, directory+": duplicate test-support entry")
		}
		seen[directory] = true
		if directory == "" || path.Clean(directory) != directory || strings.HasPrefix(directory, "/") || strings.HasPrefix(directory, "..") {
			problems = append(problems, directory+": test-support entry must be a clean relative directory")
		}
		if info, err := fs.Stat(root, directory); err != nil || !info.IsDir() {
			problems = append(problems, directory+": remove stale test-support entry")
		}
	}
	problems = append(problems, tenancyCountProblems("exception", exceptions, policy.Exceptions)...)
	problems = append(problems, tenancyCountProblems("debt", debt, policy.Debt)...)
	slices.Sort(problems)
	return problems
}

func tenancyCountProblems(section string, current, baseline tenancyCounts) []string {
	var problems []string
	for file, counts := range current {
		for symbol, count := range counts {
			if allowed := baseline[file][symbol]; count > allowed {
				problems = append(problems, fmt.Sprintf("%s: %s count %d exceeds %s baseline %d; see INV-23 and docs/architecture/multi-tenancy.md", file, symbol, count, section, allowed))
			}
		}
	}
	for file, counts := range baseline {
		if len(counts) == 0 {
			problems = append(problems, fmt.Sprintf("%s: remove empty %s entry", file, section))
		}
		for symbol, allowed := range counts {
			if count := current[file][symbol]; allowed <= 0 || count < allowed {
				problems = append(problems, fmt.Sprintf("%s: lower %s %s baseline from %d to %d in %s", file, section, symbol, allowed, count, tenancyPolicyFile))
			}
		}
	}
	return problems
}

func rewriteTenancyPolicy(root string) error {
	command := exec.Command("git", "show", "HEAD:"+tenancyPolicyFile)
	command.Dir = root
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	committedData, err := command.Output()
	if err != nil {
		return fmt.Errorf("read committed tenancy policy: %w", err)
	}
	var committed tenancyPolicy
	if err := json.Unmarshal(committedData, &committed); err != nil {
		return fmt.Errorf("decode committed tenancy policy: %w", err)
	}
	current, err := readTenancyPolicy(os.DirFS(root))
	if err != nil {
		return err
	}
	data, err := tenancyBaselineData(os.DirFS(root), committed, current.TestSupport)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, tenancyPolicyFile), data, 0o644)
}

func tenancyBaselineData(root fs.FS, committed tenancyPolicy, testSupport []string) ([]byte, error) {
	seen := map[string]bool{}
	for _, directory := range testSupport {
		if seen[directory] || !slices.Contains(committed.TestSupport, directory) {
			return nil, fmt.Errorf("refuse tenancy test-support entry %q: every exemption must be a distinct member of the committed set", directory)
		}
		seen[directory] = true
	}
	exceptions, debt, err := scanTenancy(root, testSupport)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, problem := range append(tenancyCountProblems("exception", exceptions, committed.Exceptions), tenancyCountProblems("debt", debt, committed.Debt)...) {
		if strings.Contains(problem, " exceeds ") {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, fmt.Errorf("refuse tenancy baseline growth:\n%s", strings.Join(problems, "\n"))
	}
	data, err := json.MarshalIndent(tenancyPolicy{TestSupport: testSupport, Exceptions: exceptions, Debt: debt}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func scanTenancy(root fs.FS, testSupport []string) (tenancyCounts, tenancyCounts, error) {
	exceptions, debt := tenancyCounts{}, tenancyCounts{}
	err := fs.WalkDir(root, ".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if file != "." && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || slices.Contains(tenancyScratchPaths, file) || slices.Contains(testSupport, file)) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(file) != ".go" || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(root, file)
		if err != nil {
			return err
		}
		return tenancySourceCounts(file, data, exceptions, debt)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scan Go sources: %w", err)
	}
	return exceptions, debt, nil
}

func tenancyPackageName(module string) string {
	name := path.Base(module)
	if len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		return path.Base(path.Dir(module))
	}
	return name
}

func tenancyAdd(counts tenancyCounts, file, symbol string) {
	if counts[file] == nil {
		counts[file] = map[string]int{}
	}
	counts[file][symbol]++
}

func tenancySourceCounts(name string, data []byte, exceptions, debt tenancyCounts) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse Go source %s: %w", name, err)
	}
	hosted := slices.ContainsFunc(tenancyHostedTrees, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
	poolOwner := strings.HasPrefix(name, tenancyPoolOwner)
	processCounts := exceptions
	if hosted {
		processCounts = debt
	}
	imports := map[string]string{}
	var dotImports []string
	for _, spec := range file.Imports {
		module, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("parse import in %s: %w", name, err)
		}
		alias := tenancyPackageName(module)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias == "." {
			dotImports = append(dotImports, module)
		} else {
			imports[alias] = module
		}
		if slices.Contains(tenancyProcessImports, module) {
			tenancyAdd(processCounts, name, "import:"+module)
		}
	}
	reference := func(module, symbol string) {
		label := tenancyPackageName(module) + "." + symbol
		switch {
		case slices.Contains(tenancyProcessSymbols[module], symbol):
			tenancyAdd(processCounts, name, "process:"+label)
		case slices.Contains(tenancyPoolConstructors[module], symbol):
			if poolOwner {
				tenancyAdd(exceptions, name, "pool:"+label)
			} else {
				tenancyAdd(debt, name, "pool:"+label)
			}
		case slices.Contains(tenancyPoolFactories[module], symbol):
			tenancyAdd(exceptions, name, "pool:"+label)
		case hosted && slices.Contains(tenancyTimerSymbols[module], symbol):
			tenancyAdd(debt, name, "timer:"+label)
		}
	}
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.Field:
			for _, field := range n.Names {
				if hosted && slices.Contains(tenancyOrganizationFields, field.Name) {
					tenancyAdd(debt, name, "organization:"+field.Name)
				}
			}
		case *ast.FuncDecl:
			if hosted && n.Recv != nil && slices.Contains(tenancyOrganizationFields, n.Name.Name) {
				tenancyAdd(debt, name, "organization:"+n.Name.Name)
			}
		case *ast.SelectorExpr:
			if qualifier, ok := n.X.(*ast.Ident); ok {
				if module, ok := imports[qualifier.Name]; ok {
					reference(module, n.Sel.Name)
					return false
				}
			}
			if hosted && slices.Contains(tenancyOrganizationFields, n.Sel.Name) {
				tenancyAdd(debt, name, "organization:"+n.Sel.Name)
			}
			if hosted && n.Sel.Name == "OrganizationID" && tenancyProcessConfig(n.X) {
				tenancyAdd(debt, name, "organization:Hosted.OrganizationID")
			}
			ast.Inspect(n.X, visit)
			return false
		case *ast.Ident:
			for _, module := range dotImports {
				reference(module, n.Name)
			}
		}
		return true
	}
	ast.Inspect(file, visit)
	return nil
}

func tenancyProcessConfig(expression ast.Expr) bool {
	hosted, ok := expression.(*ast.SelectorExpr)
	if !ok || hosted.Sel.Name != "Hosted" {
		return false
	}
	switch owner := hosted.X.(type) {
	case *ast.Ident:
		return slices.Contains(tenancyProcessConfigs, owner.Name)
	case *ast.SelectorExpr:
		return slices.Contains(tenancyProcessConfigs, owner.Sel.Name)
	}
	return false
}

func TestTenancyPolicyViolations(t *testing.T) {
	t.Parallel()
	launch := "package tenant\nimport \"os/exec\"\nfunc start() { _ = exec.CommandContext(nil, \"detent\", \"hub\", \"serve\") }\n"
	launchDebt := map[string]int{"import:os/exec": 1, "process:exec.CommandContext": 1}
	for _, tt := range []struct {
		name   string
		files  map[string]string
		policy tenancyPolicy
		want   []string
	}{
		{name: "clean repository", files: map[string]string{"internal/hubserver/clean.go": "package hub\n"}},
		{
			name:  "process launch in the hosted tree is debt",
			files: map[string]string{"internal/cloudentry/spawn.go": launch},
			want: []string{
				"internal/cloudentry/spawn.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/cloudentry/spawn.go: process:exec.CommandContext count 1 exceeds debt baseline 0",
			},
		},
		{
			name:  "aliased import and function value",
			files: map[string]string{"internal/hubserver/spawn.go": "package hub\nimport run \"os/exec\"\nvar start = run.Command\n"},
			want: []string{
				"internal/hubserver/spawn.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/hubserver/spawn.go: process:exec.Command count 1 exceeds debt baseline 0",
			},
		},
		{
			name:  "dot import",
			files: map[string]string{"internal/hubserver/spawn.go": "package hub\nimport . \"os/exec\"\nfunc start() { _ = Command(\"detent\") }\n"},
			want: []string{
				"internal/hubserver/spawn.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/hubserver/spawn.go: process:exec.Command count 1 exceeds debt baseline 0",
			},
		},
		{
			name:  "process group wrapper in the hosted tree",
			files: map[string]string{"internal/hubserver/spawn.go": "package hub\nimport \"github.com/digitaldrywood/detent/internal/procgroup\"\nvar _ = procgroup.Usage{}\n"},
			want:  []string{"internal/hubserver/spawn.go: import:github.com/digitaldrywood/detent/internal/procgroup count 1 exceeds debt baseline 0"},
		},
		{
			name:  "new helper package outside the hosted tree needs a named exception",
			files: map[string]string{"internal/launchhelper/run.go": launch},
			want: []string{
				"internal/launchhelper/run.go: import:os/exec count 1 exceeds exception baseline 0",
				"internal/launchhelper/run.go: process:exec.CommandContext count 1 exceeds exception baseline 0",
			},
		},
		{
			name:   "named exception passes",
			files:  map[string]string{"internal/runner/run.go": launch},
			policy: tenancyPolicy{Exceptions: tenancyCounts{"internal/runner/run.go": launchDebt}},
		},
		{
			name:   "exception cannot hide hosted debt",
			files:  map[string]string{"internal/cloudentry/spawn.go": launch},
			policy: tenancyPolicy{Exceptions: tenancyCounts{"internal/cloudentry/spawn.go": launchDebt}},
			want: []string{
				"internal/cloudentry/spawn.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/cloudentry/spawn.go: lower exception import:os/exec baseline from 1 to 0",
				"internal/cloudentry/spawn.go: lower exception process:exec.CommandContext baseline from 1 to 0",
				"internal/cloudentry/spawn.go: process:exec.CommandContext count 1 exceeds debt baseline 0",
			},
		},
		{
			name:   "removed debt must lower the baseline",
			files:  map[string]string{"internal/cloudentry/tenant/launcher.go": "package tenant\n"},
			policy: tenancyPolicy{Debt: tenancyCounts{"internal/cloudentry/tenant/launcher.go": {"process:exec.CommandContext": 1}}},
			want:   []string{"internal/cloudentry/tenant/launcher.go: lower debt process:exec.CommandContext baseline from 1 to 0"},
		},
		{
			name:   "a different violation cannot replace removed debt",
			files:  map[string]string{"internal/cloudentry/tenant/launcher.go": "package tenant\nimport \"os\"\nfunc start() { _, _ = os.StartProcess(\"detent\", nil, nil) }\n"},
			policy: tenancyPolicy{Debt: tenancyCounts{"internal/cloudentry/tenant/launcher.go": {"process:exec.CommandContext": 1}}},
			want: []string{
				"internal/cloudentry/tenant/launcher.go: lower debt process:exec.CommandContext baseline from 1 to 0",
				"internal/cloudentry/tenant/launcher.go: process:os.StartProcess count 1 exceeds debt baseline 0",
			},
		},
		{
			name: "pool constructors outside the owner are debt; the owner and factories are exceptions",
			files: map[string]string{
				"internal/hubserver/billing/pool.go": "package billing\nimport \"github.com/jackc/pgx/v5/pgxpool\"\nfunc open() { _, _ = pgxpool.NewWithConfig(nil, nil) }\n",
				"cmd/example/main.go":                "package main\nimport \"database/sql\"\nfunc main() { _, _ = sql.Open(\"pgx\", \"\") }\n",
				"tools/example/main.go":              "package main\nimport \"github.com/jackc/pgx/v5\"\nfunc main() { _, _ = pgx.Connect(nil, \"\") }\n",
				"internal/pgtransport/pools.go":      "package pgtransport\nimport \"github.com/jackc/pgx/v5/pgxpool\"\nfunc open() { _, _ = pgxpool.New(nil, \"\") }\n",
				"internal/hostexample/host.go":       "package hostexample\nimport \"github.com/digitaldrywood/detent/internal/pgtransport\"\nfunc open() { _, _ = pgtransport.Open(nil, nil, nil) }\n",
			},
			want: []string{
				"cmd/example/main.go: pool:sql.Open count 1 exceeds debt baseline 0",
				"internal/hostexample/host.go: pool:pgtransport.Open count 1 exceeds exception baseline 0",
				"internal/hubserver/billing/pool.go: pool:pgxpool.NewWithConfig count 1 exceeds debt baseline 0",
				"internal/pgtransport/pools.go: pool:pgxpool.New count 1 exceeds exception baseline 0",
				"tools/example/main.go: pool:pgx.Connect count 1 exceeds debt baseline 0",
			},
		},
		{
			name:  "directories named like tests are scanned unless listed",
			files: map[string]string{"internal/hubserver/sourcetest/source.go": launch},
			want: []string{
				"internal/hubserver/sourcetest/source.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/hubserver/sourcetest/source.go: process:exec.CommandContext count 1 exceeds debt baseline 0",
			},
		},
		{
			name:   "listed test support and test files are exempt",
			files:  map[string]string{"internal/hubserver/sourcetest/source.go": launch, "internal/hubserver/spawn_test.go": launch},
			policy: tenancyPolicy{TestSupport: []string{"internal/hubserver/sourcetest"}},
		},
		{
			name:   "stale test support entry",
			files:  map[string]string{"internal/hubserver/clean.go": "package hub\n"},
			policy: tenancyPolicy{TestSupport: []string{"internal/hubserver/removedtest"}},
			want:   []string{"internal/hubserver/removedtest: remove stale test-support entry"},
		},
		{
			name:   "duplicate and widened test support entries",
			files:  map[string]string{"internal/hubserver/hubtest/db.go": "package hubtest\n"},
			policy: tenancyPolicy{TestSupport: []string{"internal/hubserver/hubtest", "internal/hubserver/hubtest", "internal/hubserver/../hubserver"}},
			want: []string{
				"internal/hubserver/../hubserver: remove stale test-support entry",
				"internal/hubserver/../hubserver: test-support entry must be a clean relative directory",
				"internal/hubserver/hubtest: duplicate test-support entry",
			},
		},
		{
			name:  "only named scratch paths are skipped",
			files: map[string]string{"tmp/scratch.go": launch, "internal/hubserver/tmp/spawn.go": launch},
			want: []string{
				"internal/hubserver/tmp/spawn.go: import:os/exec count 1 exceeds debt baseline 0",
				"internal/hubserver/tmp/spawn.go: process:exec.CommandContext count 1 exceeds debt baseline 0",
			},
		},
		{
			name:  "hosted timers are debt; timers elsewhere are not counted",
			files: map[string]string{"internal/hubserver/loop.go": "package hub\nimport \"time\"\nvar tick = time.NewTicker\n", "internal/runner/loop.go": "package runner\nimport \"time\"\nvar tick = time.NewTicker\n"},
			want:  []string{"internal/hubserver/loop.go: timer:time.NewTicker count 1 exceeds debt baseline 0"},
		},
		{
			name: "process-wide organization reads",
			files: map[string]string{
				"internal/hubserver/report.go":  "package hub\nfunc read(s *Service) string { return s.config.Hosted.OrganizationID }\n",
				"internal/hubserver/billing.go": "package hub\ntype database struct{ hostedOrganization string }\nfunc (d *database) org() string { return d.hostedOrganization }\nfunc (s *Service) accessOrganization() string { return \"\" }\n",
			},
			want: []string{
				"internal/hubserver/billing.go: organization:accessOrganization count 1 exceeds debt baseline 0",
				"internal/hubserver/billing.go: organization:hostedOrganization count 2 exceeds debt baseline 0",
				"internal/hubserver/report.go: organization:Hosted.OrganizationID count 1 exceeds debt baseline 0",
			},
		},
		{
			name: "request-scoped organization and local variables are allowed",
			files: map[string]string{
				"internal/hubserver/invite.go": "package hub\nfunc read(credential Credential) string { return credential.Hosted.OrganizationID }\n",
				"internal/hubserver/import.go": "package hub\nfunc read(row map[string]string) string { hostedOrganization := row[\"id\"]; return hostedOrganization }\n",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := fstest.MapFS{}
			for name, source := range tt.files {
				root[name] = &fstest.MapFile{Data: []byte(source)}
			}
			problems := checkTenancyPolicy(root, tt.policy)
			if len(problems) != len(tt.want) {
				t.Fatalf("got problems %q, want %q", problems, tt.want)
			}
			for i, want := range tt.want {
				if !strings.HasPrefix(problems[i], want) {
					t.Errorf("problem %d = %q, want prefix %q", i, problems[i], want)
				}
			}
		})
	}
}

func TestTenancyBaselineRefusesGrowth(t *testing.T) {
	t.Parallel()
	launch := "package tenant\nimport \"os/exec\"\nfunc start() { _ = exec.CommandContext(nil, \"detent\") }\n"
	for _, tt := range []struct {
		name        string
		files       map[string]string
		committed   tenancyPolicy
		testSupport []string
		want        string
	}{
		{name: "new debt", files: map[string]string{"internal/cloudentry/spawn.go": launch}, want: "exceeds debt baseline 0"},
		{name: "new exception", files: map[string]string{"internal/runner/run.go": launch}, want: "exceeds exception baseline 0"},
		{name: "new test support", testSupport: []string{"internal/hubserver"}, want: "every exemption must be a distinct member"},
		{name: "equal-count substitution", committed: tenancyPolicy{TestSupport: []string{"internal/hubserver/hubtest"}}, testSupport: []string{"internal/hubserver"}, want: "every exemption must be a distinct member"},
		{name: "duplicate test support", committed: tenancyPolicy{TestSupport: []string{"internal/hubserver/hubtest", "internal/hubserver/other"}}, testSupport: []string{"internal/hubserver/hubtest", "internal/hubserver/hubtest"}, want: "every exemption must be a distinct member"},
		{
			name:      "shrinking rewrites",
			files:     map[string]string{"internal/cloudentry/tenant/launcher.go": "package tenant\n"},
			committed: tenancyPolicy{Debt: tenancyCounts{"internal/cloudentry/tenant/launcher.go": {"process:exec.CommandContext": 1}}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := fstest.MapFS{}
			for name, source := range tt.files {
				root[name] = &fstest.MapFile{Data: []byte(source)}
			}
			data, err := tenancyBaselineData(root, tt.committed, tt.testSupport)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) || data != nil {
					t.Fatalf("got %s, %v; want refusal containing %q", data, err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := "{\n  \"test_support\": null,\n  \"exceptions\": {},\n  \"debt\": {}\n}\n"; string(data) != want {
				t.Fatalf("got %s, want %s", data, want)
			}
		})
	}
}
