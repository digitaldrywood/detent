package telemetry_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
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
				SessionID:      "thread-1",
				TurnCount:      2,
				StartedAt:      startedAt,
				RuntimeSeconds: 300,
				DiffAdded:      4,
				DiffRemoved:    2,
				DiffFiles:      3,
				DiffStatus:     "ok",
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

	counts := got["counts"].(map[string]any)
	if counts["running"] != float64(1) || counts["queue"] != float64(2) || counts["blocked"] != float64(3) || counts["completed"] != float64(4) {
		t.Fatalf("counts = %#v", counts)
	}

	running := got["running"].([]any)[0].(map[string]any)
	if running["issue_id"] != "issue-1" || running["identifier"] != "DD-1" {
		t.Fatalf("running row = %#v", running)
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
