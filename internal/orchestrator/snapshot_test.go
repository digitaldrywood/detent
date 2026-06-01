package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStateSnapshotEmpty(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newState(normalizeConfig(Config{}))

	snapshot := state.Snapshot(now)

	if !snapshot.GeneratedAt.Equal(now) {
		t.Fatalf("GeneratedAt = %v, want %v", snapshot.GeneratedAt, now)
	}
	if snapshot.Counts != (telemetry.Counts{}) {
		t.Fatalf("Counts = %#v, want zero", snapshot.Counts)
	}
	if len(snapshot.Running) != 0 {
		t.Fatalf("Running = %#v, want empty", snapshot.Running)
	}
	if len(snapshot.Queue) != 0 {
		t.Fatalf("Queue = %#v, want empty", snapshot.Queue)
	}
	if len(snapshot.Blocked) != 0 {
		t.Fatalf("Blocked = %#v, want empty", snapshot.Blocked)
	}
	if len(snapshot.Completed) != 0 {
		t.Fatalf("Completed = %#v, want empty", snapshot.Completed)
	}
}

func TestStateSnapshotPopulated(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	startedAt := now.Add(-2 * time.Minute)
	dueAt := now.Add(30 * time.Second)
	completedAt := now.Add(-time.Minute)
	blockedAt := now.Add(-3 * time.Minute)
	pipelineUpdatedAt := now.Add(-4 * time.Minute)

	state := newState(normalizeConfig(Config{}))
	state.LastRefreshAt = now.Add(-30 * time.Second)
	state.NextRefreshAt = now.Add(30 * time.Second)
	state.Pipeline = []connector.Issue{
		{
			ID:         "i-pr",
			Identifier: "ISS-PR",
			Title:      "Pipeline PR",
			State:      "Human Review",
			Labels:     []string{"enhancement"},
			UpdatedAt:  &pipelineUpdatedAt,
			PullRequest: &connector.PullRequest{
				Number:           142,
				URL:              "https://github.com/digitaldrywood/detent/pull/142",
				State:            "OPEN",
				CIStatus:         "pending",
				CodexReviewState: "P2",
			},
		},
	}
	state.Running["i-2"] = Running{
		Issue:           connector.Issue{ID: "i-2", Identifier: "ISS-2", Title: "Two", State: "In Progress", URL: "u2"},
		Attempt:         1,
		StartedAt:       startedAt,
		WorkerHost:      "host-b",
		ProcessIdentity: "4242",
		SessionID:       "thread-2-turn-2",
		TurnCount:       2,
		LastEventAt:     now.Add(-10 * time.Second),
		LastEvent:       "agent_message_delta",
		LastMessage:     "editing dashboard telemetry",
		RecentEvents: []telemetry.ActivityEvent{
			{At: now.Add(-12 * time.Second), Event: "turn_started", Message: "turn started"},
			{At: now.Add(-10 * time.Second), Event: "agent_message_delta", Message: "editing dashboard telemetry"},
		},
		DiffStats: DiffStats{FilesChanged: 3, AddedLines: 12, RemovedLines: 4, Status: "ok"},
		Tokens:    CodexTotals{InputTokens: 20, OutputTokens: 8, TotalTokens: 28, RuntimeSeconds: 30},
	}
	state.Running["i-1"] = Running{
		Issue:     connector.Issue{ID: "i-1", Identifier: "ISS-1", Title: "One", State: "In Progress"},
		Attempt:   0,
		StartedAt: startedAt,
		TurnCount: 1,
		Tokens:    CodexTotals{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, RuntimeSeconds: 15},
	}
	state.Retry["i-3"] = Retry{
		Issue:      connector.Issue{ID: "i-3", Identifier: "ISS-3", Title: "Three", State: "Todo"},
		Attempt:    2,
		DueAt:      dueAt,
		Error:      "boom",
		WorkerHost: "host-c",
	}
	state.Blocked["i-4"] = Blocked{
		Issue:     connector.Issue{ID: "i-4", Identifier: "ISS-4", Title: "Four", State: "Todo"},
		Reason:    "blocked by non-terminal dependency",
		BlockedAt: blockedAt,
	}
	state.Completed["i-5"] = Completed{
		Issue:       connector.Issue{ID: "i-5", Identifier: "ISS-5", Title: "Five", State: "Done"},
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		FinalState:  FinalStateCompleted,
		Tokens:      CodexTotals{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, RuntimeSeconds: 60},
	}
	state.CodexTotals = CodexTotals{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, RuntimeSeconds: 120}
	state.RateLimits = &telemetry.RateLimits{LimitID: "lim", LimitName: "name"}

	snapshot := state.Snapshot(now)

	wantCounts := telemetry.Counts{Running: 2, Queue: 1, Blocked: 1, Completed: 1}
	if snapshot.Counts != wantCounts {
		t.Fatalf("Counts = %#v, want %#v", snapshot.Counts, wantCounts)
	}

	if len(snapshot.Pipeline) != 1 {
		t.Fatalf("Pipeline len = %d, want 1", len(snapshot.Pipeline))
	}
	pipeline := snapshot.Pipeline[0]
	if pipeline.ID != "i-pr" || pipeline.State != "Human Review" || pipeline.UpdatedAt == nil || !pipeline.UpdatedAt.Equal(pipelineUpdatedAt) {
		t.Fatalf("Pipeline[0] = %#v", pipeline)
	}
	if pipeline.PullRequest == nil || pipeline.PullRequest.Number != 142 || pipeline.PullRequest.CIStatus != "pending" || pipeline.PullRequest.CodexReviewState != "P2" {
		t.Fatalf("Pipeline[0].PullRequest = %#v", pipeline.PullRequest)
	}

	if len(snapshot.Running) != 2 {
		t.Fatalf("Running len = %d, want 2", len(snapshot.Running))
	}
	// Deterministic ordering by issue id.
	if snapshot.Running[0].ID != "i-1" || snapshot.Running[1].ID != "i-2" {
		t.Fatalf("Running order = [%s, %s], want [i-1, i-2]", snapshot.Running[0].ID, snapshot.Running[1].ID)
	}
	if snapshot.Running[1].WorkerHost != "host-b" {
		t.Fatalf("Running[1].WorkerHost = %q, want host-b", snapshot.Running[1].WorkerHost)
	}
	if snapshot.Running[1].ProcessIdentity != "4242" {
		t.Fatalf("Running[1].ProcessIdentity = %q, want 4242", snapshot.Running[1].ProcessIdentity)
	}
	if snapshot.Running[0].Identifier != "ISS-1" || snapshot.Running[0].Title != "One" {
		t.Fatalf("Running[0] issue mapping = %#v", snapshot.Running[0].Issue)
	}
	if !snapshot.Running[0].StartedAt.Equal(startedAt) {
		t.Fatalf("Running[0].StartedAt = %v, want %v", snapshot.Running[0].StartedAt, startedAt)
	}
	if snapshot.Running[0].TurnCount != 1 || snapshot.Running[0].Tokens.Total != 3 {
		t.Fatalf("Running[0] live usage = turns %d tokens %#v, want 1 turn and 3 tokens", snapshot.Running[0].TurnCount, snapshot.Running[0].Tokens)
	}
	if snapshot.Running[1].RuntimeSeconds != 30 {
		t.Fatalf("Running[1].RuntimeSeconds = %v, want 30", snapshot.Running[1].RuntimeSeconds)
	}
	if snapshot.Running[1].SessionID != "thread-2-turn-2" || snapshot.Running[1].LastEvent != "agent_message_delta" {
		t.Fatalf("Running[1] live activity = %#v", snapshot.Running[1])
	}
	if snapshot.Running[1].LastEventAt == nil || !snapshot.Running[1].LastEventAt.Equal(now.Add(-10*time.Second)) {
		t.Fatalf("Running[1].LastEventAt = %v", snapshot.Running[1].LastEventAt)
	}
	if snapshot.Running[1].LastMessage != "editing dashboard telemetry" {
		t.Fatalf("Running[1].LastMessage = %q", snapshot.Running[1].LastMessage)
	}
	if len(snapshot.Running[1].RecentEvents) != 2 || snapshot.Running[1].RecentEvents[1].Event != "agent_message_delta" {
		t.Fatalf("Running[1].RecentEvents = %#v", snapshot.Running[1].RecentEvents)
	}
	if snapshot.Running[1].DiffFiles != 3 || snapshot.Running[1].DiffAdded != 12 || snapshot.Running[1].DiffRemoved != 4 || snapshot.Running[1].DiffStatus != "ok" {
		t.Fatalf("Running[1] diff = %#v", snapshot.Running[1])
	}

	if len(snapshot.Queue) != 1 {
		t.Fatalf("Queue len = %d, want 1", len(snapshot.Queue))
	}
	q := snapshot.Queue[0]
	if q.ID != "i-3" || q.Attempt != 2 || q.Error != "boom" || q.WorkerHost != "host-c" {
		t.Fatalf("Queue[0] = %#v", q)
	}
	if q.DueAt == nil || !q.DueAt.Equal(dueAt) {
		t.Fatalf("Queue[0].DueAt = %v, want %v", q.DueAt, dueAt)
	}

	if len(snapshot.Blocked) != 1 {
		t.Fatalf("Blocked len = %d, want 1", len(snapshot.Blocked))
	}
	b := snapshot.Blocked[0]
	if b.ID != "i-4" || b.Error != "blocked by non-terminal dependency" {
		t.Fatalf("Blocked[0] = %#v", b)
	}
	if b.BlockedAt == nil || !b.BlockedAt.Equal(blockedAt) {
		t.Fatalf("Blocked[0].BlockedAt = %v, want %v", b.BlockedAt, blockedAt)
	}

	if len(snapshot.Completed) != 1 {
		t.Fatalf("Completed len = %d, want 1", len(snapshot.Completed))
	}
	c := snapshot.Completed[0]
	if c.ID != "i-5" || c.FinalState != FinalStateCompleted {
		t.Fatalf("Completed[0] = %#v", c)
	}
	if !c.CompletedAt.Equal(completedAt) {
		t.Fatalf("Completed[0].CompletedAt = %v, want %v", c.CompletedAt, completedAt)
	}
	if c.Tokens.Total != 15 {
		t.Fatalf("Completed[0].Tokens.Total = %d, want 15", c.Tokens.Total)
	}

	wantTokens := telemetry.Tokens{Input: 122, Output: 59, Total: 181, RuntimeSeconds: 165}
	if snapshot.Tokens != wantTokens {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, wantTokens)
	}
	if snapshot.RateLimits == nil || snapshot.RateLimits.LimitID != "lim" {
		t.Fatalf("RateLimits = %#v, want lim", snapshot.RateLimits)
	}
	if snapshot.Refresh.PollIntervalSeconds != 30 || snapshot.Refresh.LastRefreshAt == nil || snapshot.Refresh.NextRefreshAt == nil {
		t.Fatalf("Refresh = %#v, want poll interval and refresh timestamps", snapshot.Refresh)
	}
}

