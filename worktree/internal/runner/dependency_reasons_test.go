package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDependencyReasonTurnUsesIsolatedReadOnlyScratch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	backend := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "commentary", Delta: "I will summarize the relationship."}, {Type: AgentUpdateMessageDelta, ItemID: "final", Delta: "Enables project preferences."}}}
	r := &Runner{}
	generate := r.dependencyReasonGenerator(backend, AgentTurnRequest{Workspace: "source-workspace", Resume: AgentResume{ThreadID: "existing"}, SupplementalTools: true}, nil, "codex")
	got, err := generate(t.Context(), "Summarize authorized issue data.")
	if err != nil || got != "Enables project preferences." {
		t.Fatalf("reason=%q err=%v", got, err)
	}
	request := backend.request
	if !request.ReadOnly || !request.TextOnly || request.SupplementalTools || request.Resume.ThreadID != "" || request.MaxTurns != 1 || request.Prompt != "Summarize authorized issue data." || filepath.Dir(request.Workspace) != root || !strings.HasPrefix(request.TempDir, root+string(filepath.Separator)) {
		t.Fatalf("reason request escaped isolated read-only context: %+v", request)
	}
	if _, err := os.Stat(request.Workspace); !os.IsNotExist(err) {
		t.Fatalf("reason scratch retained: %v", err)
	}
}
