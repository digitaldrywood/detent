package invariants

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var sqliteShrink = flag.Bool("sqlite-shrink", false, "shrink SQLite policy against the integration baseline")

func sqliteGit(root string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	return command.Output()
}

func integrationSQLiteBaseline(root string) ([]byte, error) {
	base, err := sqliteGit(root, "merge-base", "HEAD", "origin/develop")
	ref := strings.TrimSpace(string(base))
	if err != nil || ref == "" {
		ref = "origin/develop"
	}
	refs := []string{ref}
	if ref != "origin/develop" {
		refs = append(refs, "origin/develop")
	}
	for _, candidate := range refs {
		entry, err := sqliteGit(root, "ls-tree", "--name-only", candidate, "--", "internal/invariants/sqlite_policy.json")
		if err != nil {
			return nil, fmt.Errorf("inspect integration SQLite baseline: %w", err)
		}
		if len(bytes.TrimSpace(entry)) > 0 {
			data, err := sqliteGit(root, "show", candidate+":internal/invariants/sqlite_policy.json")
			if err != nil {
				return nil, fmt.Errorf("read integration SQLite baseline: %w", err)
			}
			return data, nil
		}
	}
	archive, err := sqliteGit(root, "archive", "--format=tar", ref)
	if err != nil {
		return nil, fmt.Errorf("read initial integration source: %w", err)
	}
	files := fstest.MapFS{}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read integration source archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read integration source file: %w", err)
		}
		files[header.Name] = &fstest.MapFile{Data: data}
	}
	policy, err := scanSQLite(files)
	if err != nil {
		return nil, fmt.Errorf("scan initial integration source: %w", err)
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("encode initial integration baseline: %w", err)
	}
	return data, nil
}

func checkSQLiteRepository(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, "internal/invariants/sqlite_policy.json"))
	if err != nil {
		return []string{err.Error()}
	}
	var policy sqlitePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return []string{err.Error()}
	}
	data, err = integrationSQLiteBaseline(root)
	if err != nil {
		return []string{err.Error()}
	}
	var baseline sqlitePolicy
	if err := json.Unmarshal(data, &baseline); err != nil {
		return []string{fmt.Sprintf("decode integration SQLite baseline: %v", err)}
	}
	problems := checkSQLitePolicy(os.DirFS(root), policy)
	problems = append(problems, sqliteGrowthProblems(policy, baseline)...)
	if len(problems) == 0 && len(policy.Files) == 0 && len(policy.Modules) == 0 {
		if err := checkSQLiteDependencies(root); err != nil {
			problems = append(problems, err.Error())
		}
	}
	slices.Sort(problems)
	return problems
}

func checkSQLiteDependencies(root string) error {
	command := exec.Command("go", "list", "-deps", "-test", "-mod=readonly", "-p", "2", "-f", "{{.ImportPath}}", "./...")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("list final Go dependencies: %w: %s", err, output)
	}
	var forbidden []string
	for line := range strings.SplitSeq(string(output), "\n") {
		if sqliteDriver(line) {
			forbidden = append(forbidden, line)
		}
	}
	if len(forbidden) > 0 {
		slices.Sort(forbidden)
		return fmt.Errorf("SQLite remains in final Go dependencies: %s", strings.Join(forbidden, ", "))
	}
	return nil
}

func shrinkSQLitePolicy(root string) error {
	committed, err := integrationSQLiteBaseline(root)
	if err != nil {
		return err
	}
	data, err := sqliteShrinkData(os.DirFS(root), committed)
	if err != nil {
		return err
	}
	var policy sqlitePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return fmt.Errorf("decode shrunken SQLite baseline: %w", err)
	}
	if len(policy.Files) == 0 && len(policy.Modules) == 0 {
		if err := checkSQLiteDependencies(root); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "internal/invariants/sqlite_policy.json"), data, 0o644); err != nil {
		return fmt.Errorf("write SQLite baseline: %w", err)
	}
	return nil
}

func sqliteShrinkData(root fs.FS, committed []byte) ([]byte, error) {
	var baseline sqlitePolicy
	if err := json.Unmarshal(committed, &baseline); err != nil {
		return nil, fmt.Errorf("decode integration SQLite baseline: %w", err)
	}
	current, err := scanSQLite(root)
	if err != nil {
		return nil, err
	}
	problems := sqliteGrowthProblems(current, baseline)
	if len(problems) > 0 {
		return nil, fmt.Errorf("refuse SQLite baseline growth:\n%s", strings.Join(problems, "\n"))
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode SQLite baseline: %w", err)
	}
	return append(data, '\n'), nil
}

