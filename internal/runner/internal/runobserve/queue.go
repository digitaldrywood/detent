package runobserve

import (
	"context"
	"errors"
)

func Start(ctx context.Context, authorityLost error, report func(error)) (context.Context, func(func(context.Context) error), func() error) {
	turnCtx, cancelTurn := context.WithCancelCause(ctx)
	observationCtx, stop := context.WithCancel(turnCtx)
	pending := make(chan func(context.Context) error, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for observationCtx.Err() == nil {
			select {
			case <-observationCtx.Done():
				return
			case observe := <-pending:
				err := observe(observationCtx)
				if errors.Is(err, authorityLost) {
					cancelTurn(err)
					return
				}
				if err != nil && observationCtx.Err() == nil {
					report(err)
				}
			}
		}
	}()
	return turnCtx, func(observe func(context.Context) error) {
			select {
			case pending <- observe:
			default:
			}
		}, func() error {
			stop()
			<-done
			cause := context.Cause(turnCtx)
			cancelTurn(nil)
			if errors.Is(cause, authorityLost) {
				return cause
			}
			return nil
		}
}

type ArtifactLog struct {
	queued, written int
}

func (a *ArtifactLog) Record(submit func(func(context.Context) error), write func(context.Context, string) error, delta string) {
	a.queued++
	submit(func(ctx context.Context) error {
		if err := write(ctx, delta); err != nil {
			return err
		}
		a.written++
		return nil
	})
}

func (a *ArtifactLog) Incomplete() bool {
	return a.queued != a.written
}
