package telemetry_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
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
		Seq:         42,
		GeneratedAt: generatedAt,
		Project: telemetry.Project{
			DisplayName: "Detent",
			URL:         "https://github.com/digitaldrywood/detent",
			Pool:        "code",
		},
		AgentPools: []telemetry.AgentPool{
			{Name: "code", Used: 5, Capacity: 5, Guaranteed: 5, BurstTo: 5, Generation: 2},
			{Name: "video", Used: 12, Capacity: 15, Guaranteed: 10, BurstTo: 15, Borrowed: 2, Available: 3, Generation: 3},
		},
		Instance: telemetry.Instance{
			Name:                    "release-captain",
			GitHubLogin:             "detent-bot",
			AuthorizationScope:      "assignee in @me (detent-bot, release-captain)",
			AuthorizationConfigured: true,
		},
		DashboardURL: "http://localhost:4101",
		Auth: telemetry.AuthHealth{
			Status:    telemetry.AuthStatusRecovered,
			LastError: "github authentication failed: status 401",
		},
		Refresh: telemetry.Refresh{
			PollIntervalSeconds: 30,
			DataSeq:             7,
			NextRefreshAt:       new(generatedAt.Add(30 * time.Second)),
		},
		Counts: telemetry.Counts{
			Running:   1,
			Queue:     2,
			Blocked:   3,
			Completed: 4,
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "issue-board",
				Identifier: "DD-BOARD",
				State:      "Backlog",
				Title:      "Board issue",
			},
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-1",
					Identifier: "DD-1",
					State:      "In Progress",
					Title:      "Port hub",
					URL:        "https://example.com/issues/1",
					Labels:     []string{"enhancement"},
					Assignees:  []string{"release-captain"},
					BlockedBy:  []telemetry.BlockedRef{{Identifier: "DD-0", State: "Done"}},
					RuntimeIdentity: agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", startedAt).
						Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", generatedAt)),
				},
				DetentSessionID: 42,
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
				Error:  "dependency #2 is not merged",
				Source: telemetry.BlockedSourceDependency,
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
		"seq",
		"generated_at",
		"project",
		"agent_pools",
		"instance",
		"dashboard_url",
		"auth",
		"refresh",
		"counts",
		"board_issues",
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
	if project["display_name"] != "Detent" || project["url"] != "https://github.com/digitaldrywood/detent" || project["pool"] != "code" {
		t.Fatalf("project = %#v", project)
	}
	pools := got["agent_pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("agent_pools len = %d, want 2", len(pools))
	}
	codePool := pools[0].(map[string]any)
	if codePool["name"] != "code" || codePool["used"] != float64(5) ||
		codePool["capacity"] != float64(5) || codePool["guaranteed"] != float64(5) ||
		codePool["burst_to"] != float64(5) || codePool["borrowed"] != float64(0) ||
		codePool["generation"] != float64(2) {
		t.Fatalf("agent_pools[0] = %#v", codePool)
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
	if got["seq"] != float64(42) {
		t.Fatalf("seq = %#v", got["seq"])
	}
	auth := got["auth"].(map[string]any)
	if auth["status"] != string(telemetry.AuthStatusRecovered) || auth["last_error"] != "github authentication failed: status 401" {
		t.Fatalf("auth = %#v", auth)
	}
	refresh := got["refresh"].(map[string]any)
	if refresh["poll_interval_seconds"] != float64(30) || refresh["next_refresh_at"] != "2026-05-30T22:15:30Z" {
		t.Fatalf("refresh = %#v", refresh)
	}
	if refresh["data_seq"] != float64(7) {
		t.Fatalf("refresh.data_seq = %#v", refresh["data_seq"])
	}

	counts := got["counts"].(map[string]any)
	if counts["running"] != float64(1) || counts["queue"] != float64(2) || counts["blocked"] != float64(3) || counts["completed"] != float64(4) {
		t.Fatalf("counts = %#v", counts)
	}

	running := got["running"].([]any)[0].(map[string]any)
	if running["issue_id"] != "issue-1" || running["identifier"] != "DD-1" {
		t.Fatalf("running row = %#v", running)
	}
	assignees := running["assignees"].([]any)
	if len(assignees) != 1 || assignees[0] != "release-captain" {
		t.Fatalf("running assignees = %#v", assignees)
	}
	blockers := running["blocked_by"].([]any)
	if len(blockers) != 1 || blockers[0].(map[string]any)["identifier"] != "DD-0" || blockers[0].(map[string]any)["state"] != "Done" {
		t.Fatalf("running blockers = %#v", blockers)
	}
	if running["process_identity"] != "4242" {
		t.Fatalf("running process identity = %#v", running)
	}
	if running["detent_session_id"] != float64(42) {
		t.Fatalf("running Detent session identity = %#v", running)
	}
	runtimeIdentity := running["runtime_identity"].(map[string]any)
	if runtimeIdentity["backend_kind"] != "codex" || runtimeIdentity["backend_id"] != "codex-high" {
		t.Fatalf("runtime identity route = %#v", runtimeIdentity)
	}
	if runtimeIdentity["provider"].(map[string]any)["value"] != "openai" || runtimeIdentity["resolved_model"].(map[string]any)["value"] != "gpt-5.6-sol" {
		t.Fatalf("runtime identity observed values = %#v", runtimeIdentity)
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

	blocked := got["blocked"].([]any)[0].(map[string]any)
	if blocked["source"] != string(telemetry.BlockedSourceDependency) {
		t.Fatalf("blocked source = %#v", blocked["source"])
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

func TestSnapshotEffectiveCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     telemetry.Counts
	}{
		{
			name: "uses aggregate counts except current blocked rows",
			snapshot: telemetry.Snapshot{
				Counts:    telemetry.Counts{Running: 7, Queue: 6, Blocked: 5, Completed: 4},
				Running:   []telemetry.Running{{}},
				Queue:     []telemetry.Queued{{}},
				Blocked:   []telemetry.Blocked{{}},
				Completed: []telemetry.Completed{{}},
			},
			want: telemetry.Counts{Running: 7, Queue: 6, Blocked: 1, Completed: 4},
		},
		{
			name: "falls back to row lengths",
			snapshot: telemetry.Snapshot{
				Running:   []telemetry.Running{{}, {}},
				Queue:     []telemetry.Queued{{}},
				Blocked:   []telemetry.Blocked{{}, {}, {}},
				Completed: []telemetry.Completed{{}, {}, {}, {}},
			},
			want: telemetry.Counts{Running: 2, Queue: 1, Blocked: 3, Completed: 4},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.snapshot.EffectiveCounts(); got != test.want {
				t.Fatalf("EffectiveCounts() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSnapshotOmitsUnavailableRuntimeIdentityFields(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(telemetry.Snapshot{
		Running:      []telemetry.Running{{Issue: telemetry.Issue{ID: "issue-1"}}},
		WorkAttempts: []telemetry.WorkAttempt{{AttemptID: 1}},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for name, entry := range map[string]map[string]any{
		"running":      decoded["running"].([]any)[0].(map[string]any),
		"work_attempt": decoded["work_attempts"].([]any)[0].(map[string]any),
	} {
		if _, ok := entry["runtime_identity"]; ok {
			t.Fatalf("%s runtime_identity = %#v, want omitted", name, entry["runtime_identity"])
		}
		if _, ok := entry["detent_session_id"]; ok {
			t.Fatalf("%s detent_session_id = %#v, want omitted", name, entry["detent_session_id"])
		}
	}
}

func TestTokensJSONUsesLastCallForContextPressureWhenWindowKnown(t *testing.T) {
	t.Parallel()

	contextWindow := int64(200)
	tokens := telemetry.Tokens{
		Input:              100,
		CachedInput:        25,
		Output:             40,
		Total:              170,
		Last:               &telemetry.TokenBreakdown{Input: 75, CachedInput: 20, Output: 15, Total: 90},
		ModelContextWindow: &contextWindow,
	}

	data, err := json.Marshal(tokens)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	pressure := got["context_pressure"].(map[string]any)
	if pressure["total_tokens"] != float64(90) || pressure["context_limit_tokens"] != float64(200) {
		t.Fatalf("context_pressure token fields = %#v", pressure)
	}
	if pressure["percent_used"] != 45.0 || pressure["threshold_state"] != string(telemetry.ContextPressureNormal) {
		t.Fatalf("context_pressure threshold fields = %#v", pressure)
	}
	last := got["last"].(map[string]any)
	if last["total_tokens"] != float64(90) || last["cached_input_tokens"] != float64(20) {
		t.Fatalf("last token fields = %#v", last)
	}
	if got["cache_read_fraction"] != 0.25 {
		t.Fatalf("cache_read_fraction = %#v, want 0.25", got["cache_read_fraction"])
	}

	var restored telemetry.Tokens
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal(Tokens) error = %v", err)
	}
	if restored.Last == nil || restored.Last.Total != 90 || restored.Last.CachedInput != 20 {
		t.Fatalf("restored Last = %#v, want last-call token fields", restored.Last)
	}
	restoredPressure, ok := restored.ContextPressure()
	if !ok || restoredPressure.TotalTokens != 90 || restoredPressure.PercentUsed != 45 {
		t.Fatalf("restored ContextPressure() = %#v, %t; want last-call pressure", restoredPressure, ok)
	}
}

func TestTokensJSONOmitsContextPressureWhenWindowUnknown(t *testing.T) {
	t.Parallel()

	tokens := telemetry.Tokens{
		Input:       100,
		CachedInput: 25,
		Output:      40,
		Total:       170,
	}

	data, err := json.Marshal(tokens)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := got["context_pressure"]; ok {
		t.Fatalf("context_pressure present for unknown window: %s", string(data))
	}
	if _, ok := got["model_context_window"]; ok {
		t.Fatalf("model_context_window present for unknown window: %s", string(data))
	}
	if got["cache_read_fraction"] != 0.25 {
		t.Fatalf("cache_read_fraction = %#v, want 0.25", got["cache_read_fraction"])
	}
}

func TestLifetimeTotalsResumedCacheReadFraction(t *testing.T) {
	t.Parallel()

	totals := telemetry.LifetimeTotals{
		ResumedInputTokens:  200,
		ResumedCachedTokens: 150,
	}
	fraction, ok := totals.ResumedCacheReadFraction()
	if !ok || fraction != 0.75 {
		t.Fatalf("ResumedCacheReadFraction() = %v, %v, want 0.75, true", fraction, ok)
	}

	totals.ResumedCachedTokens = 300
	fraction, ok = totals.ResumedCacheReadFraction()
	if !ok || fraction != 1 {
		t.Fatalf("ResumedCacheReadFraction() capped = %v, %v, want 1, true", fraction, ok)
	}
}

func TestContextPressureStateForPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		percent float64
		want    telemetry.ContextPressureState
	}{
		{name: "normal below watch", percent: 69.9, want: telemetry.ContextPressureNormal},
		{name: "watch at seventy", percent: 70, want: telemetry.ContextPressureWatch},
		{name: "warning at eighty five", percent: 85, want: telemetry.ContextPressureWarning},
		{name: "critical at ninety five", percent: 95, want: telemetry.ContextPressureCritical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := telemetry.ContextPressureStateForPercent(tt.percent); got != tt.want {
				t.Fatalf("ContextPressureStateForPercent(%v) = %q, want %q", tt.percent, got, tt.want)
			}
		})
	}
}

func TestProjectSnapshotJSONIncludesAuthHealth(t *testing.T) {
	t.Parallel()

	failedAt := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
	recoveredAt := failedAt.Add(30 * time.Second)
	project := telemetry.ProjectSnapshot{
		Project: telemetry.Project{ID: "detent"},
		Auth: telemetry.AuthHealth{
			Status:          telemetry.AuthStatusRecovered,
			LastError:       "github authentication failed: status 401",
			LastErrorAt:     &failedAt,
			LastRecoveredAt: &recoveredAt,
		},
	}

	data, err := json.Marshal(project)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	auth := got["auth"].(map[string]any)
	if auth["status"] != string(telemetry.AuthStatusRecovered) {
		t.Fatalf("auth.status = %#v", auth["status"])
	}
	if auth["last_error"] != "github authentication failed: status 401" {
		t.Fatalf("auth.last_error = %#v", auth["last_error"])
	}
	if auth["last_error_at"] != "2026-06-23T14:00:00Z" {
		t.Fatalf("auth.last_error_at = %#v", auth["last_error_at"])
	}
	if auth["last_recovered_at"] != "2026-06-23T14:00:30Z" {
		t.Fatalf("auth.last_recovered_at = %#v", auth["last_recovered_at"])
	}
}

func TestSnapshotJSONZeroValueSemantics(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	tests := []struct {
		name              string
		snapshot          telemetry.Snapshot
		wantBudgetPresent []string
		wantBudgetMissing []string
	}{
		{
			name:              "zero budget period omitted",
			snapshot:          telemetry.Snapshot{},
			wantBudgetMissing: []string{"period_start", "period_end"},
		},
		{
			name: "configured budget period emitted",
			snapshot: telemetry.Snapshot{
				Budget: telemetry.Budget{
					Enabled:     true,
					PeriodStart: start,
					PeriodEnd:   end,
				},
			},
			wantBudgetPresent: []string{"period_start", "period_end"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.snapshot)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}

			for _, key := range []string{"shutdown", "throughput", "cycle_time"} {
				if _, ok := got[key]; !ok {
					t.Fatalf("snapshot JSON missing %q: %s", key, string(data))
				}
			}

			budget := got["budget"].(map[string]any)
			for _, key := range tt.wantBudgetPresent {
				if _, ok := budget[key]; !ok {
					t.Fatalf("budget JSON missing %q: %s", key, string(data))
				}
			}
			for _, key := range tt.wantBudgetMissing {
				if _, ok := budget[key]; ok {
					t.Fatalf("budget JSON includes %q: %s", key, string(data))
				}
			}
		})
	}
}

func TestRefreshReadinessStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value telemetry.Refresh
		want  telemetry.RefreshStatus
		ready bool
	}{
		{
			name: "explicit initializing",
			value: telemetry.Refresh{
				Status: telemetry.RefreshStatusInitializing,
			},
			want: telemetry.RefreshStatusInitializing,
		},
		{
			name: "explicit ready",
			value: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &now,
			},
			want:  telemetry.RefreshStatusReady,
			ready: true,
		},
		{
			name: "explicit degraded",
			value: telemetry.Refresh{
				Status:    telemetry.RefreshStatusDegraded,
				LastError: "tracker unavailable",
			},
			want: telemetry.RefreshStatusDegraded,
		},
		{
			name: "legacy loaded snapshot infers ready",
			value: telemetry.Refresh{
				LastRefreshAt: &now,
			},
			want:  telemetry.RefreshStatusReady,
			ready: true,
		},
		{
			name:  "legacy zero refresh remains initializing",
			value: telemetry.Refresh{},
			want:  telemetry.RefreshStatusInitializing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.value.ReadinessStatus(); got != tt.want {
				t.Fatalf("ReadinessStatus() = %q, want %q", got, tt.want)
			}
			if got := tt.value.Ready(); got != tt.ready {
				t.Fatalf("Ready() = %v, want %v", got, tt.ready)
			}
		})
	}
}

func TestRefreshReadinessStatusAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 1, 39, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-18 * time.Minute)
	tests := []struct {
		name          string
		nextRefreshAt time.Time
		wantStatus    telemetry.RefreshStatus
		wantOverdue   bool
	}{
		{
			name:          "healthy refresh remains ready",
			nextRefreshAt: now.Add(time.Minute),
			wantStatus:    telemetry.RefreshStatusReady,
		},
		{
			name:          "past due refresh becomes degraded",
			nextRefreshAt: now.Add(-9 * time.Minute),
			wantStatus:    telemetry.RefreshStatusDegraded,
			wantOverdue:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refresh := (telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
				NextRefreshAt: &tt.nextRefreshAt,
			}).WithFreshness(now)
			if got := refresh.ReadinessStatus(); got != tt.wantStatus {
				t.Fatalf("ReadinessStatus() = %q, want %q", got, tt.wantStatus)
			}
			if refresh.NextRefreshOverdue != tt.wantOverdue {
				t.Fatalf("NextRefreshOverdue = %v, want %v", refresh.NextRefreshOverdue, tt.wantOverdue)
			}
		})
	}
}

func TestSnapshotWithFreshness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 1, 39, 0, 0, time.UTC)
	generatedAt := now.Add(-18 * time.Minute)
	nextRefreshAt := now.Add(-9 * time.Minute)
	snapshot := (telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Refresh: telemetry.Refresh{
			Status:        telemetry.RefreshStatusReady,
			LastRefreshAt: &generatedAt,
			NextRefreshAt: &nextRefreshAt,
		},
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent"},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &generatedAt,
				NextRefreshAt: &nextRefreshAt,
			},
		}},
	}).WithFreshness(now)

	if got := snapshot.AgeSeconds(now); got != int64((18*time.Minute)/time.Second) {
		t.Fatalf("AgeSeconds() = %d, want 1080", got)
	}
	if snapshot.Refresh.ReadinessStatus() != telemetry.RefreshStatusDegraded || !snapshot.Refresh.NextRefreshOverdue {
		t.Fatalf("Refresh = %#v, want overdue degraded refresh", snapshot.Refresh)
	}
	if snapshot.Projects[0].Refresh.ReadinessStatus() != telemetry.RefreshStatusDegraded || !snapshot.Projects[0].Refresh.NextRefreshOverdue {
		t.Fatalf("Projects[0].Refresh = %#v, want overdue degraded refresh", snapshot.Projects[0].Refresh)
	}
}

