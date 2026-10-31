package hubclient

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/digitaldrywood/detent/internal/runner"
)

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

func (s *Scheduler) syncLeaseHold(ctx context.Context) {
	if s == nil || s.leaseHold == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.mu.Lock()
		if pending := s.leaseHoldPending; pending != nil {
			s.mu.Unlock()
			<-pending
			continue
		}
		want := len(s.nativeClaims) > 0 || s.nativeFetches > 0
		release := s.leaseHoldRelease
		if want == (release != nil) {
			s.mu.Unlock()
			return
		}
		pending := make(chan struct{})
		s.leaseHoldPending = pending
		if !want {
			s.leaseHoldRelease = nil
		}
		s.mu.Unlock()
		var acquired func()
		var err error
		if want {
			acquired, err = s.leaseHold(context.WithoutCancel(ctx))
		} else {
			release()
		}
		s.mu.Lock()
		if err == nil && want {
			s.leaseHoldRelease = acquired
		}
		s.leaseHoldPending = nil
		close(pending)
		s.mu.Unlock()
		if err != nil {
			slog.Default().Warn("sprite lease hold unavailable", "error", err)
			return
		}
	}
}
