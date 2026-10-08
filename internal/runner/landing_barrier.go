package runner

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var ErrLandingBarrierRed = errors.New("the rolling landing barrier is red; waiting for its repair")

type LandingBarrierOwner interface {
	NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error)
	FinishLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult) error
}

type LandingBarrierAuthorization interface {
	AuthorizeLanding(context.Context, string) error
}

func (r *Runner) RunLandingBarriers(ctx context.Context, owner LandingBarrierOwner) {
	backend, ok := r.workspace.(workspace.LandingBarrierWorkspace)
	if !ok {
		return
	}
	var release func() error
	defer func() {
		if release != nil {
			if err := release(); err != nil {
				r.logger.Warn("landing barrier runner release failed", "error", err)
			}
		}
	}()
	recoverClaim := true
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workflow, _, _, _ := r.runtimeSnapshot()
		cfg := gate.Effective(workflow.Config.Gate)
		if cfg.LandingMode == gate.LandingRollingBarrier {
			if release == nil {
				var err error
				release, err = backend.AcquireLandingBarrierRunner(ctx)
				if err != nil {
					r.logger.Warn("landing barrier runner unavailable", "error", err)
					return
				}
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err == nil {
				barrier, started, claimErr := owner.NextLandingBarrier(ctx, r.projectID, backend.LandingRepository(ctx), descriptor.ID, recoverClaim, backend.LandingBarrierHead)
				if claimErr == nil {
					recoverClaim = false
				}
				if claimErr != nil {
					r.logger.Warn("landing barrier claim failed", "error", claimErr)
				}
				if started {
					result, runErr := backend.RunLandingBarrier(ctx, barrier.ID, barrier.BaseRef, cfg.Run)
					var completed *gate.CommandResult
					if runErr == nil {
						completed = &result
					} else {
						r.logger.Warn("landing barrier instance failure", "error", runErr)
					}
					if !r.finishLandingBarrier(ctx, owner, barrier, completed) {
						return
					}
					if runErr == nil {
						continue
					}
				}
			} else {
				r.logger.Warn("landing barrier policy unavailable", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) finishLandingBarrier(ctx context.Context, owner LandingBarrierOwner, barrier tracker.LandingBarrier, result *gate.CommandResult) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		reportCtx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			reportCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
		}
		err := owner.FinishLandingBarrier(reportCtx, r.projectID, barrier, result)
		if err == nil {
			return ctx.Err() == nil
		}
		r.logger.Warn("landing barrier publication failed", "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
