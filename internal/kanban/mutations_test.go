package kanban

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestMutationTrackerCardStateByDataSeq(t *testing.T) {
	t.Parallel()

	type observation struct {
		snapshotState string
		dataSeq       uint64
		want          string
	}
	tests := []struct {
		name         string
		observations []observation
		wantPending  bool
		wantNotice   RevertNotice
	}{
		{
			name: "same-seq republish holds optimistic state",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 7, want: "Todo"},
				{snapshotState: "Blocked", dataSeq: 7, want: "Todo"},
			},
			wantPending: true,
		},
		{
			name: "newer-seq match confirms and drops entry",
			observations: []observation{
				{snapshotState: "Todo", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 9, want: "Backlog"},
			},
		},
		{
			name: "same newer contradicting seq holds optimistic state",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
			},
			wantPending: true,
		},
		{
			name: "older contradiction after counted seq does not increment",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 9, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 10, want: "Backlog"},
			},
			wantNotice: RevertNotice{Identifier: "DDW-433", From: "Todo", To: "Backlog"},
		},
		{
			name: "contradicting polls revert at limit with notice",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 9, want: "Backlog"},
			},
			wantNotice: RevertNotice{Identifier: "DDW-433", From: "Todo", To: "Backlog"},
		},
		{
			name: "third state reverts immediately with notice",
			observations: []observation{
				{snapshotState: "Blocked", dataSeq: 8, want: "Blocked"},
			},
			wantNotice: RevertNotice{Identifier: "DDW-433", From: "Todo", To: "Blocked"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := NewMutationTracker()
			issue := telemetry.Issue{
				ID:         "confirmed-card",
				Identifier: "DDW-433",
				ProjectID:  "detent",
				Title:      "Confirmed pending card",
				State:      "Backlog",
			}
			tracker.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 7)

			for _, observation := range tt.observations {
				got := tracker.CardState("project:detent", issue.ID, observation.snapshotState, observation.dataSeq)
				if got != observation.want {
					t.Fatalf("CardState(%q, %d) = %q, want %q", observation.snapshotState, observation.dataSeq, got, observation.want)
				}
			}
			if got := pendingStateExists(tracker, "project:detent", issue.ID); got != tt.wantPending {
				t.Fatalf("pending state exists = %t, want %t", got, tt.wantPending)
			}

			notices := tracker.ConsumeRevertNotices("project:detent", "detent")
			if tt.wantNotice.Identifier == "" {
				if len(notices) != 0 {
					t.Fatalf("ConsumeRevertNotices() = %#v, want none", notices)
				}
				return
			}
			if len(notices) != 1 {
				t.Fatalf("ConsumeRevertNotices() len = %d, want 1: %#v", len(notices), notices)
			}
			assertRevertNotice(t, notices[0], tt.wantNotice)
			if got := tracker.ConsumeRevertNotices("project:detent", "detent"); len(got) != 0 {
				t.Fatalf("second ConsumeRevertNotices() = %#v, want drained", got)
			}
		})
	}
}

func TestMutationTrackerCardStateConcurrentSameContradictingSeq(t *testing.T) {
	t.Parallel()

	tracker := NewMutationTracker()
	issue := telemetry.Issue{
		ID:         "race-card",
		Identifier: "DDW-436",
		ProjectID:  "detent",
		Title:      "Race pending card",
		State:      "Backlog",
	}
	tracker.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 7)

	start := make(chan struct{})
	errs := make(chan string, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				if got := tracker.CardState("project:detent", issue.ID, "Backlog", 8); got != "Todo" {
					select {
					case errs <- got:
					default:
					}
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for got := range errs {
		t.Fatalf("CardState() = %q, want Todo", got)
	}
	if got := pendingStateExists(tracker, "project:detent", issue.ID); !got {
		t.Fatalf("pending state exists = %t, want true", got)
	}
	if got := tracker.ConsumeRevertNotices("project:detent", "detent"); len(got) != 0 {
		t.Fatalf("ConsumeRevertNotices() = %#v, want none", got)
	}
}

func TestMutationTrackerPendingMovedCards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		snapshot    telemetry.Snapshot
		wantIssues  []string
		wantPending bool
	}{
		{
			name: "missing pending card is reinserted",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
				Refresh: telemetry.Refresh{DataSeq: 1},
			},
			wantIssues:  []string{"DDW-434:Todo"},
			wantPending: true,
		},
		{
			name: "visible pending card is not duplicated",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
				Refresh: telemetry.Refresh{DataSeq: 1},
				BoardIssues: []telemetry.Issue{{
					ID:         "pending-card",
					Identifier: "DDW-434",
					ProjectID:  "detent",
					State:      "Backlog",
				}},
			},
			wantPending: true,
		},
		{
			name: "completed row confirms and drops pending card",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
				Refresh: telemetry.Refresh{DataSeq: 2},
				Completed: []telemetry.Completed{{
					Issue: telemetry.Issue{
						ID:         "pending-card",
						Identifier: "DDW-434",
						ProjectID:  "detent",
						State:      "Todo",
					},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := NewMutationTracker()
			tracker.NoteCardState("project:detent", "detent", telemetry.Issue{
				ID:         "pending-card",
				Identifier: "DDW-434",
				ProjectID:  "detent",
				Title:      "Pending card",
				State:      "Backlog",
			}, "Backlog", "Todo", 1)

			got := tracker.PendingMovedCards("project:detent", "detent", tt.snapshot)
			if len(got) != len(tt.wantIssues) {
				t.Fatalf("PendingMovedCards() len = %d, want %d: %#v", len(got), len(tt.wantIssues), got)
			}
			for i, issue := range got {
				key := issue.Identifier + ":" + issue.State
				if key != tt.wantIssues[i] {
					t.Fatalf("PendingMovedCards()[%d] = %q, want %q", i, key, tt.wantIssues[i])
				}
			}
			if got := pendingStateExists(tracker, "project:detent", "pending-card"); got != tt.wantPending {
				t.Fatalf("pending state exists = %t, want %t", got, tt.wantPending)
			}
		})
	}
}

