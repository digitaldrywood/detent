package configdoc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestConfigDocumentation(t *testing.T) {
	fields, nodes, err := build()
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}

	t.Run("covers config package YAML keys", func(t *testing.T) {
		sourceKeys := configSourceYAMLKeys(t)
		generatedKeys := make(map[string]struct{}, len(fields))
		for _, field := range fields {
			path := strings.TrimSuffix(field.Path, "[]")
			key := path[strings.LastIndex(path, ".")+1:]
			generatedKeys[key] = struct{}{}
		}

		var missing []string
		for key := range sourceKeys {
			if _, ok := generatedKeys[key]; !ok {
				missing = append(missing, key)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Fatalf("generated fields omit YAML keys: %s", strings.Join(missing, ", "))
		}
	})

	t.Run("includes discoverable backlog admission", func(t *testing.T) {
		markdown := renderMarkdown(fields)
		reference := string(renderReferenceYAML(fields, nodes))
		for name, content := range map[string]string{
			"Markdown": markdown,
			"YAML":     reference,
		} {
			if !strings.Contains(content, "backlog_admission.schedule") {
				t.Errorf("%s does not include backlog_admission.schedule", name)
			}
			if !strings.Contains(content, "valid five-field cron expression") {
				t.Errorf("%s does not include backlog admission cron validation", name)
			}
		}
	})

	t.Run("uses validator constraints and conditional requiredness", func(t *testing.T) {
		byPath := make(map[string]fieldDetails, len(fields))
		for _, field := range fields {
			byPath[field.Path] = field
		}
		tests := []struct {
			path       string
			required   string
			ruleDetail string
		}{
			{path: "tracker.kind", required: "Yes", ruleDetail: "must be one of"},
			{path: "identity.name", required: "Conditional", ruleDetail: "must not be blank"},
			{path: "agents.backends[].provider", required: "No", ruleDetail: "sanitized label"},
			{path: "agents.backends[].protocol", required: "No", ruleDetail: "must be app-server"},
			{path: "tracker.github_app_id", required: "Conditional", ruleDetail: "required for github app"},
			{path: "backlog_admission.target_state", required: "Conditional", ruleDetail: "configured workflow state"},
		}
		for _, test := range tests {
			test := test
			t.Run(test.path, func(t *testing.T) {
				field, ok := byPath[test.path]
				if !ok {
					t.Fatalf("field %q is missing", test.path)
				}
				if field.Required != test.required {
					t.Errorf("Required = %q, want %q", field.Required, test.required)
				}
				if !strings.Contains(strings.Join(field.Validation, "; "), test.ruleDetail) {
					t.Errorf("Validation = %q, want containing %q", field.Validation, test.ruleDetail)
				}
			})
		}
		if shell := byPath["agents.backends[].options.shell"]; shell.literal != "null" {
			t.Errorf("backend shell literal = %q, want symbolic null", shell.literal)
		}
	})

	t.Run("normalizes generated documentation line endings", func(t *testing.T) {
		content := []byte("before\r\n" + beginMarker + "\r\nold\r\n" + endMarker + "\r\n")
		rendered, err := renderDocs(normalizeLineEndings(content), "new")
		if err != nil {
			t.Fatalf("renderDocs() error = %v", err)
		}
		if strings.Contains(string(rendered), "\r") {
			t.Fatalf("renderDocs() retained carriage returns: %q", rendered)
		}
	})

	t.Run("generated artifacts are current", func(t *testing.T) {
		root := filepath.Clean(filepath.Join("..", "..", ".."))
		if err := Generate(root, true); err != nil {
			t.Fatalf("Generate(check) error = %v", err)
		}
	})
}

func configSourceYAMLKeys(t *testing.T) map[string]struct{} {
	t.Helper()

	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	keys := map[string]struct{}{}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join("..", entry.Name())
		file, parseErr := parser.ParseFile(files, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("ParseFile(%q) error = %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			field, ok := node.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			tag, unquoteErr := strconv.Unquote(field.Tag.Value)
			if unquoteErr != nil {
				t.Fatalf("Unquote(%q) error = %v", field.Tag.Value, unquoteErr)
			}
			key, _, _ := strings.Cut(reflect.StructTag(tag).Get("yaml"), ",")
			if key != "" && key != "-" {
				keys[key] = struct{}{}
			}
			return true
		})
	}
	return keys
}
