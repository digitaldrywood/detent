package orchestrator

import (
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRateWindowPacingSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	freshAt := now.Add(-time.Minute)
	staleAt := now.Add(-16 * time.Minute)
	tests := []struct {
		name             string
		pacing           workflowconfig.RateWindowPacing
		limits           *telemetry.RateLimits
		wantStatus       string
		wantCeiling      int
		wantApplicable   bool
		wantScaling      bool
		wantRemaining    float64
		wantHasRemaining bool
	}{
		{name: "missing fails open", wantStatus: telemetry.RateWindowBucketMissing, wantCeiling: 10, wantApplicable: true},
		{name: "stale fails open", limits: pacingRateLimits(10, &staleAt), wantStatus: telemetry.RateWindowBucketStale, wantCeiling: 10, wantApplicable: true, wantRemaining: 10, wantHasRemaining: true},
		{name: "proportional scales fresh", limits: pacingRateLimits(10, &freshAt), wantStatus: telemetry.RateWindowBucketFresh, wantCeiling: 1, wantApplicable: true, wantScaling: true, wantRemaining: 10, wantHasRemaining: true},
		{name: "off reports but does not scale", pacing: workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}, limits: pacingRateLimits(10, &freshAt), wantStatus: telemetry.RateWindowBucketFresh, wantCeiling: 10, wantRemaining: 10, wantHasRemaining: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				BillingMode:         workflowconfig.BillingModeSubscription,
				RateWindowPacing:    tt.pacing,
				MaxConcurrentAgents: 10,
			})
			state := newState(cfg)
			state.RateLimits = tt.limits
			got := state.Snapshot(now).Dispatch.RateWindowPacing
			if got.BucketStatus != tt.wantStatus || got.PermitCeiling != tt.wantCeiling || got.Applicable != tt.wantApplicable || got.ScalingApplied != tt.wantScaling {
				t.Fatalf("RateWindowPacing = %#v", got)
			}
			if (got.ObservedRemainingPercent != nil) != tt.wantHasRemaining {
				t.Fatalf("ObservedRemainingPercent = %v, want presence %t", got.ObservedRemainingPercent, tt.wantHasRemaining)
			}
			if tt.wantHasRemaining && *got.ObservedRemainingPercent != tt.wantRemaining {
				t.Fatalf("ObservedRemainingPercent = %v, want %.0f", *got.ObservedRemainingPercent, tt.wantRemaining)
			}
		})
	}
}

func pacingRateLimits(remaining int64, observedAt *time.Time) *telemetry.RateLimits {
	return &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{Remaining: remaining, Limit: 100, ObservedAt: observedAt}}
}