func TestMutationTrackerSnapshotCardStates(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{
		ID:         "visible-card",
		Identifier: "DDW-435",
		ProjectID:  "detent",
		State:      "Backlog",
	}
	tests := []struct {
		name        string
		snapshot    telemetry.Snapshot
		wantState   string
		wantPending bool
	}{
		{
			name: "same sequence overlays optimistic state",
			snapshot: telemetry.Snapshot{
				Project:     telemetry.Project{ID: "detent"},
				Refresh:     telemetry.Refresh{DataSeq: 1},
				BoardIssues: []telemetry.Issue{issue},
			},
			wantState:   "Todo",
			wantPending: true,
		},
		{
			name: "newer matching sequence confirms pending state",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
				Refresh: telemetry.Refresh{DataSeq: 2},
				BoardIssues: []telemetry.Issue{{
					ID:         "visible-card",
					Identifier: "DDW-435",
					ProjectID:  "detent",
					State:      "Todo",
				}},
			},
			wantState: "Todo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := NewMutationTracker()
			tracker.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 1)
			states := tracker.SnapshotCardStates("project:detent", "detent", tt.snapshot)
			stateKey := MutationStateKey("project:detent", "visible-card")
			if got := states[stateKey]; got != tt.wantState {
				t.Fatalf("SnapshotCardStates()[%q] = %q, want %q", stateKey, got, tt.wantState)
			}
			if got := pendingStateExists(tracker, "project:detent", "visible-card"); got != tt.wantPending {
				t.Fatalf("pending state exists = %t, want %t", got, tt.wantPending)
			}
		})
	}
}

func TestMutationTrackerWithLockReturnsCallbackError(t *testing.T) {
	t.Parallel()

	tracker := NewMutationTracker()
	wantErr := errors.New("callback failed")
	called := false

	err := tracker.WithLock(" ", func() error {
		called = true
		return wantErr
	})
	if !called {
		t.Fatalf("callback was not called")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithLock() error = %v, want %v", err, wantErr)
	}
}

func TestMutationTrackerRemovalLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		snapshotState string
		dataSeq       uint64
		expired       bool
		wantRemoved   bool
		wantPending   bool
	}{
		{name: "same seq hides recorded state", snapshotState: "Backlog", dataSeq: 5, wantRemoved: true, wantPending: true},
		{name: "same seq hides changed state", snapshotState: "Done", dataSeq: 5, wantRemoved: true, wantPending: true},
		{name: "newer seq hides recorded state", snapshotState: "Backlog", dataSeq: 6, wantRemoved: true, wantPending: true},
		{name: "newer seq releases changed state", snapshotState: "Done", dataSeq: 6},
		{name: "ttl expires as backstop", snapshotState: "Backlog", dataSeq: 5, expired: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := NewMutationTracker()
			tracker.NoteCardRemoved("project:detent", "removed-card", "Backlog", 5)
			if tt.expired {
				stateKey := MutationStateKey("project:detent", "removed-card")
				tracker.mu.Lock()
				removed := tracker.removed[stateKey]
				removed.removedAt = time.Now().Add(-removalPendingTTL - time.Minute)
				tracker.removed[stateKey] = removed
				tracker.mu.Unlock()
			}

			got := tracker.CardRemoved("project:detent", "removed-card", tt.snapshotState, tt.dataSeq)
			if got != tt.wantRemoved {
				t.Fatalf("CardRemoved(%q, %d) = %t, want %t", tt.snapshotState, tt.dataSeq, got, tt.wantRemoved)
			}
			if pending := pendingRemovalExists(tracker, "project:detent", "removed-card"); pending != tt.wantPending {
				t.Fatalf("pending removal exists = %t, want %t", pending, tt.wantPending)
			}
		})
	}
}

func assertRevertNotice(t *testing.T, got RevertNotice, want RevertNotice) {
	t.Helper()

	if got.Identifier != want.Identifier || got.From != want.From || got.To != want.To {
		t.Fatalf("revert notice = %#v, want identifier/from/to %#v", got, want)
	}
	if got.At.IsZero() {
		t.Fatalf("revert notice At is zero")
	}
}

func pendingStateExists(tracker *MutationTracker, key string, issueID string) bool {
	stateKey := MutationStateKey(key, issueID)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	_, ok := tracker.states[stateKey]
	return ok
}

func pendingRemovalExists(tracker *MutationTracker, key string, issueID string) bool {
	stateKey := MutationStateKey(key, issueID)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	_, ok := tracker.removed[stateKey]
	return ok
}
