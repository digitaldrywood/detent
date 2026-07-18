package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadProjectDefinitionLayouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		workflow    string
		config      string
		local       string
		localConfig string
		wantLayout  ProjectDefinitionLayout
		wantPrompt  string
		wantKeys    []string
		wantErr     string
	}{
		{
			name:       "legacy",
			workflow:   "---\ntracker:\n  kind: memory\n---\nShared direction.\n",
			wantLayout: ProjectDefinitionLegacy,
			wantPrompt: "Shared direction.\n",
			wantKeys:   []string{"tracker"},
		},
		{
			name:       "split",
			workflow:   "Shared direction.\n",
			config:     "schema: 1\ntracker:\n  kind: memory\n",
			wantLayout: ProjectDefinitionSplit,
			wantPrompt: "Shared direction.\n",
		},
		{
			name:        "split with local files",
			workflow:    "Shared direction.\n",
			config:      "schema: 1\ntracker:\n  kind: memory\nagent:\n  max_turns: 10\n",
			local:       "Local direction.\n",
			localConfig: "schema: 1\nagent:\n  max_turns: 12\n",
			wantLayout:  ProjectDefinitionSplit,
			wantPrompt:  "Shared direction.\n\n---\n\n## Machine-local workflow overlay\n\nLocal direction.\n",
		},
		{
			name:       "mixed shared authority",
			workflow:   "---\ntracker:\n  kind: memory\n---\nShared direction.\n",
			config:     "schema: 1\ntracker:\n  kind: memory\n",
			wantLayout: ProjectDefinitionMixed,
			wantErr:    "ambiguous authority",
		},
		{
			name:       "mixed local authority",
			workflow:   "Shared direction.\n",
			config:     "schema: 1\ntracker:\n  kind: memory\n",
			local:      "---\nagent:\n  max_turns: 12\n---\nLocal direction.\n",
			wantLayout: ProjectDefinitionMixed,
			wantErr:    "WORKFLOW.local.md structured frontmatter",
		},
		{
			name:       "incomplete",
			workflow:   "Shared direction.\n",
			wantLayout: ProjectDefinitionIncomplete,
			wantErr:    "detent.yaml is missing",
		},
		{
			name:       "unsupported schema",
			workflow:   "Shared direction.\n",
			config:     "schema: 2\ntracker:\n  kind: memory\n",
			wantLayout: ProjectDefinitionSplit,
			wantErr:    "unsupported schema version 2",
		},
		{
			name:       "missing schema",
			workflow:   "Shared direction.\n",
			config:     "tracker:\n  kind: memory\n",
			wantLayout: ProjectDefinitionSplit,
			wantErr:    "schema is required",
		},
		{
			name:       "invalid split YAML",
			workflow:   "Shared direction.\n",
			config:     "schema: 1\ntracker: [\n",
			wantLayout: ProjectDefinitionSplit,
			wantErr:    "parse",
		},
		{
			name:       "invalid legacy YAML",
			workflow:   "---\ntracker: [\n---\nShared direction.\n",
			wantLayout: ProjectDefinitionLegacy,
			wantErr:    "parse legacy workflow config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			workflowPath := filepath.Join(dir, "WORKFLOW.md")
			writeProjectDefinitionTestFile(t, workflowPath, tt.workflow, 0o644)
			if tt.config != "" {
				writeProjectDefinitionTestFile(t, filepath.Join(dir, "detent.yaml"), tt.config, 0o644)
			}
			if tt.local != "" {
				writeProjectDefinitionTestFile(t, filepath.Join(dir, "WORKFLOW.local.md"), tt.local, 0o600)
			}
			if tt.localConfig != "" {
				writeProjectDefinitionTestFile(t, filepath.Join(dir, "detent.local.yaml"), tt.localConfig, 0o600)
			}

			workflow, err := LoadProjectDefinition(workflowPath)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadProjectDefinition() error = %v, want containing %q", err, tt.wantErr)
				}
				var definitionErr *ProjectDefinitionError
				if !errors.As(err, &definitionErr) {
					t.Fatalf("error type = %T, want *ProjectDefinitionError", err)
				}
				if definitionErr.Definition.Layout != tt.wantLayout {
					t.Fatalf("error layout = %q, want %q", definitionErr.Definition.Layout, tt.wantLayout)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProjectDefinition() error = %v", err)
			}
			if workflow.Definition.Layout != tt.wantLayout {
				t.Fatalf("layout = %q, want %q", workflow.Definition.Layout, tt.wantLayout)
			}
			if workflow.Prompt != tt.wantPrompt {
				t.Fatalf("Prompt = %q, want %q", workflow.Prompt, tt.wantPrompt)
			}
			if !reflect.DeepEqual(workflow.Definition.LegacyKeys, tt.wantKeys) {
				t.Fatalf("LegacyKeys = %#v, want %#v", workflow.Definition.LegacyKeys, tt.wantKeys)
			}
			if workflow.Definition.Revision == "" {
				t.Fatal("Revision is blank")
			}
		})
	}
}

func TestLoadProjectDefinitionMissingWorkflowIsIncomplete(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	_, err := LoadProjectDefinition(workflowPath)
	if err == nil {
		t.Fatal("LoadProjectDefinition() error = nil")
	}
	var definitionErr *ProjectDefinitionError
	if !errors.As(err, &definitionErr) {
		t.Fatalf("error type = %T, want *ProjectDefinitionError", err)
	}
	if definitionErr.Definition.Layout != ProjectDefinitionIncomplete {
		t.Fatalf("layout = %q, want incomplete", definitionErr.Definition.Layout)
	}
}

func TestLoadProjectDefinitionUsesConfiguredExternalRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	definitionRoot := filepath.Join(root, "orchestration", "detent")
	workflowPath := filepath.Join(definitionRoot, "WORKFLOW.md")
	writeProjectDefinitionTestFile(t, workflowPath, "External direction.\n", 0o644)
	writeProjectDefinitionTestFile(t, filepath.Join(definitionRoot, "detent.yaml"), "schema: 1\ntracker:\n  kind: memory\n", 0o644)

	workflow, err := LoadProjectDefinition(workflowPath)
	if err != nil {
		t.Fatalf("LoadProjectDefinition() error = %v", err)
	}
	if workflow.Definition.ConfigPath != filepath.Join(definitionRoot, "detent.yaml") {
		t.Fatalf("ConfigPath = %q, want configured definition root", workflow.Definition.ConfigPath)
	}
}

func writeProjectDefinitionTestFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
