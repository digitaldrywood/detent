package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRecordMergeQueueEnteredResetsTerminalAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 0, 0, 0, time.UTC)
	oldEnteredAt := now.Add(-30 * time.Minute)
	oldFailedAt := now.Add(-20 * time.Minute)
	stageUpdatedAt := now.Add(-2 * time.Minute)
	issue := connector.Issue{
		ID:             "issue-721",
		Identifier:     "digitaldrywood/detent#721",
		State:          "Merging",
		StageUpdatedAt: &stageUpdatedAt,
		PullRequest:    &connector.PullRequest{Number: 729},
	}
	state := newState(normalizeConfig(Config{}))
	state.MergeTimings[issue.ID] = MergeTiming{
		EnteredMergingAt:           oldEnteredAt,
		MergeWorkerSlotAcquiredAt:  oldEnteredAt.Add(time.Minute),
		MergeStartedAt:             oldEnteredAt.Add(2 * time.Minute),
		MergeFailedAt:              oldFailedAt,
		MergeFailureReason:         "merge_conflicts",
		QueueWaitSeconds:           int64(time.Minute / time.Second),
		ActiveMergeDurationSeconds: int64((18 * time.Minute) / time.Second),
		TotalMergingSeconds:        int64((10 * time.Minute) / time.Second),
	}

	got := (&Orchestrator{}).recordMergeQueueEntered(&state, issue, now, "auto_promote")

	if !got.EnteredMergingAt.Equal(stageUpdatedAt) {
		t.Fatalf("EnteredMergingAt = %v, want %v", got.EnteredMergingAt, stageUpdatedAt)
	}
	if !got.MergeFailedAt.IsZero() || got.MergeFailureReason != "" {
		t.Fatalf("terminal failure fields = %v/%q, want reset", got.MergeFailedAt, got.MergeFailureReason)
	}
	if !got.MergeWorkerSlotAcquiredAt.IsZero() || !got.MergeStartedAt.IsZero() {
		t.Fatalf("active attempt fields = %v/%v, want reset", got.MergeWorkerSlotAcquiredAt, got.MergeStartedAt)
	}
	if got.QueueWaitSeconds != int64((2*time.Minute)/time.Second) || got.ActiveMergeDurationSeconds != 0 || got.TotalMergingSeconds != int64((2*time.Minute)/time.Second) {
		t.Fatalf("durations = queue %d active %d total %d, want 120/0/120", got.QueueWaitSeconds, got.ActiveMergeDurationSeconds, got.TotalMergingSeconds)
	}
}
