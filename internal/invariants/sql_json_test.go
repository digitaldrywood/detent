package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var inv20JSONFunction = regexp.MustCompile(`(?i)\bjson(?:b)?(?:_(?:array(?:_length)?|each|error_position|extract|group_(?:array|object)|insert|object|patch|pretty|quote|remove|replace|set|tree|type|valid))?\s*\(`)

func TestINV20NoSQLOverJSON(t *testing.T) {
	raw, err := os.ReadFile("../hubserver/testdata/inv20_json_sql_baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("invalid baseline line: %q", line)
		}
		count, err := strconv.Atoi(fields[1])
		if err != nil || count <= 0 {
			t.Fatalf("invalid baseline count: %q", line)
		}
		if _, exists := baseline[fields[0]]; exists {
			t.Fatalf("duplicate baseline path: %s", fields[0])
		}
		baseline[fields[0]] = count
	}
	root := filepath.Join("..", "..")
	for _, directory := range []string{"internal/hubserver", "internal/hubhealth", "internal/hubanalytics", "internal/hubsql", "internal/store", "db/queries"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || (filepath.Ext(path) != ".go" && filepath.Ext(path) != ".sql") {
				return nil
			}
			if strings.Contains(filepath.ToSlash(path), "/migrations/") && entry.Name() < "20261015000000" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			count, err := inv20JSONFunctionCount(path, raw)
			if err != nil {
				return err
			}
			want := baseline[relative]
			if count > want {
				t.Errorf("%s: %d JSON SQL calls exceed baseline %d; use typed columns", relative, count, want)
			}
			if count < want {
				t.Errorf("%s: %d JSON SQL calls below baseline %d; lower the baseline", relative, count, want)
			}
			delete(baseline, relative)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path := range baseline {
		t.Errorf("%s no longer scanned; remove its baseline entry", path)
	}
}

func inv20JSONFunctionCount(path string, raw []byte) (int, error) {
	if filepath.Ext(path) != ".go" {
		return len(inv20JSONFunction.FindAll(raw, -1)), nil
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(literal.Value)
		if unquoteErr != nil {
			err = unquoteErr
			return false
		}
		count += len(inv20JSONFunction.FindAllString(value, -1))
		return true
	})
	return count, err
}

func TestINV20JSONFunctionPatterns(t *testing.T) {
	for _, test := range []struct {
		input    string
		want     int
		goSource bool
	}{
		{"JSON_EXTRACT (data, '$.id')", 1, false},
		{"json(data), jsonb(data), json_quote(data), json_pretty(data)", 4, false},
		{"json_each(data), json_tree(data)", 2, false},
		{"json_set(data), json_insert(data), json_replace(data), json_patch(data), json_remove(data)", 5, false},
		{"json_object(data), json_array(data), json_group_array(data), json_group_object(data)", 4, false},
		{"json_valid(data), json_type(data), json_array_length(data)", 3, false},
		{"not_json_extract(data), json_extract_suffix(data), json_extract", 0, false},
		{"package example; func json_extract(data []byte) {}; func json_type(value any) {}; func use() { json_extract(nil); json_type(nil) }", 0, true},
		{"package example; const query = `SELECT json_extract(data, '$.id')`", 1, true},
		{`package example; const query = "SELECT json_type(data), json_extract(data, '$.id')"`, 2, true},
		{`package example; const query = "SELECT json_extract\n(data, '$.id')"`, 1, true},
	} {
		t.Run(test.input, func(t *testing.T) {
			path := "query.sql"
			if test.goSource {
				path = "query.go"
			}
			got, err := inv20JSONFunctionCount(path, []byte(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %d calls, want %d", got, test.want)
			}
		})
	}
}
