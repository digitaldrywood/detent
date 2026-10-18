package web

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestLifetimeTotalsResponseStaleness(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "fresh"
		if stale {
			name = "stale"
		}
		t.Run(name, func(t *testing.T) {
			got := lifetimeTotalsResponseFromTelemetry(telemetry.LifetimeTotals{Available: true, Stale: stale, ReadFailures: 3, TotalTokens: 42})
			if !got.Available || got.Stale != stale || got.ReadFailures != 3 || got.TotalTokens != 42 {
				t.Fatalf("response = %+v", got)
			}
		})
	}
}
