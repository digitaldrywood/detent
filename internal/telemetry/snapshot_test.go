package telemetry_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestSnapshotJSONShape(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 5, 30, 22, 15, 0, 0, time.UTC)
	startedAt := generatedAt.Add(-5 * time.Minute)
	completedAt := generatedAt.Add(-time.Minute)
	perDay := 50.0
	perIssue := 5.0

	snapshot := telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Project: telemetry.Project{
			DisplayName: "Detent",
			URL:         "https://github.com/digitaldrywood/detent",
		},
		Instance: telemetry.Instance{
			Name:                    "release-captain",
			GitHubLogin:             "detent-bot",
			AuthorizationScope:      "assignee in @me (detent-bot, release-captain)",
			AuthorizationConfigured: true,
		},
		DashboardURL: "http://localhost:4101",
		Refresh: telemetry.Refresh{
			PollIntervalSeconds: 30,
			NextRefreshAt:       new(generatedAt.Add(30 * time.Second)),
		},
		Counts: telemetry.Counts{
			Running:   1,
			Queue:     2,
			Blocked:   3,
			Completed: 4,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-1",
					Identifier: "DD-1",
					State:      "In Progress",
					Title:      "Port hub",
					URL:        "https://example.com/issues/1",
				},
				ProcessIdentity: "4242",
				SessionID:       "thread-1",
				TurnCount:       2,
				StartedAt:       startedAt,
				RuntimeSeconds:  300,
				RecentEvents: []telemetry.ActivityEvent{
					{At: generatedAt.Add(-10 * time.Second), Event: "turn_started", Message: "turn started"},
					{At: generatedAt, Event: "agent_message_delta", Message: "writing telemetry"},
				},
				DiffAdded:   4,
				DiffRemoved: 2,
				DiffFiles:   3,
				DiffStatus:  "ok",
				Tokens: telemetry.Tokens{
					Input:  10,
					Output: 20,
					Total:  30,
				},
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:         "issue-2",
					Identifier: "DD-2",
				},
				Attempt: 2,
				Error:   "no available orchestrator slots",
			},
		},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "issue-3",
					Identifier: "DD-3",
					State:      "Blocked",
				},
				Error: "dependency #2 is not merged",
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-4",
					Identifier: "DD-4",
				},
				StartedAt:      startedAt,
				CompletedAt:    completedAt,
				Turns:          3,
				RuntimeSeconds: 240,
				FinalState:     "Done",
				Model:          "gpt-5",
				Tokens: telemetry.Tokens{
					Input:  100,
					Output: 200,
					Total:  300,
				},
			},
		},
		Budget: telemetry.Budget{
			Enabled:          true,
			PerDayMaxUSD:     &perDay,
			PerIssueMaxUSD:   &perIssue,
			CurrentSpendUSD:  12.5,
			ProjectedCostUSD: 0.75,
			Days: []telemetry.BudgetDay{
				{Date: "2026-05-30", SpendUSD: 12.5},
			},
		},
		RateLimits: &telemetry.RateLimits{
			LimitID: "codex-primary",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      90,
				Limit:          100,
				ResetInSeconds: 60,
			},
			Credits: &telemetry.RateLimitBucket{
				HasCredits: true,
				Unlimited:  false,
				Balance:    "7.25",
			},
		},
		Tokens: telemetry.Tokens{
			Input:          110,
			Output:         220,
			Total:          330,
			RuntimeSeconds: 540,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 42.5,
			WindowSeconds:   60,
			Tokens:          2550,
		},
		LifetimeTotals: telemetry.LifetimeTotals{
			Available:      true,
			InputTokens:    1000,
			OutputTokens:   500,
			TotalTokens:    1500,
			RuntimeSeconds: 600,
			Sessions:       6,
			Runs:           2,
		},
		TokenTrend: []telemetry.TokenTrendPoint{
			{
				At:     generatedAt.Add(-time.Minute),
				Input:  50,
				Output: 100,
				Total:  150,
			},
			{
				At:     generatedAt,
				Input:  110,
				Output: 220,
				Total:  330,
			},
		},
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"generated_at",
		"project",
		"instance",
		"dashboard_url",
		"refresh",
		"counts",
		"running",
		"queue",
		"blocked",
		"completed",
		"budget",
		"rate_limits",
		"tokens",
		"throughput",
		"lifetime_totals",
		"token_trend",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("snapshot JSON missing %q: %s", key, string(data))
		}
	}

	project := got["project"].(map[string]any)
	if project["display_name"] != "Detent" || project["url"] != "https://github.com/digitaldrywood/detent" {
		t.Fatalf("project = %#v", project)
	}
	instance := got["instance"].(map[string]any)
	if instance["name"] != "release-captain" || instance["github_login"] != "detent-bot" {
		t.Fatalf("instance identity = %#v", instance)
	}
	if instance["authorization_scope"] != "assignee in @me (detent-bot, release-captain)" {
		t.Fatalf("instance authorization_scope = %#v", instance)
	}
	if instance["authorization_configured"] != true {
		t.Fatalf("instance authorization_configured = %#v", instance)
	}
	if got["dashboard_url"] != "http://localhost:4101" {
		t.Fatalf("dashboard_url = %#v", got["dashboard_url"])
	}
	refresh := got["refresh"].(map[string]any)
	if refresh["poll_interval_seconds"] != float64(30) || refresh["next_refresh_at"] != "2026-05-30T22:15:30Z" {
		t.Fatalf("refresh = %#v", refresh)
	}

	counts := got["counts"].(map[string]any)
	if counts["running"] != float64(1) || counts["queue"] != float64(2) || counts["blocked"] != float64(3) || counts["completed"] != float64(4) {
		t.Fatalf("counts = %#v", counts)
	}

	running := got["running"].([]any)[0].(map[string]any)
	if running["issue_id"] != "issue-1" || running["identifier"] != "DD-1" {
		t.Fatalf("running row = %#v", running)
	}
	if running["process_identity"] != "4242" {
		t.Fatalf("running process identity = %#v", running)
	}
	recentEvents := running["recent_events"].([]any)
	if len(recentEvents) != 2 || recentEvents[1].(map[string]any)["message"] != "writing telemetry" {
		t.Fatalf("running recent_events = %#v", recentEvents)
	}
	if running["diff_added"] != float64(4) || running["diff_removed"] != float64(2) || running["diff_files"] != float64(3) || running["diff_status"] != "ok" {
		t.Fatalf("running diff fields = %#v", running)
	}
	if _, ok := running["issue"]; ok {
		t.Fatalf("running row has nested issue: %#v", running)
	}

	budget := got["budget"].(map[string]any)
	if budget["per_day_max_usd"] != 50.0 || budget["per_issue_max_usd"] != 5.0 {
		t.Fatalf("budget caps = %#v", budget)
	}
	if budget["projected_cost_usd"] != 0.75 {
		t.Fatalf("budget projected cost = %#v", budget)
	}
	days := budget["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["date"] != "2026-05-30" || days[0].(map[string]any)["spend_usd"] != 12.5 {
		t.Fatalf("budget days = %#v", days)
	}

	rateLimits := got["rate_limits"].(map[string]any)
	if rateLimits["limit_id"] != "codex-primary" {
		t.Fatalf("rate_limits = %#v", rateLimits)
	}
	credits := rateLimits["credits"].(map[string]any)
	if credits["has_credits"] != true || credits["balance"] != "7.25" {
		t.Fatalf("credits = %#v", credits)
	}

	tokens := got["tokens"].(map[string]any)
	if tokens["input_tokens"] != float64(110) || tokens["output_tokens"] != float64(220) || tokens["total_tokens"] != float64(330) {
		t.Fatalf("tokens = %#v", tokens)
	}

	throughput := got["throughput"].(map[string]any)
	if throughput["tokens_per_second"] != 42.5 || throughput["window_seconds"] != float64(60) || throughput["tokens"] != float64(2550) {
		t.Fatalf("throughput = %#v", throughput)
	}

	lifetime := got["lifetime_totals"].(map[string]any)
	if lifetime["available"] != true || lifetime["total_tokens"] != float64(1500) || lifetime["sessions"] != float64(6) || lifetime["runs"] != float64(2) {
		t.Fatalf("lifetime_totals = %#v", lifetime)
	}

	trend := got["token_trend"].([]any)
	if len(trend) != 2 {
		t.Fatalf("token_trend len = %d, want 2", len(trend))
	}
	latest := trend[1].(map[string]any)
	if latest["input_tokens"] != float64(110) || latest["output_tokens"] != float64(220) || latest["total_tokens"] != float64(330) {
		t.Fatalf("token_trend[1] = %#v", latest)
	}
}

