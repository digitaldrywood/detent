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
		ticker := time.NewTicker(BaseInterval)
		defer ticker.Stop()
		Run(ctx, wake, ticker, time.Now, func(after *platformstore.AllocationCandidate) (bool, time.Time) {
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
		}, nil)
	})
}

func TestAllocationWakeDuringAttachmentRetention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		ticker := time.NewTicker(BaseInterval)
		defer ticker.Stop()
		wake := make(chan struct{}, 1)
		done := make(chan struct{})
		release := make(chan struct{}, 1)
		clock := time.Now()
		turns := 0
		var pages []string
		go func() {
			defer close(done)
			Run(ctx, wake, ticker, func() time.Time { return clock }, func(*platformstore.AllocationCandidate) (bool, time.Time) {
				turns++
				return false, time.Time{}
			}, func(ctx context.Context, after string) string {
				pages = append(pages, after)
				<-release
				if after == "" {
					return "next-tenant"
				}
				return ""
			})
		}()
		defer func() { cancel(); close(release); <-done }()
		synctest.Wait()
		wake <- struct{}{}
		synctest.Wait()
		if turns != 2 || len(pages) != 1 {
			t.Fatalf("while the first retention page is held: allocation turns=%d retention pages=%v", turns, pages)
		}
		release <- struct{}{}
		synctest.Wait()
		wake <- struct{}{}
		synctest.Wait()
		if turns != 3 || len(pages) != 2 || pages[1] != "next-tenant" {
			t.Fatalf("after releasing the first page: allocation turns=%d retention pages=%v", turns, pages)
		}
		release <- struct{}{}
		synctest.Wait()
		wake <- struct{}{}
		synctest.Wait()
		if turns != 4 || len(pages) != 2 {
			t.Fatalf("completed sweep restarted before the next hour: turns=%d pages=%q", turns, pages)
		}
		clock = clock.Add(time.Hour)
		wake <- struct{}{}
		synctest.Wait()
		if turns != 5 || len(pages) != 3 || pages[2] != "" {
			t.Fatalf("next hourly sweep: turns=%d pages=%q", turns, pages)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("allocator shutdown returned with retention still running")
		default:
		}
	})
}
