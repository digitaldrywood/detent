package store

import (
	"context"
	"testing"
	"time"

	retropkg "github.com/digitaldrywood/detent/internal/retro"
)

func TestRetroSnapshotAndRunLedger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID: "detent", Identifier: "digitaldrywood/detent#1206", WorkerType: "agent", StartedAt: base,
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
		AttemptID: attemptID, CompletedAt: base.Add(time.Minute), TerminalState: WorkAttemptTerminalFailure,
		ErrorClass: "runner", ErrorMessage: "session token ceiling exceeded",
	}); err != nil {
		t.Fatalf("CompleteWorkAttempt() error = %v", err)
	}
	sessionID, err := backend.StartSession(ctx, SessionStart{
		WorkAttemptID: attemptID, Identifier: "digitaldrywood/detent#1206", StartedAt: base,
		OrphanRecoveryOutcome: OrphanRecoveryFresh,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := backend.FinishSession(ctx, sessionID, SessionFinish{CompletedAt: base.Add(time.Minute), TotalTokens: 1234, FinalState: "failed"}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{
		ProjectID: "detent", SessionID: sessionID, Identifier: "digitaldrywood/detent#1206",
		TotalTokens: 1234, StartedAt: base, FinishedAt: base.Add(time.Minute), Outcome: "failed",
	}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
		ProjectID: "detent", Identifier: "digitaldrywood/detent#1206", PhaseType: WorkflowPhaseTypeLane,
		PhaseName: "In Progress", Reason: "workpad_status_invalid", StartedAt: base,
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}

	snapshot, err := backend.LoadRetroSnapshot(ctx, "detent", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("LoadRetroSnapshot() error = %v", err)
	}
	if len(snapshot.Attempts) != 1 || len(snapshot.Sessions) != 1 || len(snapshot.UsageEvents) != 1 || len(snapshot.PhaseEvents) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Sessions[0].TotalTokens != 1234 || snapshot.Sessions[0].OrphanRecoveryOutcome != OrphanRecoveryFresh {
		t.Fatalf("session = %#v", snapshot.Sessions[0])
	}

	if err := backend.RecordRetroRun(ctx, retropkg.RunRecord{
		ProjectID: "detent", Trigger: retropkg.TriggerDaily, StartedAt: base, CompletedAt: base.Add(time.Minute), Filed: 2,
	}); err != nil {
		t.Fatalf("RecordRetroRun() error = %v", err)
	}
	filed, err := backend.RetroFiledOnDay(ctx, "detent", base)
	if err != nil {
		t.Fatalf("RetroFiledOnDay() error = %v", err)
	}
	if filed != 2 {
		t.Fatalf("RetroFiledOnDay() = %d, want 2", filed)
	}
}
