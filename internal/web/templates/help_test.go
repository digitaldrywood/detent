package templates

import (
	"strings"
	"testing"
)

func TestHelpTermsCoverDashboardSectionsAndMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		term helpTerm
		unit string
	}{
		{name: "running", term: helpRunning},
		{name: "queue", term: helpQueue},
		{name: "backoff queue", term: helpBackoffQueue},
		{name: "blocked", term: helpBlocked},
		{name: "completed", term: helpCompleted},
		{name: "budget", term: helpBudget, unit: "USD"},
		{name: "rate limits", term: helpRateLimits},
		{name: "tokens", term: helpTokens, unit: "tokens"},
		{name: "throughput", term: helpThroughput, unit: "tps"},
		{name: "runtime", term: helpRuntime},
		{name: "age turn", term: helpAgeTurn},
		{name: "session", term: helpSession},
		{name: "event", term: helpEvent},
		{name: "diff", term: helpDiff},
		{name: "projected spend", term: helpProjectedSpend, unit: "USD"},
		{name: "daily cap", term: helpDailyCap, unit: "USD"},
		{name: "issue cap", term: helpIssueCap, unit: "USD"},
		{name: "primary rate bucket", term: helpPrimaryRateBucket},
		{name: "secondary rate bucket", term: helpSecondaryRateBucket},
		{name: "credits rate bucket", term: helpCreditsRateBucket},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, ok := helpDefinitions[tt.term]
			if !ok {
				t.Fatalf("helpDefinitions[%q] missing", tt.term)
			}
			if entry.Label == "" {
				t.Fatalf("helpDefinitions[%q].Label is empty", tt.term)
			}
			if entry.Description == "" {
				t.Fatalf("helpDefinitions[%q].Description is empty", tt.term)
			}
			if tt.unit != "" && !containsFold(entry.Description, tt.unit) {
				t.Fatalf("helpDefinitions[%q].Description = %q, want unit %q", tt.term, entry.Description, tt.unit)
			}
		})
	}
}

func TestHelpIDIncludesScopeAndTerm(t *testing.T) {
	t.Parallel()

	if got := helpID(helpThroughput, "metric-card"); got != "help-metric-card-throughput" {
		t.Fatalf("helpID() = %q, want %q", got, "help-metric-card-throughput")
	}

	if got := helpID(helpRateLimits, "Codex Rate Limits"); got != "help-codex-rate-limits-rate-limits" {
		t.Fatalf("helpID() = %q, want %q", got, "help-codex-rate-limits-rate-limits")
	}
}

func containsFold(value string, substr string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substr))
}
