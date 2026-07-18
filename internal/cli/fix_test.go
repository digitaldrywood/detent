package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestWorkflowLayoutFixUsesRuntimeGitHubCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		env           map[string]string
		globalToken   string
		wantGHCalls   int
		resolvedToken string
		mixed         bool
	}{
		{
			name:          "GITHUB_TOKEN",
			env:           map[string]string{"GITHUB_TOKEN": "environment-token"},
			resolvedToken: "environment-token",
		},
		{
			name:          "github_token gh",
			globalToken:   "gh",
			wantGHCalls:   2,
			resolvedToken: "gh-token",
		},
		{
			name:          "github_token gh repairs mixed layout",
			globalToken:   "gh",
			wantGHCalls:   2,
			resolvedToken: "gh-token",
			mixed:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			workflowPath := filepath.Join(dir, "WORKFLOW.md")
			workflowContent := "---\ntracker:\n  kind: github\n  project_slug: PVT_example\n---\nPrompt\n"
			if tt.mixed {
				workflowContent = "Prompt\n"
				if err := os.WriteFile(workflowconfig.DefinitionPath(workflowPath), []byte("schema: 1\ntracker:\n  kind: github\n  project_slug: PVT_example\n"), 0o640); err != nil {
					t.Fatalf("WriteFile(detent.yaml) error = %v", err)
				}
				if err := os.WriteFile(workflowconfig.LocalWorkflowPath(workflowPath), []byte("---\nagent:\n  max_turns: 12\n---\nLocal prompt\n"), 0o600); err != nil {
					t.Fatalf("WriteFile(WORKFLOW.local.md) error = %v", err)
				}
			}
			if err := os.WriteFile(workflowPath, []byte(workflowContent), 0o640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			configPath := filepath.Join(dir, "global.yaml")
			writeWorkflowLayoutGlobalConfig(t, configPath, []globalconfig.Project{{
				ID:       "example",
				Workflow: workflowPath,
				Workdir:  dir,
				Weight:   1,
			}})
			global, err := globalconfig.Read(configPath)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			global.GitHubToken = tt.globalToken
			if err := globalconfig.Write(configPath, global); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			ghCalls := 0
			run := func(args ...string) error {
				t.Helper()
				cmd := NewRootCommand(t.Context(), func(opts *options) {
					opts.lookupEnv = mapLookup(tt.env)
					opts.ghAuthToken = func(context.Context) (string, error) {
						ghCalls++
						return "gh-token", nil
					}
					opts.stdoutTTY = func() bool { return true }
				})
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(append([]string{"--config", configPath, "fix", "workflow-layout", "--workflow", workflowPath}, args...))
				return cmd.ExecuteContext(cmd.Context())
			}

			configBefore, readBeforeErr := os.ReadFile(workflowconfig.DefinitionPath(workflowPath))
			if err := run("--dry-run"); err != nil {
				t.Fatalf("dry run error = %v", err)
			}
			configAfter, readAfterErr := os.ReadFile(workflowconfig.DefinitionPath(workflowPath))
			switch {
			case errors.Is(readBeforeErr, os.ErrNotExist) && !errors.Is(readAfterErr, os.ErrNotExist):
				t.Fatalf("ReadFile(detent.yaml) error = %v, want not exist after dry run", readAfterErr)
			case readBeforeErr == nil && (readAfterErr != nil || string(configAfter) != string(configBefore)):
				t.Fatalf("detent.yaml changed during dry run: error = %v, content = %q", readAfterErr, configAfter)
			}
			if err := run("--yes"); err != nil {
				t.Fatalf("apply error = %v", err)
			}
			if ghCalls != tt.wantGHCalls {
				t.Fatalf("gh auth token calls = %d, want %d", ghCalls, tt.wantGHCalls)
			}

			configRaw, err := os.ReadFile(workflowconfig.DefinitionPath(workflowPath))
			if err != nil {
				t.Fatalf("ReadFile(detent.yaml) error = %v", err)
			}
			for _, unwanted := range []string{"api_key:", tt.resolvedToken} {
				if strings.Contains(string(configRaw), unwanted) {
					t.Fatalf("detent.yaml contains runtime credential %q: %s", unwanted, configRaw)
				}
			}
			workflowRaw, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatalf("ReadFile(WORKFLOW.md) error = %v", err)
			}
			if string(workflowRaw) != "Prompt\n" {
				t.Fatalf("WORKFLOW.md = %q, want prompt only", workflowRaw)
			}
			if tt.mixed {
				localRaw, err := os.ReadFile(workflowconfig.LocalWorkflowPath(workflowPath))
				if err != nil {
					t.Fatalf("ReadFile(WORKFLOW.local.md) error = %v", err)
				}
				if string(localRaw) != "Local prompt\n" {
					t.Fatalf("WORKFLOW.local.md = %q, want prompt only", localRaw)
				}
			}
		})
	}
}

func TestWorkflowLayoutFixReportsMissingRuntimeGitHubCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: github\n  project_slug: PVT_example\n---\nPrompt\n"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	configPath := filepath.Join(dir, "global.yaml")
	writeWorkflowLayoutGlobalConfig(t, configPath, []globalconfig.Project{{
		ID:       "example",
		Workflow: workflowPath,
		Workdir:  dir,
		Weight:   1,
	}})

	cmd := NewRootCommand(t.Context(), func(opts *options) {
		opts.lookupEnv = mapLookup(nil)
		opts.stdoutTTY = func() bool { return true }
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "fix", "workflow-layout", "--workflow", workflowPath, "--dry-run"})
	err := cmd.ExecuteContext(cmd.Context())
	if !errors.Is(err, ErrGitHubAuth) {
		t.Fatalf("ExecuteContext() error = %v, want %v", err, ErrGitHubAuth)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("ExecuteContext() error = %v, want GITHUB_TOKEN guidance", err)
	}
	hint, _, ok := HintFor(err)
	if !ok || !strings.Contains(hint, "gh auth login") {
		t.Fatalf("HintFor() = %q, %t, want gh auth login guidance", hint, ok)
	}
}

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
			cmd := newWorkflowLayoutFixCommand(nil, defaultOptions())
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
	cmd := newWorkflowLayoutFixCommand(nil, defaultOptions())
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

func writeWorkflowLayoutGlobalConfig(t *testing.T, path string, projects []globalconfig.Project) {
	t.Helper()

	cfg := globalconfig.Config{
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 1,
			Scheduling:          globalconfig.SchedulingWeighted,
		},
		Projects: projects,
	}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}
