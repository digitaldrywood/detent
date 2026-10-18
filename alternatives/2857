package store

import (
	"context"
	"testing"
	"time"
)

func TestListIssueActivityCombinesHistoryAndVerboseUsage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	identity := IssueIdentity{IssueID: "issue-1156", Identifier: "digitaldrywood/detent#1156", IssueURL: "https://github.com/digitaldrywood/detent/issues/1156"}

	if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
		ProjectID:  "detent",
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
		Result:     SchedulerDecisionResultSkipped,
		Reason:     "artifact_gate_wait_status",
		DecisionAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordSchedulerDecision() error = %v", err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
		ProjectID:         "detent",
		IssueID:           identity.IssueID,
		Identifier:        identity.Identifier,
		IssueURL:          identity.IssueURL,
		PhaseType:         WorkflowPhaseTypeLane,
		PhaseName:         "In Progress",
		PreviousPhaseName: "Todo",
		Reason:            "dispatch",
		Status:            "completed",
		StartedAt:         base,
		FinishedAt:        base.Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{
		ProjectID:   "detent",
		IssueID:     identity.IssueID,
		Identifier:  identity.Identifier,
		Model:       "gpt-5.6-codex",
		TotalTokens: 420,
		StartedAt:   base,
		FinishedAt:  base.Add(2 * time.Minute),
		Outcome:     "completed",
	}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}

	query := IssueActivityQuery{
		ProjectID:  "detent",
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
		Limit:      10,
	}
	events, err := backend.(ActivityStore).ListIssueActivity(ctx, query)
	if err != nil {
		t.Fatalf("ListIssueActivity() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ListIssueActivity() len = %d, want 2 meaningful events: %#v", len(events), events)
	}
	if events[0].Source != "scheduler" || events[0].Reason != "artifact_gate_wait_status" {
		t.Fatalf("newest event = %#v, want scheduler skip reason", events[0])
	}

	query.IncludeVerbose = true
	events, err = backend.(ActivityStore).ListIssueActivity(ctx, query)
	if err != nil {
		t.Fatalf("ListIssueActivity(verbose) error = %v", err)
	}
	if len(events) != 3 || events[0].Source != "usage" || events[0].TotalTokens != 420 || !events[0].Verbose {
		t.Fatalf("verbose events = %#v, want newest token usage", events)
	}
}

func TestListIssueActivityIncludesCurrentAndPreviousAttempts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	start := WorkAttemptStart{
		ProjectID:  "detent",
		IssueID:    "issue-1156",
		Identifier: "digitaldrywood/detent#1156",
		WorkerType: "agent",
		StartedAt:  base,
	}
	previousID, err := backend.StartWorkAttempt(ctx, start)
	if err != nil {
		t.Fatalf("StartWorkAttempt(previous) error = %v", err)
	}
	if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
		AttemptID:     previousID,
		CompletedAt:   base.Add(time.Minute),
		Status:        WorkAttemptStatusTerminal,
		TerminalState: WorkAttemptTerminalFailure,
		ErrorMessage:  "quota exhausted",
	}); err != nil {
		t.Fatalf("CompleteWorkAttempt() error = %v", err)
	}
	start.AttemptNumber = 2
	start.StartedAt = base.Add(2 * time.Minute)
	if _, err := backend.StartWorkAttempt(ctx, start); err != nil {
		t.Fatalf("StartWorkAttempt(current) error = %v", err)
	}

	events, err := backend.(ActivityStore).ListIssueActivity(ctx, IssueActivityQuery{ProjectID: "detent", IssueID: "issue-1156", Limit: 20})
	if err != nil {
		t.Fatalf("ListIssueActivity() error = %v", err)
	}
	attempts := map[int]bool{}
	for _, event := range events {
		if event.Source == "work_attempt" {
			attempts[event.AttemptNumber] = true
		}
	}
	if !attempts[1] || !attempts[2] {
		t.Fatalf("attempt events = %#v, want current and previous attempts in %#v", attempts, events)
	}
}

func TestLatestIssueAgentSessionIncludesTerminalFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		finalState string
	}{
		{name: "completed", finalState: "completed"},
		{name: "failed", finalState: "failed"},
		{name: "cancelled", finalState: "cancelled"},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issueID := "issue-history-" + tt.name
			sessionID, err := backend.StartSession(ctx, SessionStart{
				ProjectID:        "detent",
				IssueID:          issueID,
				Identifier:       "digitaldrywood/detent#" + tt.name,
				StartedAt:        base.Add(time.Duration(index) * time.Minute),
				AgentBackendKind: "codex",
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			if err := backend.FinishSession(ctx, sessionID, SessionFinish{
				CompletedAt:      base.Add(time.Duration(index+1) * time.Minute),
				FinalState:       tt.finalState,
				ProviderThreadID: "thread-" + tt.name,
			}); err != nil {
				t.Fatalf("FinishSession() error = %v", err)
			}

			got, err := backend.(ActivityStore).LatestIssueAgentSession(ctx, IssueIdentity{ProjectID: "detent", IssueID: issueID})
			if err != nil {
				t.Fatalf("LatestIssueAgentSession() error = %v", err)
			}
			if got.DetentSessionID != sessionID || got.ProviderThreadID != "thread-"+tt.name || got.AgentBackendKind != "codex" {
				t.Fatalf("LatestIssueAgentSession() = %#v, want %s session %d", got, tt.finalState, sessionID)
			}
		})
	}
}
