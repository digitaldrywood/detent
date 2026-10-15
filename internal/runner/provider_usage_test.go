package runner

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProviderUsageRetainsWindowObservations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	usage := &ProviderUsage{}
	percent := 85.0
	for _, backend := range []string{"claude", "codex"} {
		usage.Observe(backend, &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", PercentUsed: &percent, ObservedAt: &now}})
	}
	old := now.Add(-time.Hour)
	usage.Observe("claude", &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", ObservedAt: &old}})
	usage.Observe("claude", &telemetry.RateLimits{Secondary: &telemetry.RateLimitBucket{WindowKind: "weekly", ObservedAt: &old}})
	percent = 10
	for _, backend := range []string{"claude", "codex"} {
		windows := usage.Windows(backend)
		if len(windows) == 0 || windows[0].PercentUsed == nil || *windows[0].PercentUsed != 85 || !windows[0].ObservedAt.Equal(now) {
			t.Fatalf("%s windows = %+v", backend, windows)
		}
		*windows[0].PercentUsed = 0
		cached := usage.Windows(backend)
		if len(cached) == 0 || cached[0].PercentUsed == nil || *cached[0].PercentUsed != 85 {
			t.Fatal("reader mutated cached usage")
		}
	}
	windows := usage.Windows("claude")
	if len(windows) != 2 || windows[1].PercentUsed != nil || !windows[1].ObservedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("stale unknown window was lost or refreshed: %+v", windows)
	}
	if now.Sub(windows[1].ObservedAt) <= providercapacity.MaxAge || len(usage.Windows("unknown")) != 0 {
		t.Fatal("observation age or backend isolation changed")
	}
}

func TestProviderBillingObservations(t *testing.T) {
	t.Parallel()
	usage := &ProviderUsage{}
	for _, test := range []struct{ backend, billing, want string }{
		{"claude", "subscription", "subscription"}, {"codex", "api_key", "api_key"},
		{"claude", "", "subscription"}, {"claude", "invalid", "subscription"}, {"codex", "unknown", "unknown"},
	} {
		usage.ObserveUpdate(test.backend, AgentUpdate{Billing: test.billing})
		if got := usage.Billing(test.backend); got != test.want {
			t.Fatalf("%s billing = %q, want %q", test.backend, got, test.want)
		}
	}
	if got := usage.Billing("claude"); got != "subscription" {
		t.Fatalf("other backend overwrote Claude billing: %q", got)
	}
}
