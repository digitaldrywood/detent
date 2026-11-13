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
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

type tenancyPolicy struct {
	Files map[string]map[string]int `json:"files"`
}

const (
	tenancyProcessLaunch       = "process_launch"
	tenancyPoolOpen            = "pool_open"
	tenancyProcessOrganization = "process_organization"
)

var tenancyServingTrees = []string{"internal/cloudentry/", "internal/hubserver/"}

var tenancyPoolOwners = []string{"internal/pgtransport/"}

var tenancyLaunchCalls = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"os":      {"StartProcess"},
	"syscall": {"Exec", "ForkExec", "StartProcess"},
}

var tenancyPoolCalls = map[string][]string{
	"github.com/jackc/pgx/v5/pgxpool": {"New", "NewWithConfig"},
	"github.com/jackc/pgx/v5/stdlib":  {"OpenDB", "OpenDBFromPool"},
	"github.com/jackc/pgx/v5":         {"Connect", "ConnectConfig"},
	"database/sql":                    {"Open", "OpenDB"},
}

var tenancyProcessConfigs = []string{"config", "cfg", "deps"}

func TestRepositoryTenancy(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "internal/invariants/tenancy_policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy tenancyPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkTenancyPolicy(os.DirFS(root), policy) {
		t.Error(problem)
	}
}

