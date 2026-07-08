package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func TestWorkItemAddCreatesLocalSQLiteItem(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "video")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	workflowPath := filepath.Join(workdir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(localSQLiteWorkItemWorkflow()), 0o600); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}
	configPath := filepath.Join(root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{{
		ID:       "video",
		Workflow: workflowPath,
		Workdir:  workdir,
		Weight:   1,
		Priority: 0,
	}})

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"--format", "json",
		"work-item", "add", "video",
		"--title", "Author beat visuals",
		"--body", "Render storyboard frames",
		"--label", "video-assets",
		"--field", "render_status=queued",
		"--priority", "2",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var result struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		URL        string `json:"url"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v; output = %s", err, stdout.String())
	}
	if result.ID == "" || result.Identifier == "" || result.URL == "" {
		t.Fatalf("result missing id, identifier, or url: %#v", result)
	}

	conn, err := local.New(local.Config{
		Path:           filepath.Join(workdir, ".detent", "work-items.db"),
		ProjectID:      "video",
		ActiveStates:   []string{"Todo", "In Progress"},
		ObservedStates: []string{"Backlog", "Blocked"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	issues, err := conn.FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	issue := issues[0]
	if issue.Title != "Author beat visuals" || issue.Description != "Render storyboard frames" {
		t.Fatalf("issue text = %#v", issue)
	}
	if issue.Fields["render_status"] != "queued" {
		t.Fatalf("Fields = %#v", issue.Fields)
	}
	if issue.Priority == nil || *issue.Priority != 2 {
		t.Fatalf("Priority = %v, want 2", issue.Priority)
	}
}

func TestWorkItemAddRejectsMemoryTrackerAsUnsupported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "video")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	workflowPath := filepath.Join(workdir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(memoryWorkItemWorkflow()), 0o600); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}
	configPath := filepath.Join(root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{{
		ID:       "video",
		Workflow: workflowPath,
		Workdir:  workdir,
		Weight:   1,
		Priority: 0,
	}})

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"work-item", "add", "video",
		"--title", "Author beat visuals",
		"--body", "Render storyboard frames",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want unsupported tracker")
	}
	var itemErr *workitem.Error
	if !errors.As(err, &itemErr) {
		t.Fatalf("Execute() error = %v, want workitem.Error", err)
	}
	if itemErr.Code != workitem.CodeUnsupportedTracker {
		t.Fatalf("error code = %q, want %q", itemErr.Code, workitem.CodeUnsupportedTracker)
	}
}

func localSQLiteWorkItemWorkflow() string {
	return `---
tracker:
  kind: local_sqlite
  local_sqlite:
    path: .detent/work-items.db
    project_id: video
  active_states:
    - Todo
    - In Progress
  observed_states:
    - Backlog
    - Blocked
  terminal_states:
    - Done
workspace:
  root: .detent/workspaces
  source_root: .
---
Prompt
`
}

func memoryWorkItemWorkflow() string {
	return `---
tracker:
  kind: memory
  active_states:
    - Todo
    - In Progress
  observed_states:
    - Backlog
    - Blocked
  terminal_states:
    - Done
workspace:
  root: .detent/workspaces
  source_root: .
---
Prompt
`
}
