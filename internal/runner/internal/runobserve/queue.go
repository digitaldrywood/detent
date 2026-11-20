package runobserve

import (
	"context"
	"errors"
)

func Start(ctx context.Context, authorityLost error, report func(error)) (context.Context, func(func(context.Context) error) bool, func() error) {
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
				if err := observe(observationCtx); err != nil && observationCtx.Err() == nil {
					if errors.Is(err, authorityLost) {
						cancelTurn(err)
						return
					}
					report(err)
				}
			}
		}
	}()
	return turnCtx, func(observe func(context.Context) error) bool {
			select {
			case pending <- observe:
				return true
			default:
				return false
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
