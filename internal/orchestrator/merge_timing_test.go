package orchestrator

import (
	"log/slog"
	"strings"
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

func TestMarkMergeStartedPreservesCurrentHeadCIWaitDeadline(t *testing.T) {
	t.Parallel()

	firstStart := time.Date(2026, 8, 7, 18, 24, 27, 0, time.UTC)
	retryStart := firstStart.Add(2*time.Minute + 17*time.Second)
	issue := connector.Issue{
		ID:         "issue-current-head-ci-wait",
		Identifier: "digitaldrywood/detent#1634",
		State:      "Merging",
		PullRequest: &connector.PullRequest{
			Number: 1636,
			State:  "OPEN",
		},
	}
	state := newState(normalizeConfig(Config{}))
	orch := &Orchestrator{}

	orch.markMergeStarted(&state, issue, firstStart)
	got := orch.markMergeStarted(&state, issue, retryStart)

	if !got.CIWaitStartedAt.Equal(firstStart) {
		t.Fatalf("CIWaitStartedAt = %s, want original start %s", got.CIWaitStartedAt, firstStart)
	}
	if !got.MergeStartedAt.Equal(retryStart) {
		t.Fatalf("MergeStartedAt = %s, want current attempt start %s", got.MergeStartedAt, retryStart)
	}
}

func TestRecordMergeFailedRejectsFailureBeforeMergeEntry(t *testing.T) {
	t.Parallel()

	enteredMergingAt := time.Date(2026, 7, 29, 5, 1, 8, 0, time.UTC)
	failedAt := enteredMergingAt.Add(-time.Minute - 2*time.Second)
	issue := connector.Issue{
		ID:             "issue-invalid-merge-timing",
		Identifier:     "getparable/parable#1946",
		State:          "Merging",
		StageUpdatedAt: &enteredMergingAt,
	}
	original := MergeTiming{EnteredMergingAt: enteredMergingAt}
	state := newState(normalizeConfig(Config{}))
	state.MergeTimings[issue.ID] = original
	var logs strings.Builder
	orch := &Orchestrator{logger: slog.New(slog.NewTextHandler(&logs, nil))}

	got := orch.recordMergeFailed(&state, issue, failedAt, "runner_startup_timeout", nil)

	if got != original {
		t.Fatalf("recordMergeFailed() = %#v, want unchanged %#v", got, original)
	}
	if timing := state.MergeTimings[issue.ID]; timing != original {
		t.Fatalf("MergeTimings[%q] = %#v, want unchanged %#v", issue.ID, timing, original)
	}
	for _, fragment := range []string{
		"level=ERROR",
		"msg=merge_failed_rejected",
		"reason=runner_startup_timeout",
		"entered_merging_at=2026-07-29T05:01:08Z",
		"merge_failed_at=2026-07-29T05:00:06Z",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "msg=merge_failed ") {
		t.Fatalf("logs %q contain merge_failed record", logs.String())
	}
}

func TestObserveMergeSlotConcentrationWarnsOncePerWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:         "issue-slot-monopoly",
		Identifier: "digitaldrywood/video-studio#41",
		State:      "Merging",
		PullRequest: &connector.PullRequest{
			Number: 45,
			State:  "OPEN",
		},
	}
	cfg := normalizeConfig(Config{})
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	for index := range mergeSlotConcentrationMinimumCount + 1 {
		orch.observeMergeSlotConcentration(&state, issue, now.Add(time.Duration(index)*time.Minute))
	}

	logText := logs.String()
	if count := strings.Count(logText, "merge_worker_slot_concentration"); count != 1 {
		t.Fatalf("merge_worker_slot_concentration count = %d, want 1; logs = %q", count, logText)
	}
	for _, fragment := range []string{
		"issue_acquisitions=5",
		"total_acquisitions=5",
		"share_percent=100",
		"pull_request_number=45",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs = %q, missing %q", logText, fragment)
		}
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "merge_worker_slot_concentration" {
		t.Fatalf("recent events = %#v, want one concentration warning", state.RecentEvents)
	}
}
