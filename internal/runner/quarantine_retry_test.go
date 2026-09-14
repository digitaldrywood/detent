package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerQuarantinedRebaseReachesFirstTurn(t *testing.T) {
	t.Parallel()
	source := initRunnerSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := connector.Issue{ID: "2636", Identifier: "quarantine-retry", BranchName: "detent/quarantine-retry"}
	first, err := backend.Create(t.Context(), workspaceIssue(defaultProjectID, issue))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{first.Path, source} {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(dir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runRunnerGit(t, dir, "commit", "-am", "conflicting change")
	}
	if err := runAgentGit(t.Context(), first.Path, "rebase", "main"); err == nil {
		t.Fatal("want conflicted rebase")
	}
	agent := &committingAgentBackend{}
	runner, err := NewRunner(Dependencies{Workflow: config.Workflow{Prompt: "Work on {{ issue.identifier }}"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(t.Context(), RunRequest{Issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	if agent.request.Workspace != first.Path || result.FinalState != FinalStateCompleted {
		t.Fatalf("retry workspace = %q, state = %q", agent.request.Workspace, result.FinalState)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".detent", "quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantines = %v, err = %v", entries, err)
	}
}
