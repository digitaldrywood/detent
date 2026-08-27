package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLaneMutationReceiptRequiresMatchingActiveAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		issueID       string
		completeFirst bool
		wantErr       error
	}{
		{name: "matching active attempt", issueID: "issue-1999"},
		{name: "different issue", issueID: "issue-other", wantErr: ErrNotFound},
		{name: "terminal attempt", issueID: "issue-1999", completeFirst: true, wantErr: ErrNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			backend := openTestStore(t, ctx)
			now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
			attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
				ProjectID:      "detent",
				IssueID:        "issue-1999",
				Identifier:     "digitaldrywood/detent#1999",
				WorkerType:     "agent",
				Lane:           "In Progress",
				AttemptNumber:  1,
				StartedAt:      now.Add(-time.Minute),
				LeaseExpiresAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatalf("StartWorkAttempt() error = %v", err)
			}
			if tt.completeFirst {
				if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
					AttemptID:     attemptID,
					CompletedAt:   now,
					TerminalState: WorkAttemptTerminalSuccess,
				}); err != nil {
					t.Fatalf("CompleteWorkAttempt() error = %v", err)
				}
			}

			receipt, err := backend.BeginLaneMutation(ctx, LaneMutationStart{
				ProjectID:     "detent",
				IssueID:       tt.issueID,
				WorkAttemptID: attemptID,
				Generation:    7,
				Disposition:   LaneMutationPreserveOwnership,
				FromState:     "In Progress",
				ToState:       "Merging",
				Reason:        "gate_passed",
				RequestedAt:   now,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("BeginLaneMutation() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if receipt.ID != 0 {
					t.Fatalf("receipt = %#v, want zero", receipt)
				}
				return
			}
			if receipt.ID <= 0 || receipt.WorkAttemptID != attemptID || receipt.Generation != 7 || receipt.TrackerResult != LaneMutationTrackerPrepared {
				t.Fatalf("receipt = %#v, want prepared matching owner", receipt)
			}
		})
	}
}

func TestLaneMutationReceiptArbitratesConcurrentAttemptCompletion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	backend := openTestStore(t, ctx)
	now := time.Date(2026, 8, 27, 13, 45, 0, 0, time.UTC)
	for iteration := range 16 {
		issueID := "issue-concurrent-" + strconv.Itoa(iteration)
		attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
			ProjectID:      "detent",
			IssueID:        issueID,
			Identifier:     "digitaldrywood/detent#1999",
			WorkerType:     "agent",
			Lane:           "In Progress",
			StartedAt:      now,
			LeaseExpiresAt: now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("StartWorkAttempt() error = %v", err)
		}

		start := make(chan struct{})
		var receipt LaneMutationReceipt
		var receiptErr error
		var completionErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			receipt, receiptErr = backend.BeginLaneMutation(ctx, LaneMutationStart{
				ProjectID:     "detent",
				IssueID:       issueID,
				WorkAttemptID: attemptID,
				Generation:    uint64(iteration + 1),
				Disposition:   LaneMutationPreserveOwnership,
				FromState:     "In Progress",
				ToState:       "Merging",
				Reason:        "gate_passed",
				RequestedAt:   now,
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			completionErr = backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
				AttemptID:     attemptID,
				CompletedAt:   now.Add(time.Second),
				TerminalState: WorkAttemptTerminalSuccess,
			})
		}()
		close(start)
		wait.Wait()

		if completionErr != nil {
			t.Fatalf("CompleteWorkAttempt() error = %v", completionErr)
		}
		if receiptErr != nil && !errors.Is(receiptErr, ErrNotFound) {
			t.Fatalf("BeginLaneMutation() error = %v, want nil or ErrNotFound", receiptErr)
		}
		if receiptErr == nil && (receipt.ID <= 0 || receipt.WorkAttemptID != attemptID || receipt.Generation != uint64(iteration+1)) {
			t.Fatalf("receipt = %#v, want exact concurrent owner", receipt)
		}
	}
}

func TestLaneMutationReceiptSurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	now := time.Date(2026, 8, 27, 13, 30, 0, 0, time.UTC)
	backend, err := Open(ctx, Config{Backend: BackendSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID:      "detent",
		IssueID:        "issue-1999",
		Identifier:     "digitaldrywood/detent#1999",
		WorkerType:     "agent",
		Lane:           "In Progress",
		StartedAt:      now.Add(-time.Minute),
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	receipt, err := backend.BeginLaneMutation(ctx, LaneMutationStart{
		ProjectID:     "detent",
		IssueID:       "issue-1999",
		WorkAttemptID: attemptID,
		Generation:    13,
		Disposition:   LaneMutationPreserveOwnership,
		FromState:     "In Progress",
		ToState:       "Merging",
		Reason:        "gate_passed",
		RequestedAt:   now,
	})
	if err != nil {
		t.Fatalf("BeginLaneMutation() error = %v", err)
	}
	if err := backend.ResolveLaneMutation(ctx, LaneMutationResolution{
		ReceiptID:     receipt.ID,
		WorkAttemptID: attemptID,
		Generation:    13,
		TrackerResult: LaneMutationTrackerApplied,
		ResolvedAt:    now.Add(time.Second),
	}); err != nil {
		t.Fatalf("ResolveLaneMutation() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	restarted, err := Open(ctx, Config{Backend: BackendSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("Open(restart) error = %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	recovered, err := restarted.LaneMutationReceipt(ctx, LaneMutationLookup{
		ProjectID:     "detent",
		IssueID:       "issue-1999",
		WorkAttemptID: attemptID,
		Generation:    13,
		ToState:       "merging",
	})
	if err != nil {
		t.Fatalf("LaneMutationReceipt() error = %v", err)
	}
	if recovered.ID != receipt.ID || recovered.Disposition != LaneMutationPreserveOwnership || recovered.TrackerResult != LaneMutationTrackerApplied {
		t.Fatalf("recovered receipt = %#v, want %#v", recovered, receipt)
	}
}

func TestLaneMutationReceiptLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	backend := openTestStore(t, ctx)
	now := time.Date(2026, 8, 27, 13, 15, 0, 0, time.UTC)
	attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID:      "detent",
		IssueID:        "issue-1999",
		Identifier:     "digitaldrywood/detent#1999",
		WorkerType:     "agent",
		Lane:           "In Progress",
		AttemptNumber:  1,
		StartedAt:      now.Add(-time.Minute),
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	receipt, err := backend.BeginLaneMutation(ctx, LaneMutationStart{
		ProjectID:     "detent",
		IssueID:       "issue-1999",
		WorkAttemptID: attemptID,
		Generation:    11,
		Disposition:   LaneMutationAcceptCompletion,
		FromState:     "Merging",
		ToState:       "Done",
		Reason:        "pull_request_merged",
		RequestedAt:   now,
	})
	if err != nil {
		t.Fatalf("BeginLaneMutation() error = %v", err)
	}
	if err := backend.ResolveLaneMutation(ctx, LaneMutationResolution{
		ReceiptID:     receipt.ID,
		WorkAttemptID: attemptID,
		Generation:    11,
		TrackerResult: LaneMutationTrackerApplied,
		ResolvedAt:    now.Add(time.Second),
	}); err != nil {
		t.Fatalf("ResolveLaneMutation() error = %v", err)
	}
	consumed, err := backend.ConsumeLaneMutation(ctx, LaneMutationConsumption{
		ReceiptID:     receipt.ID,
		ProjectID:     "detent",
		IssueID:       "issue-1999",
		WorkAttemptID: attemptID,
		Generation:    11,
		ToState:       "Done",
		ConsumedAt:    now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("ConsumeLaneMutation() error = %v", err)
	}
	if consumed.TrackerResult != LaneMutationTrackerApplied || consumed.ConsumedAt.IsZero() || consumed.Disposition != LaneMutationAcceptCompletion {
		t.Fatalf("consumed receipt = %#v", consumed)
	}
	if _, err := backend.ConsumeLaneMutation(ctx, LaneMutationConsumption{
		ReceiptID:     receipt.ID,
		ProjectID:     "detent",
		IssueID:       "issue-1999",
		WorkAttemptID: attemptID,
		Generation:    11,
		ToState:       "Done",
		ConsumedAt:    now.Add(3 * time.Second),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second ConsumeLaneMutation() error = %v, want ErrNotFound", err)
	}
}
