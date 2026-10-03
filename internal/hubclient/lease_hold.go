package hubclient

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/digitaldrywood/detent/internal/runner"
)

// defaultLeaseHold keeps a Sprite awake while this runner holds a native
// lease. A Sprite pauses about 30 seconds after its last session, and an
// outbound Hub connection does not count, so a lease held across a job's
// completion and the next dispatch would otherwise sleep until something
// external wakes the Sprite. Off a Sprite there is nothing to hold.
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

// syncLeaseHold acquires the hold when the first native lease is recorded and
// releases it when the last one is removed. Callers invoke it after mutating
// nativeClaims and after releasing s.mu.
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
