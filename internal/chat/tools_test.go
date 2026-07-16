package chat

import "testing"

func TestActionSummaryDescribesOperatorActions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		action Action
		want   string
	}{
		{name: "move", action: Action{Kind: ActionMoveItem, Identifier: "repo#1", CurrentState: "Backlog", TargetState: "Todo"}, want: "Move repo#1 from Backlog to Todo"},
		{name: "priority", action: Action{Kind: ActionSetPriority, IssueID: "issue-1", Priority: "High"}, want: "Set issue-1 priority to High"},
		{name: "stop", action: Action{Kind: ActionStopRun, Destination: "Todo", Priority: "Urgent"}, want: "Stop item and move it to Todo at Urgent priority"},
		{name: "file", action: Action{Kind: ActionFileIssue, ProjectID: "detent", Title: "Follow-up"}, want: `File "Follow-up" on detent`},
		{name: "unknown", action: Action{}, want: "Unknown operator action"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ActionSummary(test.action); got != test.want {
				t.Fatalf("ActionSummary() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRandomIDReturnsHexIdentifier(t *testing.T) {
	t.Parallel()
	id, err := randomID()
	if err != nil {
		t.Fatalf("randomID() error = %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("randomID() length = %d, want 32", len(id))
	}
}
