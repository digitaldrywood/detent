package hubclient

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/digitaldrywood/detent/internal/runner"
)

// defaultLeaseHold keeps a Sprite awake from this runner's first native lease
// until a candidate fetch finds nothing left to claim. A Sprite pauses the
// moment its last task is deleted (sessions get a grace period, tasks do not),
// and an outbound Hub connection does not count, so a hold dropped with the
// lease at completion paused the staging Sprite before the next tick could
// claim the landing the completion had just made ready. Off a Sprite there is
// nothing to hold.
func defaultLeaseHold() func(context.Context) (func(), error) {
	if runtime.GOOS != "linux" || !runner.SpriteSocketPresent() {
		return nil
	}
	return func(ctx context.Context) (func(), error) {
		return runner.HoldSpriteTask(ctx, func() {
			slog.Default().Warn("sprite lease hold refresh refused")
		})
	}
}

// syncLeaseHold acquires the hold when a candidate fetch records the first
// native lease and releases it when a fetch completes with none recorded.
// Releasing a lease never drops the hold on its own: the fetch that follows a
// completion either takes the next lease under the same hold or confirms the
// runner is idle. Callers invoke it after mutating nativeClaims and after
// releasing s.mu.
func (s *Scheduler) syncLeaseHold(ctx context.Context) {
	if s == nil || s.leaseHold == nil {
		return
	}
	s.mu.Lock()
	want := len(s.nativeClaims) > 0
	release := s.leaseHoldRelease
	if want == (release != nil) || s.leaseHoldPending {
		s.mu.Unlock()
		return
	}
	if !want {
		s.leaseHoldRelease = nil
		s.mu.Unlock()
		release()
		return
	}
	s.leaseHoldPending = true
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	acquired, err := s.leaseHold(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.leaseHoldPending = false
	if err != nil {
		s.mu.Unlock()
		slog.Default().Warn("sprite lease hold unavailable", "error", err)
		return
	}
	if len(s.nativeClaims) == 0 {
		s.mu.Unlock()
		acquired()
		return
	}
	s.leaseHoldRelease = acquired
	s.mu.Unlock()
}