func TestStateSnapshotBudgetRefusals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	maxUSD := 12.5
	state := newState(normalizeConfig(Config{}))
	state.BudgetRefusals["i-9"] = BudgetRefusal{
		Issue:            connector.Issue{ID: "i-9", Identifier: "ISS-9"},
		Code:             "per_issue",
		Message:          "too expensive",
		CurrentSpendUSD:  3,
		ProjectedCostUSD: 9,
		MaxUSD:           &maxUSD,
		RefusedAt:        now,
	}

	snapshot := state.Snapshot(now)

	if len(snapshot.Budget.Refusals) != 1 {
		t.Fatalf("Budget.Refusals len = %d, want 1", len(snapshot.Budget.Refusals))
	}
	refusal := snapshot.Budget.Refusals[0]
	if refusal.IssueID != "i-9" || refusal.Identifier != "ISS-9" || refusal.Code != "per_issue" {
		t.Fatalf("Refusals[0] = %#v", refusal)
	}
	if refusal.MaxUSD == nil || *refusal.MaxUSD != maxUSD {
		t.Fatalf("Refusals[0].MaxUSD = %v, want %v", refusal.MaxUSD, maxUSD)
	}
}

func TestStateSnapshotDeterministicOrdering(t *testing.T) {
	t.Parallel()

	now := time.Now()
	state := newState(normalizeConfig(Config{}))
	for _, id := range []string{"c", "a", "b"} {
		state.Running[id] = Running{Issue: connector.Issue{ID: id, Identifier: id, Title: id, State: "In Progress"}}
	}

	first := state.Snapshot(now)
	second := state.Snapshot(now)

	for i := range first.Running {
		if first.Running[i].ID != second.Running[i].ID {
			t.Fatalf("non-deterministic ordering at %d: %s vs %s", i, first.Running[i].ID, second.Running[i].ID)
		}
	}
	if first.Running[0].ID != "a" || first.Running[1].ID != "b" || first.Running[2].ID != "c" {
		t.Fatalf("Running order = [%s,%s,%s], want [a,b,c]",
			first.Running[0].ID, first.Running[1].ID, first.Running[2].ID)
	}
}