func sqliteGrowthProblems(current, baseline sqlitePolicy) []string {
	var problems []string
	for file, counts := range current.Files {
		for kind, count := range counts {
			if count > baseline.Files[file][kind] {
				problems = append(problems, fmt.Sprintf("%s: SQLite %s count %d exceeds integration baseline %d", file, kind, count, baseline.Files[file][kind]))
			}
		}
	}
	for file, counts := range current.Modules {
		for module, count := range counts {
			if count > baseline.Modules[file][module] {
				problems = append(problems, fmt.Sprintf("%s: SQLite %s count %d exceeds integration baseline %d", file, module, count, baseline.Modules[file][module]))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func TestSQLiteShrinkData(t *testing.T) {
	t.Parallel()
	driver := "modernc.org/sqlite"
	importSource := "package fixture\nimport _ " + fmt.Sprintf("%q", driver)
	file := "internal/example/old.go"
	empty := "{\n  \"files\": {},\n  \"modules\": {}\n}\n"
	for _, tt := range []struct {
		name      string
		files     map[string]string
		committed sqlitePolicy
		want      string
		wantJSON  string
	}{
		{name: "zero baseline", wantJSON: empty},
		{
			name: "remove all debt", files: map[string]string{file: "package fixture"}, wantJSON: empty,
			committed: sqlitePolicy{Files: map[string]map[string]int{file: {"driver_import": 1}}, Modules: map[string]map[string]int{"go.mod": {driver: 1}, "go.sum": {driver: 2}}},
		},
		{
			name: "new file cannot borrow removed debt", files: map[string]string{"internal/example/new.go": importSource}, want: "internal/example/new.go: SQLite driver_import count 1 exceeds integration baseline 0",
			committed: sqlitePolicy{Files: map[string]map[string]int{file: {"driver_import": 1}}},
		},
		{
			name: "new kind cannot borrow removed debt", files: map[string]string{file: "package fixture; const BackendSQLite = 0"}, want: "backend_sqlite count 1 exceeds integration baseline 0",
			committed: sqlitePolicy{Files: map[string]map[string]int{file: {"driver_import": 1}}},
		},
		{
			name: "count cannot increase", files: map[string]string{file: importSource + "\nimport _ \"github.com/mattn/go-sqlite3\""}, want: "driver_import count 2 exceeds integration baseline 1",
			committed: sqlitePolicy{Files: map[string]map[string]int{file: {"driver_import": 1}}},
		},
		{
			name: "edited policy cannot authorize growth", files: map[string]string{
				file:                                     importSource,
				"internal/invariants/sqlite_policy.json": `{"files":{"internal/example/old.go":{"driver_import":100}},"modules":{}}`,
			}, want: "driver_import count 1 exceeds integration baseline 0",
		},
		{
			name: "checksum debt cannot move from go.mod", files: map[string]string{"go.sum": driver + " v1.0.0/go.mod h1:example\n"}, want: "go.sum: SQLite " + driver + " count 1 exceeds integration baseline 0",
			committed: sqlitePolicy{Modules: map[string]map[string]int{"go.mod": {driver: 1}}},
		},
		{
			name: "partial reduction has deterministic JSON", files: map[string]string{file: importSource},
			committed: sqlitePolicy{Files: map[string]map[string]int{file: {"driver_import": 2}}},
			wantJSON:  "{\n  \"files\": {\n    \"internal/example/old.go\": {\n      \"driver_import\": 1\n    }\n  },\n  \"modules\": {}\n}\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			files := map[string]string{"go.mod": "module example.org/project\ngo 1.26.0\n"}
			for name, data := range tt.files {
				files[name] = data
			}
			for name, data := range files {
				file := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			committed, err := json.Marshal(tt.committed)
			if err != nil {
				t.Fatal(err)
			}
			data, err := sqliteShrinkData(os.DirFS(root), committed)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) || data != nil {
					t.Fatalf("got %q, %v; want refusal containing %q and no replacement data", data, err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.wantJSON {
				t.Fatalf("got %s, want %s", data, tt.wantJSON)
			}
		})
	}
}
