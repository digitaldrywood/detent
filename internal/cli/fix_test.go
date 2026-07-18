package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestWorkflowLayoutFixModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		input      string
		wantConfig bool
		wantOutput string
	}{
		{
			name:       "dry run",
			args:       []string{"--dry-run"},
			wantOutput: "Dry run; no files changed.",
		},
		{
			name:       "interactive decline",
			input:      "no\n",
			wantOutput: "Migration cancelled; no files changed.",
		},
		{
			name:       "interactive confirmation",
			input:      "yes\n",
			wantConfig: true,
			wantOutput: "Migration applied.",
		},
		{
			name:       "non-interactive confirmation",
			args:       []string{"--yes"},
			wantConfig: true,
			wantOutput: "Migration applied.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			workflowPath := filepath.Join(dir, "WORKFLOW.md")
			if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n---\nPrompt\n"), 0o640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd := newWorkflowLayoutFixCommand()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetIn(strings.NewReader(tt.input))
			cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
				stdoutTTY: func() bool { return true },
			}))
			cmd.SetArgs(append([]string{"--workflow", workflowPath}, tt.args...))

			if err := cmd.ExecuteContext(cmd.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("stdout = %q, want containing %q", stdout.String(), tt.wantOutput)
			}
			_, err := os.Stat(workflowconfig.DefinitionPath(workflowPath))
			if tt.wantConfig && err != nil {
				t.Fatalf("Stat(detent.yaml) error = %v", err)
			}
			if !tt.wantConfig && !os.IsNotExist(err) {
				t.Fatalf("Stat(detent.yaml) error = %v, want not exist", err)
			}
		})
	}
}

func TestWorkflowLayoutFixJSONDryRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n---\nPrompt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var stdout bytes.Buffer
	cmd := newWorkflowLayoutFixCommand()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
		stdoutTTY: func() bool { return false },
	}))
	cmd.SetArgs([]string{"--workflow", workflowPath, "--dry-run"})

	if err := cmd.ExecuteContext(cmd.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	var result workflowLayoutFixResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	if !result.DryRun || result.Applied || result.BeforeLayout != workflowconfig.ProjectDefinitionLegacy {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Operations) != 2 {
		t.Fatalf("operations = %#v, want create and rewrite", result.Operations)
	}
}
