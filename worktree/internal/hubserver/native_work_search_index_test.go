package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
)

func TestSearchRefreshRunsAtJitteredOffset(t *testing.T) {
	offsets := map[float64]time.Duration{}
	for _, jitter := range []float64{0.2, 0.7} {
		service := &Service{config: Config{WebhookMaintenanceInterval: time.Minute, deps: hubdeps.Deps{
			Jitter: func() float64 { return jitter },
			NewMaintenanceTimer: func(delay time.Duration) hubdeps.Timer {
				offsets[jitter] = delay
				return hubdeps.Timer{Reset: func(time.Duration) {}, Stop: func() {}}
			},
		}}}
		service.searchRefreshTimer()
	}
	if offsets[0.2] == offsets[0.7] || offsets[0.2] < 10*time.Second || offsets[0.7] < 40*time.Second || offsets[0.7] >= time.Minute {
		t.Fatalf("search refresh offsets=%v", offsets)
	}
}
