package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestGitHubLocalImportCommandImportsExplicitIssue(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent":
			writeCLIJSON(t, w, map[string]any{"id": 123, "full_name": "digitaldrywood/detent"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/issues/779":
			body := "Issue body"
			writeCLIJSON(t, w, map[string]any{
				"node_id":    "I_kwDOtest779",
				"number":     779,
				"title":      "Add local status mode",
				"body":       body,
				"state":      "open",
				"html_url":   "https://github.com/digitaldrywood/detent/issues/779",
				"created_at": "2026-07-01T12:00:00Z",
				"updated_at": "2026-07-02T12:00:00Z",
				"user":       map[string]any{"login": "octocat"},
				"labels":     []map[string]string{{"name": "enhancement"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/pulls":
			writeCLIJSON(t, w, []map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	workdir := filepath.Join(root, "repo")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	workflowPath := filepath.Join(workdir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(githubLocalWorkflow(server.URL)), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	configPath := filepath.Join(root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{{
		ID:       "detent",
		Workflow: workflowPath,
		Workdir:  workdir,
		Weight:   1,
		Priority: 1,
	}})

	var out bytes.Buffer
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "--format", "json", "github-local", "import", "detent", "779", "--state", "Todo"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var result struct {
		Status   string `json:"status"`
		Project  string `json:"project"`
		Imported []struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			State      string `json:"state"`
		} `json:"imported"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v; output=%s", err, out.String())
	}
	if result.Status != "ok" || result.Project != "detent" || len(result.Imported) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.Imported[0].ID != "github:123:779" ||
		result.Imported[0].Identifier != "digitaldrywood/detent#779" ||
		result.Imported[0].State != "Todo" {
		t.Fatalf("imported issue = %#v", result.Imported[0])
	}
}

func githubLocalWorkflow(serverURL string) string {
	return `---
tracker:
  kind: github_local
  endpoint: ` + serverURL + `/graphql
  api_key: ghp_test
  repository: digitaldrywood/detent
  local_sqlite:
    path: .detent/work-items.db
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
workspace:
  root: .detent/workspaces
  source_root: .
---
Prompt
`
}

func writeCLIJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
