package runner

import "testing"

// TestPrepareTurnAppliesPreferences proves the conversation's turn
// preferences reach the turn request: an explicit model and effort replace
// the run's own selection, read-only access forbids writes, and a
// coordinator turn stays read-only whatever access says (decisions
// section 14).
func TestPrepareTurnAppliesPreferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		preferences ConversationPreferences
		coordinator bool
		request     AgentTurnRequest
		wantModel   string
		wantEffort  string
		wantRead    bool
	}{
		{
			name:        "auto leaves the run's selection alone",
			preferences: ConversationPreferences{Model: "auto", ReasoningEffort: "auto", Access: "auto"},
			request:     AgentTurnRequest{Model: "gpt-6-astra", ReasoningEffort: "low"},
			wantModel:   "gpt-6-astra",
			wantEffort:  "low",
		},
		{
			name:       "an empty preference set leaves the run's selection alone",
			request:    AgentTurnRequest{Model: "gpt-6-astra", ReasoningEffort: "low"},
			wantModel:  "gpt-6-astra",
			wantEffort: "low",
		},
		{
			name:        "explicit model and effort override the run",
			preferences: ConversationPreferences{Model: "claude-opus-5", ReasoningEffort: "high", Access: "full"},
			request:     AgentTurnRequest{Model: "gpt-6-astra", ReasoningEffort: "low"},
			wantModel:   "claude-opus-5",
			wantEffort:  "high",
		},
		{
			name:        "read only access forbids writes",
			preferences: ConversationPreferences{Access: "read_only"},
			request:     AgentTurnRequest{Model: "gpt-6-astra"},
			wantModel:   "gpt-6-astra",
			wantRead:    true,
		},
		{
			name:        "full access never re-enables writes the run forbade",
			preferences: ConversationPreferences{Access: "full"},
			request:     AgentTurnRequest{Model: "gpt-6-astra", ReadOnly: true},
			wantModel:   "gpt-6-astra",
			wantRead:    true,
		},
		{
			name:        "a coordinator turn is read only whatever access says",
			preferences: ConversationPreferences{Access: "full"},
			coordinator: true,
			request:     AgentTurnRequest{Model: "gpt-6-astra"},
			wantModel:   "gpt-6-astra",
			wantRead:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := newFakeConversationSession()
			session.preferences = test.preferences
			session.coordinator = test.coordinator
			run := &conversationRun{session: session}
			got := run.prepareTurn(test.request)
			if got.Model != test.wantModel {
				t.Fatalf("Model = %q, want %q", got.Model, test.wantModel)
			}
			if got.ReasoningEffort != test.wantEffort {
				t.Fatalf("ReasoningEffort = %q, want %q", got.ReasoningEffort, test.wantEffort)
			}
			if got.ReadOnly != test.wantRead {
				t.Fatalf("ReadOnly = %t, want %t", got.ReadOnly, test.wantRead)
			}
		})
	}
}
