package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestHealthViewVerdicts(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 42, 7, 0, time.UTC)
	tests := []struct {
		name        string
		snapshot    telemetry.Snapshot
		wantKind    primitives.Kind
		wantVerdict string
	}{
		{
			name: "healthy quota reads nominal",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				RateLimits: &telemetry.RateLimits{
					GitHubREST: &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000},
				},
			},
			wantKind:    primitives.KindOK,
			wantVerdict: "All systems nominal.",
		},
		{
			name:        "no snapshot stays neutral",
			snapshot:    telemetry.Snapshot{GeneratedAt: now},
			wantKind:    primitives.KindNeutral,
			wantVerdict: "Waiting for the first health snapshot.",
		},
		{
			name: "backend capacity outage warns",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				BackendOutages: []telemetry.BackendOutage{{
					BackendID: "codex",
					Provider:  "openai",
					ResumeAt:  now.Add(44 * time.Minute),
				}},
			},
			wantKind:    primitives.KindWarn,
			wantVerdict: "Backend codex at usage limit.",
		},
		{
			name: "failure breaker warns",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				FailureBreakers: []telemetry.FailureBreaker{{
					ProjectID: "detent",
				}},
			},
			wantKind:    primitives.KindWarn,
			wantVerdict: "Project dispatch paused by correlated failures — 1 project.",
		},
		{
			name: "waiting recovery warns",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				DispatchRecoveries: []telemetry.DispatchRecovery{{
					ProjectID: "detent",
					Kind:      "github_rest",
					Status:    "waiting",
				}},
			},
			wantKind:    primitives.KindWarn,
			wantVerdict: "Dispatch is waiting on capacity.",
		},
		{
			name: "stranded active issue warns",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				StrandedActiveIssues: []telemetry.StrandedIssue{{
					ProjectID: "detent", Identifier: "digitaldrywood/detent#1606", DurationSeconds: 900,
				}},
			},
			wantKind:    primitives.KindWarn,
			wantVerdict: "Active work has no live worker.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := healthViewFromDashboard(DashboardData{Snapshot: tt.snapshot})
			if view.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", view.Kind, tt.wantKind)
			}
			if view.Verdict != tt.wantVerdict {
				t.Fatalf("verdict = %q, want %q", view.Verdict, tt.wantVerdict)
			}
			if !view.CheckedAt.Equal(now) {
				t.Fatalf("checked at = %s", view.CheckedAt)
			}
		})
	}
}

func TestHealthStrandedActiveRowsGroupByProject(t *testing.T) {
	t.Parallel()

	rows := healthStrandedActiveRows([]telemetry.StrandedIssue{
		{ProjectID: "pyroapex", Identifier: "digitaldrywood/pyroapex#10", DurationSeconds: 1200},
		{ProjectID: "detent", Identifier: "digitaldrywood/detent#1607", DurationSeconds: 720, LastRefusalReason: "budget cooldown"},
		{ProjectID: "detent", Identifier: "digitaldrywood/detent#1606", DurationSeconds: 900, LastRefusalReason: "priority reservation"},
	})

	if len(rows) != 2 {
		t.Fatalf("healthStrandedActiveRows() = %#v, want two project rows", rows)
	}
	if rows[0].Component != "Active work · detent" || rows[0].Status != "No live worker" {
		t.Fatalf("detent row = %#v", rows[0])
	}
	for _, want := range []string{"digitaldrywood/detent#1606", "15m", "priority reservation", "digitaldrywood/detent#1607", "12m", "budget cooldown"} {
		if !strings.Contains(rows[0].Detail, want) {
			t.Fatalf("detent detail = %q, want %q", rows[0].Detail, want)
		}
	}
	if rows[1].Component != "Active work · pyroapex" || !strings.Contains(rows[1].Detail, "none recorded") {
		t.Fatalf("pyroapex row = %#v", rows[1])
	}
}

func TestBackendCapacityProjectDetailDoesNotInflateVerdict(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resumeAt := now.Add(44 * time.Minute)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		BackendOutages: []telemetry.BackendOutage{
			{ProjectID: "detent", BackendID: "codex", Provider: "openai", ResumeAt: resumeAt},
			{ProjectID: "docs", BackendID: "codex", Provider: "openai", ResumeAt: resumeAt},
		},
	}
	view := healthViewFromDashboard(DashboardData{Snapshot: snapshot})
	if view.Verdict != "Backend codex at usage limit." {
		t.Fatalf("verdict = %q, want one backend outage", view.Verdict)
	}
	summaries := boardBackendCapacitySummaries(snapshot.BackendOutages, time.Time{})
	if len(summaries) != 1 || summaries[0].Title != "Backend codex at usage limit — 2 projects" {
		t.Fatalf("Board summaries = %#v", summaries)
	}
	html := renderBoardComponent(t, HealthSnapshotV2(DashboardData{Snapshot: snapshot}))
	for _, project := range []string{"Project detent", "Project docs"} {
		if !strings.Contains(html, project) {
			t.Fatalf("Health detail missing %q:\n%s", project, html)
		}
	}
}

