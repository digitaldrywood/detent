package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestLifetimeCardStale(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "stale"}[stale], func(t *testing.T) {
			var out bytes.Buffer
			err := lifetimeCard(telemetry.LifetimeTotals{Available: true, Stale: stale, ReadFailures: 3, TotalTokens: 1234}).Render(t.Context(), &out)
			if err != nil {
				t.Fatal(err)
			}
			html := out.String()
			for _, marker := range []string{">stale<", "3 read failures"} {
				if strings.Contains(html, marker) != stale {
					t.Errorf("marker %q stale=%v: %s", marker, stale, html)
				}
			}
			if !strings.Contains(html, "1,234") {
				t.Error("totals missing")
			}
		})
	}
}