func TestRefreshReadinessJSON(t *testing.T) {
	t.Parallel()

	lastErrorAt := time.Date(2026, 6, 22, 9, 30, 0, 0, time.UTC)
	data, err := json.Marshal(telemetry.Refresh{
		Status:      telemetry.RefreshStatusDegraded,
		LastError:   "tracker unavailable",
		LastErrorAt: &lastErrorAt,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got["status"] != string(telemetry.RefreshStatusDegraded) {
		t.Fatalf("status = %#v, want degraded in %s", got["status"], string(data))
	}
	if got["last_error"] != "tracker unavailable" {
		t.Fatalf("last_error = %#v, want tracker unavailable in %s", got["last_error"], string(data))
	}
	if got["last_error_at"] != "2026-06-22T09:30:00Z" {
		t.Fatalf("last_error_at = %#v, want timestamp in %s", got["last_error_at"], string(data))
	}
}

func TestProjectSnapshotJSONKeepsZeroThroughput(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(telemetry.ProjectSnapshot{})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if _, ok := got["throughput"]; !ok {
		t.Fatalf("project snapshot JSON missing throughput: %s", string(data))
	}
}

func TestBoardStateCountsAggregateSnapshotStates(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "backlog", State: "Backlog"},
			{ID: "todo", State: "Backlog"},
		},
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
		{State: "Backlog", Count: 1},
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

func TestBoardStateCountsKeepDependencyWaitsInTrackerLane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		waiting telemetry.Blocked
	}{
		{
			name: "dependency source",
			waiting: telemetry.Blocked{
				Issue:  telemetry.Issue{ID: "waiting", State: "Todo"},
				Source: telemetry.BlockedSourceDependency,
			},
		},
		{
			name: "dependency reference",
			waiting: telemetry.Blocked{
				Issue: telemetry.Issue{
					ID:        "waiting",
					State:     "Todo",
					BlockedBy: []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#512", State: "In Progress"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshot := telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{
					{ID: "ready", State: "Todo"},
					tt.waiting.Issue,
					{ID: "blocked", State: "Todo"},
				},
				Blocked: []telemetry.Blocked{
					tt.waiting,
					{Issue: telemetry.Issue{ID: "blocked", State: "Todo"}},
				},
			}

			got := telemetry.BoardStateCounts(snapshot)
			want := []telemetry.BoardStateCount{
				{State: "Todo", Count: 2},
				{State: "Blocked", Count: 1},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("BoardStateCounts() = %#v, want %#v", got, want)
			}

			workload := telemetry.BoardWorkload(snapshot)
			if workload.Todo+workload.Waiting != want[0].Count || workload.Blocked != want[1].Count {
				t.Fatalf("workload = %+v, lane counts = %#v", workload, want)
			}
		})
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