func TestHealthRowsIncludeBackendCapacityOutage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	rows := healthRows(telemetry.Snapshot{
		GeneratedAt: now,
		BackendOutages: []telemetry.BackendOutage{{
			BackendID: "codex",
			Provider:  "openai",
			ResumeAt:  now.Add(44 * time.Minute),
		}},
	})
	row := rows[len(rows)-1]
	if row.Component != "Backend codex" || row.Status != "Usage limit" || !row.ResetAt.Equal(now.Add(44*time.Minute)) {
		t.Fatalf("backend outage row = %+v", row)
	}
}

func TestHealthRefreshRowsDegradeAtFailureThreshold(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Minute)
	lastErrorAt := now.Add(-10 * time.Second)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			StaleAfterSeconds: 120,
			FailureThreshold:  3,
			Sources: []telemetry.RefreshSource{{
				ProjectID:     "detent",
				Name:          telemetry.RefreshSourceCandidates,
				LastSuccessAt: &lastSuccess,
				FailureStreak: 2,
				LastError:     "status 503",
				LastErrorAt:   &lastErrorAt,
			}},
		},
	}

	rows := healthRefreshRows(snapshot)
	if len(rows) != 1 || rows[0].Kind != primitives.KindOK || rows[0].Status != "Current" {
		t.Fatalf("health row before threshold = %#v", rows)
	}
	if strings.Contains(rows[0].Detail, "status 503") {
		t.Fatalf("health row exposed transient error before threshold: %q", rows[0].Detail)
	}

	snapshot.Refresh.Sources[0].FailureStreak = 3
	rows = healthRefreshRows(snapshot)
	if len(rows) != 1 || rows[0].Kind != primitives.KindWarn || rows[0].Status != "Stale" {
		t.Fatalf("health row at threshold = %#v", rows)
	}
	if !strings.Contains(rows[0].Detail, "candidate fetch") || !strings.Contains(rows[0].Detail, "3 consecutive failures") || !strings.Contains(rows[0].Detail, "status 503") {
		t.Fatalf("health row missing failure detail: %q", rows[0].Detail)
	}
	view := healthViewFromDashboard(DashboardData{Snapshot: snapshot})
	if view.Kind != primitives.KindWarn || view.Verdict != "Tracker data is stale." {
		t.Fatalf("health verdict = (%q, %q)", view.Kind, view.Verdict)
	}
}

func TestFleetFreshnessPreservesSourcelessProjectFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Second)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			Status: telemetry.RefreshStatusDegraded,
			Sources: []telemetry.RefreshSource{
				{ProjectID: "detent", Name: telemetry.RefreshSourceCandidates, LastSuccessAt: &lastSuccess},
				{ProjectID: "docs", Name: telemetry.RefreshSourceProject, Degraded: true, LastError: "runtime unavailable"},
			},
		},
	}

	if got := refreshFreshnessKind(snapshot); got != primitives.KindWarn {
		t.Fatalf("refreshFreshnessKind() = %q, want %q", got, primitives.KindWarn)
	}
	rows := healthRefreshRows(snapshot)
	if len(rows) != 2 || rows[1].Component != "Tracker freshness · docs" || rows[1].Kind != primitives.KindWarn {
		t.Fatalf("health refresh rows = %#v", rows)
	}
	if !strings.Contains(rows[1].Detail, "project refresh") || !strings.Contains(rows[1].Detail, "runtime unavailable") {
		t.Fatalf("degraded project detail = %q", rows[1].Detail)
	}
}

