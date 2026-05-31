package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/telemetry"
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

	state := newState(normalizeConfig(Config{}))
	state.Running["i-2"] = Running{
		Issue:      connector.Issue{ID: "i-2", Identifier: "ISS-2", Title: "Two", State: "In Progress", URL: "u2"},
		Attempt:    1,
		StartedAt:  startedAt,
		WorkerHost: "host-b",
	}
	state.Running["i-1"] = Running{
		Issue:     connector.Issue{ID: "i-1", Identifier: "ISS-1", Title: "One", State: "In Progress"},
		Attempt:   0,
		StartedAt: startedAt,
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
	if snapshot.Running[0].Identifier != "ISS-1" || snapshot.Running[0].Title != "One" {
		t.Fatalf("Running[0] issue mapping = %#v", snapshot.Running[0].Issue)
	}
	if !snapshot.Running[0].StartedAt.Equal(startedAt) {
		t.Fatalf("Running[0].StartedAt = %v, want %v", snapshot.Running[0].StartedAt, startedAt)
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

	wantTokens := telemetry.Tokens{Input: 100, Output: 50, Total: 150, RuntimeSeconds: 120}
	if snapshot.Tokens != wantTokens {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, wantTokens)
	}
	if snapshot.RateLimits == nil || snapshot.RateLimits.LimitID != "lim" {
		t.Fatalf("RateLimits = %#v, want lim", snapshot.RateLimits)
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
