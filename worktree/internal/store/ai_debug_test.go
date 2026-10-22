package store

import (
	"testing"
	"time"
)

func TestListIssueAIDebugWorkAttemptsReturnsFullIssueHistory(t *testing.T) {
	t.Parallel()

	backend := openTestStore(t, t.Context())
	reader, ok := backend.(AIDebugAttemptReader)
	if !ok {
		t.Fatalf("store %T does not implement AIDebugAttemptReader", backend)
	}
	startedAt := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	for index, identifier := range []string{"digitaldrywood/detent#2006", "digitaldrywood/detent#1999", "digitaldrywood/detent#2006"} {
		attemptID, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{
			ProjectID: "detent", IssueID: "issue-" + identifier, Identifier: identifier,
			StartedAt: startedAt.Add(time.Duration(index) * time.Minute), AttemptNumber: index + 1, Lane: "In Progress", WorkerType: "agent",
		})
		if err != nil {
			t.Fatalf("StartWorkAttempt(%d) error = %v", index, err)
		}
		if index == 2 {
			if err := backend.CompleteWorkAttempt(t.Context(), WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: startedAt.Add(3 * time.Minute), Status: WorkAttemptStatusTerminal, TerminalState: WorkAttemptTerminalSuccess}); err != nil {
				t.Fatalf("CompleteWorkAttempt() error = %v", err)
			}
		}
	}

	attempts, err := reader.ListIssueAIDebugWorkAttempts(t.Context(), IssueIdentity{ProjectID: "detent", Identifier: "digitaldrywood/detent#2006"})
	if err != nil {
		t.Fatalf("ListIssueAIDebugWorkAttempts() error = %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("ListIssueAIDebugWorkAttempts() len = %d, want 2: %#v", len(attempts), attempts)
	}
	if !attempts[0].CompletedAt.IsZero() || attempts[1].TerminalState != WorkAttemptTerminalSuccess {
		t.Fatalf("attempts = %#v, want active and terminal history", attempts)
	}
}

func TestListIssueAIDebugSessionsReturnsFullIssueHistory(t *testing.T) {
	t.Parallel()

	backend := openTestStore(t, t.Context())
	reader, ok := backend.(AIDebugSessionReader)
	if !ok {
		t.Fatalf("store %T does not implement AIDebugSessionReader", backend)
	}
	startedAt := time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)
	for index, identifier := range []string{"digitaldrywood/detent#2006", "digitaldrywood/detent#1999", "digitaldrywood/detent#2006"} {
		sessionID, err := backend.StartSession(t.Context(), SessionStart{
			ProjectID: "detent", IssueID: "issue-" + identifier, Identifier: identifier,
			StartedAt: startedAt.Add(time.Duration(index) * time.Minute), Model: "gpt-5.6", RequestedModel: "gpt-5.6",
		})
		if err != nil {
			t.Fatalf("StartSession(%d) error = %v", index, err)
		}
		if err := backend.FinishSession(t.Context(), sessionID, SessionFinish{
			CompletedAt: startedAt.Add(time.Duration(index)*time.Minute + 30*time.Second), Turns: int64(index + 1),
			InputTokens: int64(100 + index), CachedInputTokens: int64(80 + index), OutputTokens: int64(20 + index), TotalTokens: int64(120 + index),
			RuntimeSeconds: 30, FinalState: "success", Model: "gpt-5.6", ResumedFromSessionID: int64(index),
		}); err != nil {
			t.Fatalf("FinishSession(%d) error = %v", index, err)
		}
	}

	sessions, err := reader.ListIssueAIDebugSessions(t.Context(), IssueIdentity{ProjectID: "detent", Identifier: "digitaldrywood/detent#2006"})
	if err != nil {
		t.Fatalf("ListIssueAIDebugSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListIssueAIDebugSessions() len = %d, want 2: %#v", len(sessions), sessions)
	}
	if sessions[0].StartedAt == nil || !sessions[0].StartedAt.Equal(startedAt) {
		t.Fatalf("first StartedAt = %v, want %v", sessions[0].StartedAt, startedAt)
	}
	if sessions[1].TotalTokens != 122 || sessions[1].CachedInputTokens != 82 || sessions[1].ResumedFromSessionID != 2 {
		t.Fatalf("second session = %#v, want complete token and resume evidence", sessions[1])
	}
}
