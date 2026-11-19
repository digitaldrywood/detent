package workspacehub

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

type SweepDeps struct {
	Now             func() time.Time
	Due             func(context.Context, time.Time) ([]Record, error)
	ApplyDeadline   func(context.Context, Record, time.Time) (Record, bool, error)
	Committed       func(context.Context, Record)
	SweepActions    func(context.Context)
	DispatchActions func(context.Context)
	SweepRelay      func(context.Context, time.Time)
	SweepTickets    func(context.Context, time.Time) error
	Logger          *slog.Logger
}

func Maintain(ctx context.Context, stop <-chan struct{}, stopped chan<- struct{}, interval func() time.Duration, newTimer func(time.Duration) *time.Timer, sweep func(context.Context)) {
	defer close(stopped)
	next := func() time.Duration {
		spread := interval() / 2
		if spread <= 0 {
			return interval()
		}
		return interval() - spread/2 + time.Duration(rand.Int64N(int64(spread)))
	}
	timer := newTimer(next())
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			sweep(ctx)
			timer.Reset(next())
		}
	}
}

func Sweep(ctx context.Context, deps SweepDeps) {
	now := deps.Now()
	records, err := deps.Due(ctx, now)
	if err != nil {
		deps.Logger.Warn("workspace.sweep_failed", "error", err)
		return
	}
	for _, record := range records {
		moved, changed, err := deps.ApplyDeadline(ctx, record, now)
		if err != nil {
			deps.Logger.Warn("workspace.sweep_transition_failed", "workspace_id", record.ID, "error", err)
			continue
		}
		if changed {
			deps.Committed(ctx, moved)
		}
	}
	deps.SweepActions(ctx)
	deps.DispatchActions(ctx)
	deps.SweepRelay(ctx, now)
	if err := deps.SweepTickets(ctx, now); err != nil {
		deps.Logger.Warn("workspace.ticket_sweep_failed", "error", err)
	}
}