func checkTenancyPolicy(root fs.FS, policy tenancyPolicy) []string {
	current, err := scanTenancy(root)
	if err != nil {
		return []string{fmt.Sprintf("tenancy policy: %v", err)}
	}
	var problems []string
	for file, counts := range current {
		for kind, count := range counts {
			if baseline := policy.Files[file][kind]; count > baseline {
				problems = append(problems, fmt.Sprintf("%s: %s count %d exceeds frozen baseline %d; the tenant is never a process, pool or process setting (INV-23, docs/architecture/multi-tenancy.md)", file, kind, count, baseline))
			}
		}
	}
	for file, counts := range policy.Files {
		if len(counts) == 0 {
			problems = append(problems, file+": remove empty tenancy baseline entry")
		}
		for kind, baseline := range counts {
			if count := current[file][kind]; baseline <= 0 || count < baseline {
				problems = append(problems, fmt.Sprintf("%s: shrink %s baseline from %d to %d in internal/invariants/tenancy_policy.json; remove clean entries", file, kind, baseline, count))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func scanTenancy(root fs.FS) (map[string]map[string]int, error) {
	current := make(map[string]map[string]int)
	err := fs.WalkDir(root, ".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if file != "." && tenancySkippedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(file) != ".go" || strings.HasSuffix(file, "_test.go") || !tenancyScanned(file) {
			return nil
		}
		data, err := fs.ReadFile(root, file)
		if err != nil {
			return err
		}
		counts, err := tenancySourceCounts(file, data)
		if err != nil {
			return err
		}
		if len(counts) > 0 {
			current[file] = counts
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan Go sources: %w", err)
	}
	return current, nil
}

func tenancySkippedDirectory(name string) bool {
	switch name {
	case ".git", "node_modules", "tmp", "vendor", "testdata", "testenv", "testsupport", "entryfixture", "hubfixture", "harness":
		return true
	}
	return strings.HasSuffix(name, "test")
}

func tenancyScanned(file string) bool {
	return strings.HasPrefix(file, "internal/") || strings.HasPrefix(file, "cmd/") || strings.HasPrefix(file, "tools/")
}

func tenancyUnder(file string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(file, prefix) })
}

func tenancyPackageName(module string) string {
	name := path.Base(module)
	if len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		return path.Base(path.Dir(module))
	}
	return name
}

func tenancySourceCounts(name string, data []byte) (map[string]int, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse Go source %s: %w", name, err)
	}
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		module, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parse import in %s: %w", name, err)
		}
		alias := tenancyPackageName(module)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = module
	}
	serving := tenancyUnder(name, tenancyServingTrees)
	poolOwner := tenancyUnder(name, tenancyPoolOwners)
	counts := make(map[string]int)
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.Field:
			for _, field := range n.Names {
				if serving && field.Name == "hostedOrganization" {
					counts[tenancyProcessOrganization]++
				}
			}
		case *ast.CallExpr:
			selector, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			module := imports[qualifier.Name]
			if serving && slices.Contains(tenancyLaunchCalls[module], selector.Sel.Name) {
				counts[tenancyProcessLaunch]++
			}
			if !poolOwner && slices.Contains(tenancyPoolCalls[module], selector.Sel.Name) {
				counts[tenancyPoolOpen]++
			}
		case *ast.SelectorExpr:
			if serving && (n.Sel.Name == "hostedOrganization" || n.Sel.Name == "OrganizationID" && tenancyProcessConfig(n.X)) {
				counts[tenancyProcessOrganization]++
			}
		}
		return true
	})
	return counts, nil
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
	aliasedLaunch := "package tenant\nimport run \"os/exec\"\nfunc start() { _ = run.Command(\"detent\") }\n"
	pool := "package store\nimport \"github.com/jackc/pgx/v5/pgxpool\"\nfunc open() { _, _ = pgxpool.NewWithConfig(nil, nil) }\n"
	sqlPool := "package store\nimport \"database/sql\"\nfunc open() { _, _ = sql.Open(\"pgx\", \"\") }\n"
	connect := "package store\nimport \"github.com/jackc/pgx/v5\"\nfunc open() { _, _ = pgx.Connect(nil, \"\") }\n"
	processOrganization := "package hub\nfunc read(s *Service) string { return s.config.Hosted.OrganizationID }\n"
	requestOrganization := "package hub\nfunc read(credential Credential) string { return credential.Hosted.OrganizationID }\n"
	ambientField := "package hub\ntype database struct{ hostedOrganization string }\nfunc (d *database) org() string { return d.hostedOrganization }\n"
	localVariable := "package hub\nfunc read(row map[string]string) string { hostedOrganization := row[\"id\"]; return hostedOrganization }\n"
	for _, tt := range []struct {
		name   string
		files  map[string]string
		policy tenancyPolicy
		want   []string
	}{
		{name: "clean repository", files: map[string]string{"internal/hubserver/clean.go": "package hub\n"}},
		{
			name:  "new process launch in the hosted tree",
			files: map[string]string{"internal/cloudentry/spawn.go": launch},
			want:  []string{"internal/cloudentry/spawn.go: process_launch count 1 exceeds frozen baseline 0"},
		},
		{
			name:  "aliased exec import is still a launch",
			files: map[string]string{"internal/hubserver/spawn.go": aliasedLaunch},
			want:  []string{"internal/hubserver/spawn.go: process_launch count 1 exceeds frozen baseline 0"},
		},
		{
			name:  "process launch outside the hosted tree is out of scope",
			files: map[string]string{"internal/runner/spawn.go": launch},
		},
		{
			name:   "baselined launch passes",
			files:  map[string]string{"internal/cloudentry/tenant/launcher.go": launch},
			policy: tenancyPolicy{Files: map[string]map[string]int{"internal/cloudentry/tenant/launcher.go": {tenancyProcessLaunch: 1}}},
		},
		{
			name:   "removed launch must shrink the baseline",
			files:  map[string]string{"internal/cloudentry/tenant/launcher.go": "package tenant\n"},
			policy: tenancyPolicy{Files: map[string]map[string]int{"internal/cloudentry/tenant/launcher.go": {tenancyProcessLaunch: 1}}},
			want:   []string{"internal/cloudentry/tenant/launcher.go: shrink process_launch baseline from 1 to 0"},
		},
		{
			name:   "new file cannot borrow another file's debt",
			files:  map[string]string{"internal/cloudentry/other.go": launch},
			policy: tenancyPolicy{Files: map[string]map[string]int{"internal/cloudentry/tenant/launcher.go": {tenancyProcessLaunch: 1}}},
			want: []string{
				"internal/cloudentry/other.go: process_launch count 1 exceeds frozen baseline 0",
				"internal/cloudentry/tenant/launcher.go: shrink process_launch baseline from 1 to 0",
			},
		},
		{
			name: "pool construction outside the owner",
			files: map[string]string{
				"internal/hubserver/billing/pool.go": pool,
				"cmd/example/main.go":                sqlPool,
				"tools/example/main.go":              connect,
			},
			want: []string{
				"cmd/example/main.go: pool_open count 1 exceeds frozen baseline 0",
				"internal/hubserver/billing/pool.go: pool_open count 1 exceeds frozen baseline 0",
				"tools/example/main.go: pool_open count 1 exceeds frozen baseline 0",
			},
		},
		{
			name:  "pool owner and test support are exempt",
			files: map[string]string{"internal/pgtransport/pools.go": pool, "internal/testenv/pgtest/postgres.go": pool, "internal/hubserver/hubtest/database.go": pool, "internal/hubserver/pool_test.go": pool},
		},
		{
			name:  "process-wide organization setting",
			files: map[string]string{"internal/hubserver/report.go": processOrganization, "internal/hubserver/billing.go": ambientField},
			want: []string{
				"internal/hubserver/billing.go: process_organization count 2 exceeds frozen baseline 0",
				"internal/hubserver/report.go: process_organization count 1 exceeds frozen baseline 0",
			},
		},
		{
			name:  "request-scoped organization is allowed",
			files: map[string]string{"internal/hubserver/invite.go": requestOrganization, "internal/hubserver/import.go": localVariable},
		},
		{
			name:   "empty baseline entry",
			files:  map[string]string{"internal/hubserver/clean.go": "package hub\n"},
			policy: tenancyPolicy{Files: map[string]map[string]int{"internal/hubserver/clean.go": {}}},
			want:   []string{"internal/hubserver/clean.go: remove empty tenancy baseline entry"},
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
