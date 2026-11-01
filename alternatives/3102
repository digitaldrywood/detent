package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestValidationToolOutputReachesUsageTelemetry(t *testing.T) {
	t.Parallel()
	output := func(item, text string) AgentUpdate {
		return AgentUpdate{Type: AgentUpdateToolOutput, ItemID: item, Delta: text}
	}
	for _, tt := range []struct {
		name, message, phase string
		updates              []AgentUpdate
	}{
		{"queued", "validation gate waiting: position=2", "waiting_validation", []AgentUpdate{output("gate", "validation gate waiting: position=2\n")}},
		{"running", "validation gate running: owner_pid=10", "validating", []AgentUpdate{output("gate", "validation gate waiting: position=2\nvalidation gate running: owner_pid=10\n")}},
		{"split marker", "validation gate waiting: position=2", "waiting_validation", []AgentUpdate{output("gate", "validation gate wai"), output("gate", "ting: position=2\n")}},
		{"independent streams", "validation gate waiting: position=2", "waiting_validation", []AgentUpdate{output("gate", "validation gate wai"), output("other", "unrelated output\n"), output("gate", "ting: position=2\n")}},
		{"finished", "validation gate finished: result=passed", "", []AgentUpdate{output("gate", "validation gate waiting: position=2\n"), output("gate", "validation gate finished: result=passed\n")}},
		{"completion payload is not output", "implementing", "", []AgentUpdate{{Type: AgentUpdateToolCompleted, ItemID: "gate", Delta: "validation gate waiting: command payload", Status: "completed"}}},
		{"unrelated output", "implementing", "", []AgentUpdate{output("other", "ordinary output that must not replace prose\n")}},
		{"quoted source", "implementing", "", []AgentUpdate{output("other", `fmt.Println("validation gate waiting: position=2")`)}},
		{"completed stream reset", "implementing", "", []AgentUpdate{output("gate", "validation gate wai"), {Type: AgentUpdateToolCompleted, ItemID: "gate", Status: "completed"}, output("gate", "ting: position=2\n")}},
		{"turn reset", "turn completed", "", []AgentUpdate{output("gate", "validation gate wai"), {Type: AgentUpdateTurnCompleted}, output("gate", "ting: position=2\n")}},
		{"large output", "validation gate running: owner_pid=10", "validating", []AgentUpdate{output("gate", "validation gate running: owner_pid=10\n"+strings.Repeat("ordinary log\n", 1000))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			progress := newAgentRunProgress(runtimeoutput.Policy{}, "", "", 0, "", 0)
			at := time.Now()
			progress.apply(AgentUpdate{Type: AgentUpdateMessageDelta, ItemID: "prose", Delta: "implementing"}, at)
			for _, update := range tt.updates {
				progress.apply(update, at)
			}
			var usage UsageUpdate
			runner := &Runner{}
			err := runner.publishRunUpdate(t.Context(), RunRequest{Admission: &AdmissionRequest{}, OnUsageUpdate: func(update UsageUpdate) error {
				usage = update
				return nil
			}}, workspace.Info{}, workspace.Issue{}, progress, RunResult{}, at, at, 1)
			if err != nil {
				t.Fatal(err)
			}
			if usage.LastMessage != tt.message || activity.ValidationPhase(usage.LastMessage) != tt.phase {
				t.Fatalf("published message = %q, phase = %q; want %q, %q", usage.LastMessage, activity.ValidationPhase(usage.LastMessage), tt.message, tt.phase)
			}
			if activity.ValidationMessage(tt.message) != "" && usage.RecentEvents[len(usage.RecentEvents)-1].Message != tt.message {
				t.Fatalf("validation event not published: %+v", usage.RecentEvents)
			}
			if progress.finalMessage() != "implementing" {
				t.Fatalf("tool output replaced final prose: %q", progress.finalMessage())
			}
		})
	}
}

func TestPublishAgentActivityContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		update      AgentUpdate
		wantContent string
	}{
		{
			name: "forwards full tool output",
			update: AgentUpdate{
				Type:     AgentUpdateToolOutput,
				ThreadID: "thread-1",
				TurnID:   "turn-1",
				ItemID:   "item-1",
				Tool:     "exec_command",
				Delta:    "complete command output",
			},
			wantContent: "complete command output",
		},
		{
			name: "forwards failed tool backend error",
			update: AgentUpdate{
				Type:                AgentUpdateToolCompleted,
				ThreadID:            "thread-2",
				TurnID:              "turn-2",
				ItemID:              "item-2",
				Tool:                "codex_apps/github.create_pull_request",
				Delta:               "null",
				Status:              "failed",
				BackendErrorMessage: "user rejected MCP tool call",
			},
			wantContent: "user rejected MCP tool call",
		},
		{
			name: "preserves successful tool content",
			update: AgentUpdate{
				Type:                AgentUpdateToolCompleted,
				ThreadID:            "thread-3",
				TurnID:              "turn-3",
				ItemID:              "item-3",
				Tool:                "codex_apps/github.create_pull_request",
				Delta:               `{"url":"https://github.test/acme/widgets/pull/18"}`,
				Status:              "completed",
				BackendErrorMessage: "stale backend error",
			},
			wantContent: `{"url":"https://github.test/acme/widgets/pull/18"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got AgentActivityUpdate
			req := RunRequest{OnActivityUpdate: func(update AgentActivityUpdate) error {
				got = update
				return nil
			}}
			at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
			err := publishAgentActivity(req, 1156, tt.update, at)
			if err != nil {
				t.Fatalf("publishAgentActivity() error = %v", err)
			}
			if got.DetentSessionID != 1156 || got.ProviderSessionID != tt.update.ThreadID || got.Type != tt.update.Type || got.Tool != tt.update.Tool || got.Content != tt.wantContent || got.At != at {
				t.Fatalf("activity update = %#v, want content %q", got, tt.wantContent)
			}
		})
	}
}
