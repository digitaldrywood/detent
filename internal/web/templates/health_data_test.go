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
			if view.Checked != "16:42:07" {
				t.Fatalf("checked = %q", view.Checked)
			}
		})
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
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	rest := rows[0]
	if rest.Quota != "4,178 / 5,000" || rest.QuotaPct != 83 || rest.QuotaWarn {
		t.Fatalf("rest quota = %q pct=%d warn=%v", rest.Quota, rest.QuotaPct, rest.QuotaWarn)
	}
	if rest.Resets != "17:00" {
		t.Fatalf("rest resets = %q", rest.Resets)
	}
	graphql := rows[1]
	if graphql.Kind != primitives.KindWarn || graphql.Status != "Backoff" || !graphql.QuotaWarn {
		t.Fatalf("graphql row = %+v", graphql)
	}
	scheduler := rows[2]
	if scheduler.Status != "Running" || !strings.Contains(scheduler.Detail, "2 active sessions") {
		t.Fatalf("scheduler row = %+v", scheduler)
	}
	backoff := rows[3]
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
	if len(rows) != 2 {
		t.Fatalf("expected scheduler and backoff rows only, got %d", len(rows))
	}
	if rows[0].Status != "Idle" || rows[1].Status != "None" {
		t.Fatalf("idle rows = %+v", rows)
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
