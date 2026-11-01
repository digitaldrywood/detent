package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerHealthLabel(t *testing.T) {
	now := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	for _, health := range []string{"online", "offline", "revoked", "expired", "needs_attention"} {
		t.Run(health, func(t *testing.T) {
			r := runnerauth.Runner{Health: health, Routing: runnerauth.Routing{Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}}}
			want := health
			switch health {
			case "online":
				want = "Outside hours"
			case "needs_attention":
				want = "Needs attention"
			}
			if got := runnerHealthLabel(r, now); got != want {
				t.Fatalf("label = %s, want %s", got, want)
			}
		})
	}
}
