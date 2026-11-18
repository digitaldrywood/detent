package allocationpoll

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

func TestWakeContinuesPageAndTimerHonorsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		wake := make(chan struct{}, 1)
		start := time.Now()
		retry := start.Add(250 * time.Millisecond)
		turn := 0
		Run(ctx, wake, time.Now, func(after *platformstore.AllocationCandidate) (bool, time.Time) {
			turn++
			switch turn {
			case 1:
				after.ID = "last-deletion-in-page"
				wake <- struct{}{}
				return true, time.Time{}
			case 2:
				if after.ID != "last-deletion-in-page" || !time.Now().Equal(start) {
					t.Fatalf("wake resumed at %q after %s", after.ID, time.Since(start))
				}
				return false, retry
			default:
				if !time.Now().Equal(retry) {
					t.Errorf("next turn at %s, want %s", time.Now(), retry)
				}
				cancel()
				return false, time.Time{}
			}
		})
	})
}