func TestHealthBudgetRowsShowEffectiveCapAndOverride(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	cap := 200.0
	rows := healthBudgetRows(DashboardData{
		Snapshot: telemetry.Snapshot{GeneratedAt: now},
		Projects: []ProjectSmallMultiple{{
			ID:               "detent",
			Name:             "Detent",
			BudgetEnabled:    true,
			BudgetObservedAt: now,
			CurrentSpendUSD:  170,
			PerDayMaxUSD:     cap,
			BudgetResetAt:    now.Add(9 * time.Hour),
			BudgetOverride: &telemetry.BudgetOverride{
				ProjectID:    "detent",
				PerDayMaxUSD: &cap,
				ExpiresAt:    now.Add(4 * time.Hour),
				Reason:       "release work",
			},
		}},
	})
	if len(rows) != 1 {
		t.Fatalf("healthBudgetRows() len = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Kind != primitives.KindWarn || row.Status != "Approaching limit" || row.QuotaPct != 85 {
		t.Fatalf("budget row = %#v", row)
	}
	if !strings.Contains(row.Detail, "override daily $200.00") || !strings.Contains(row.Detail, "expires in 4h0m0s") || !strings.Contains(row.Detail, "release work") {
		t.Fatalf("budget detail = %q", row.Detail)
	}
}

func TestHealthRows(t *testing.T) {
	resetAt := time.Date(2026, 7, 4, 17, 0, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 42, 0, 0, time.UTC),
		Counts:      telemetry.Counts{Running: 2, Queue: 1},
		RateLimits: &telemetry.RateLimits{
			GitHubREST:    &telemetry.RateLimitBucket{Remaining: 822, Limit: 5000, ResetAt: &resetAt},
			GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 78, Limit: 5000, Status: telemetry.RateLimitStatusBackoff},
		},
	}

	rows := healthRows(snapshot)
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(rows))
	}
	rest := rows[0]
	if rest.Quota != "4,178 / 5,000" || rest.QuotaPct != 83 || rest.QuotaWarn {
		t.Fatalf("rest quota = %q pct=%d warn=%v", rest.Quota, rest.QuotaPct, rest.QuotaWarn)
	}
	if !rest.ResetAt.Equal(resetAt) {
		t.Fatalf("rest reset at = %s", rest.ResetAt)
	}
	graphql := rows[1]
	if graphql.Kind != primitives.KindWarn || graphql.Status != "Backoff" || !graphql.QuotaWarn {
		t.Fatalf("graphql row = %+v", graphql)
	}
	scheduler := rows[2]
	if scheduler.Status != "Running" || !strings.Contains(scheduler.Detail, "2 active sessions") {
		t.Fatalf("scheduler row = %+v", scheduler)
	}
	if update := rows[3]; update.Status != "Disabled" {
		t.Fatalf("update row = %+v", update)
	}
	backoff := rows[4]
	if backoff.Status != "Active" || !strings.Contains(backoff.Detail, "GraphQL") {
		t.Fatalf("backoff row = %+v", backoff)
	}
}

func TestHealthRowsExhaustedByRemainingCount(t *testing.T) {
	// Zero remaining with no explicit status must read Exhausted so the
	// details row matches the exhaustion verdict.
	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 42, 0, 0, time.UTC),
		RateLimits: &telemetry.RateLimits{
			GitHubREST: &telemetry.RateLimitBucket{Remaining: 0, Limit: 5000},
		},
	}
	rest := healthRows(snapshot)[0]
	if rest.Kind != primitives.KindErr || rest.Status != "Exhausted" {
		t.Fatalf("exhausted REST row = %+v", rest)
	}
}

func TestHealthRowsRESTUsageBackoff(t *testing.T) {
	// A secondary REST throttle lives in RESTUsage, not a bucket status; the
	// Backoff row must still surface it.
	backoffUntil := time.Date(2026, 7, 4, 16, 45, 0, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 42, 0, 0, time.UTC),
		RateLimits: &telemetry.RateLimits{
			GitHubREST: &telemetry.RateLimitBucket{Remaining: 4000, Limit: 5000},
			RESTUsage:  &telemetry.RESTUsage{RateLimited: true, BackoffUntil: &backoffUntil},
		},
	}
	rows := healthRows(snapshot)
	backoff := rows[len(rows)-1]
	if backoff.Status != "Active" || !strings.Contains(backoff.Detail, "REST") {
		t.Fatalf("REST usage backoff row = %+v", backoff)
	}
}

func TestHealthRowsIdleWithoutData(t *testing.T) {
	rows := healthRows(telemetry.Snapshot{})
	if len(rows) != 3 {
		t.Fatalf("expected scheduler, update, and backoff rows, got %d", len(rows))
	}
	if rows[0].Status != "Idle" || rows[1].Status != "Disabled" || rows[2].Status != "None" {
		t.Fatalf("idle rows = %+v", rows)
	}
}

func TestHealthUpdateRowShowsRuntimeStatus(t *testing.T) {
	t.Parallel()

	lastCheck := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	nextCheck := lastCheck.Add(12 * time.Hour)
	row := healthUpdateRow(telemetry.Update{
		Enabled:            true,
		AutoApplyEnabled:   true,
		CheckIntervalHours: 12,
		State:              "scheduled",
		LastCheckAt:        &lastCheck,
		LastAppliedVersion: "1.2.4",
		NextCheckAt:        &nextCheck,
	})
	if row.Kind != primitives.KindOK || row.Status != "Scheduled" {
		t.Fatalf("healthUpdateRow() = %+v", row)
	}
	if !strings.Contains(row.Detail, "last applied 1.2.4") || !row.DetailAt.Equal(lastCheck) || !row.ResetAt.Equal(nextCheck) {
		t.Fatalf("healthUpdateRow() status detail = %+v", row)
	}
}

func TestHealthExhaustedRowDetailNotHealthy(t *testing.T) {
	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		RateLimits: &telemetry.RateLimits{
			GitHubREST: &telemetry.RateLimitBucket{Remaining: 0, Limit: 5000},
			RESTUsage:  &telemetry.RESTUsage{TotalRequests: 4200},
		},
	}
	rest := healthRows(snapshot)[0]
	if rest.Status != "Exhausted" {
		t.Fatalf("expected Exhausted, got %q", rest.Status)
	}
	if rest.Detail == "Within budget" {
		t.Fatalf("exhausted row detail must not say Within budget: %q", rest.Detail)
	}
}
