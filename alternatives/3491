package store

import (
	"context"
	"testing"
	"time"
)

func TestOperatorStopPersistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := openTestStore(t, ctx)
	now := time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC)
	id, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{ProjectID: "detent", IssueID: "issue-1311", Identifier: "digitaldrywood/detent#1311", WorkerType: "agent", Lane: "In Progress", AttemptNumber: 3, StartedAt: now, LeaseExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Second), Status: WorkAttemptStatusTerminal, TerminalState: WorkAttemptTerminalOperatorStopped, Phase: "operator_stop_pending", WorkerMetadataJSON: `{"operator_stop":{"issue_id":"issue-1311","destination":"Blocked","outcome":"pending"}}`}); err != nil {
		t.Fatalf("CompleteWorkAttempt() error = %v", err)
	}
	pending, err := backend.ListPendingOperatorStops(ctx, "detent")
	if err != nil {
		t.Fatalf("ListPendingOperatorStops() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].TerminalState != WorkAttemptTerminalOperatorStopped {
		t.Fatalf("pending = %#v, want operator-stopped attempt %d", pending, id)
	}
	if err := backend.UpdateOperatorStop(ctx, OperatorStopUpdate{AttemptID: id, Phase: "operator_stop_succeeded", StatusMessage: "moved to Blocked", WorkerMetadataJSON: `{"operator_stop":{"issue_id":"issue-1311","destination":"Blocked","outcome":"succeeded"}}`, NextAction: "await operator resume"}); err != nil {
		t.Fatalf("UpdateOperatorStop() error = %v", err)
	}
	pending, err = backend.ListPendingOperatorStops(ctx, "detent")
	if err != nil {
		t.Fatalf("ListPendingOperatorStops() after success error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after success = %#v, want none", pending)
	}
	receipt, err := backend.WorkAttempt(ctx, id)
	if err != nil {
		t.Fatalf("WorkAttempt() error = %v", err)
	}
	if receipt.Phase != "operator_stop_succeeded" || receipt.NextAction != "await operator resume" {
		t.Fatalf("receipt = %#v, want successful operator stop", receipt)
	}
}