func TestBoardStateCountsAggregateSnapshotStates(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Pipeline: []telemetry.Issue{
			{ID: "review", State: "Human Review"},
			{ID: "done", State: "Done"},
			{ID: "cancelled", State: "Cancelled"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running", State: "In Progress"}},
			{Issue: telemetry.Issue{ID: "merging", State: "Merging"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "todo", State: "Todo"}},
			{Issue: telemetry.Issue{ID: "rework", State: "Rework"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "blocked", State: "Blocked"}},
		},
	}

	got := telemetry.BoardStateCounts(snapshot)
	want := []telemetry.BoardStateCount{
		{State: "Todo", Count: 1},
		{State: "In Progress", Count: 1},
		{State: "Review", Count: 1},
		{State: "Merging", Count: 1},
		{State: "Done", Count: 2},
		{State: "Rework", Count: 1},
		{State: "Blocked", Count: 1},
	}

	if len(got) != len(want) {
		t.Fatalf("BoardStateCounts() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BoardStateCounts()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}

func TestBoardStateCountsIncludeAggregateDetailDelta(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Counts: telemetry.Counts{
			Running:   3,
			Queue:     2,
			Blocked:   2,
			Completed: 3,
		},
		Pipeline: []telemetry.Issue{
			{ID: "review", State: "Human Review"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "merging", State: "Merging"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "todo", State: "Todo"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "blocked", State: "Blocked"}},
		},
	}

	got := telemetry.BoardStateCounts(snapshot)
	want := []telemetry.BoardStateCount{
		{State: "Todo", Count: 2},
		{State: "In Progress", Count: 2},
		{State: "Review", Count: 1},
		{State: "Merging", Count: 1},
		{State: "Blocked", Count: 2},
	}

	if len(got) != len(want) {
		t.Fatalf("BoardStateCounts() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BoardStateCounts()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}

func TestBoardStateCountsIgnoreCompletedSessionHistory(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Counts: telemetry.Counts{Blocked: 1, Completed: 1},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "issue-396",
					Identifier: "digitaldrywood/detent#396",
					State:      "Blocked",
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-396",
					Identifier: "digitaldrywood/detent#396",
					State:      "Done",
				},
				FinalState: "completed",
			},
		},
	}

	got := telemetry.BoardStateCounts(snapshot)
	want := []telemetry.BoardStateCount{{State: "Blocked", Count: 1}}

	if len(got) != len(want) {
		t.Fatalf("BoardStateCounts() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BoardStateCounts()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}

func TestBoardStateCountsScopeIssueKeysByProject(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:        "issue-1",
					ProjectID: "detent",
					State:     "In Progress",
				},
			},
			{
				Issue: telemetry.Issue{
					ID:        "issue-1",
					ProjectID: "pyroapex",
					State:     "Merging",
				},
			},
		},
	}

	got := telemetry.BoardStateCounts(snapshot)
	want := []telemetry.BoardStateCount{
		{State: "In Progress", Count: 1},
		{State: "Merging", Count: 1},
	}

	if len(got) != len(want) {
		t.Fatalf("BoardStateCounts() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BoardStateCounts()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}

func TestBoardProgressPointsSortCompletedSessions(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "later"}, CompletedAt: base.Add(2 * time.Minute), FinalState: "Done"},
			{Issue: telemetry.Issue{ID: "earlier"}, CompletedAt: base, FinalState: "Human Review"},
		},
	}

	got := telemetry.BoardProgressPoints(snapshot)
	want := []telemetry.BoardProgressPoint{
		{At: base, Label: "15:00", Count: 1},
		{At: base.Add(2 * time.Minute), Label: "15:02", Count: 2},
	}

	if len(got) != len(want) {
		t.Fatalf("BoardProgressPoints() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].Label != want[i].Label || got[i].Count != want[i].Count {
			t.Fatalf("BoardProgressPoints()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}

func TestBoardProgressPointsOffsetAggregateCompletedCount(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		Counts: telemetry.Counts{Completed: 5},
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "first"}, CompletedAt: base},
			{Issue: telemetry.Issue{ID: "second"}, CompletedAt: base.Add(time.Minute)},
		},
	}

	got := telemetry.BoardProgressPoints(snapshot)
	want := []telemetry.BoardProgressPoint{
		{At: base, Label: "15:00", Count: 4},
		{At: base.Add(time.Minute), Label: "15:01", Count: 5},
	}

	if len(got) != len(want) {
		t.Fatalf("BoardProgressPoints() len = %d, want %d; got %#v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].Label != want[i].Label || got[i].Count != want[i].Count {
			t.Fatalf("BoardProgressPoints()[%d] = %#v, want %#v; got %#v", i, got[i], want[i], got)
		}
	}
}
