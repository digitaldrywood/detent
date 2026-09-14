package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRunnerTriageIsReadOnly(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		updates []AgentUpdate
		wantErr bool
	}{
		{"note", []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: "triage note"}}, false},
		{"tool refused", []AgentUpdate{{Type: AgentUpdateToolStarted}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeCodexClient{updates: tt.updates}
			workspace := &fakeWorkspaceBackend{}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{}}, Workspace: workspace,
				AgentBackend: backend, Store: &fakeSessionStore{sessionID: 2595}, SecurityAuditRoot: t.TempDir(),
				Now: time.Now,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(t.Context(), RunRequest{
				Issue: connector.Issue{ID: "stalled", Identifier: "owner/repo#1", State: "Rework"}, Mode: RunModeTriage,
				TriageContext: "CI failed; three sessions; review thread https://example.com/review",
				AgentTools:    []AgentTool{{Name: "file_machine_issue"}},
				AgentToolHandler: func(_ context.Context, _ AgentToolCall) (AgentToolResult, error) {
					t.Error("triage exposed worker mutation tool")
					return AgentToolResult{}, nil
				},
			})
			if tt.wantErr {
				if !errors.Is(err, ErrSecurityAuditToolUse) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || result.Output != "triage note" {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			request := backend.request
			if !request.ReadOnly || request.SupplementalTools || len(request.ExtraWritableRoots) != 0 || request.Resume != (AgentResume{}) {
				t.Fatalf("triage restrictions = %#v", request)
			}
			if !strings.Contains(request.Prompt, "https://example.com/review") || request.MaxTurns != 1 {
				t.Fatalf("triage input/turn bound = %#v", request)
			}
			if backend.calls != 1 {
				t.Fatalf("backend calls = %d", backend.calls)
			}
		})
	}
}
