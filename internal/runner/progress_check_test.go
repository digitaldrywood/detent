package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func TestProgressCheckUsesReadOnlyBackendAndJSONBudget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	output := `{"outcome":"reroute","step":"implement","reason":"` + strings.Repeat("evidence ", 100) + `","evidence":["failed-attempt"]}`
	backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "final", Delta: output}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := r.CheckProgress(t.Context(), RunRequest{Issue: connector.Issue{ID: "example-issue"}, Mode: RunModeImplement}, progresscheck.Request{ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{RunModeImplement}})
	if err != nil || decision.Outcome != progresscheck.Reroute {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	request := backend.request
	if !request.ReadOnly || !request.TextOnly || request.SupplementalTools || request.MaxTurns != 1 || request.Resume.ThreadID != "" || filepath.Dir(request.Workspace) != root {
		t.Fatalf("progress request escaped read-only isolated turn: %+v", request)
	}
	if !strings.Contains(request.Prompt, "failed-attempt") || !strings.Contains(request.Prompt, progresscheck.Instructions) {
		t.Fatal("backend did not receive evidence and decision contract")
	}
}
