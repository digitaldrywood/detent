package runobserve

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Queue struct {
	mu            sync.Mutex
	pending       []func(context.Context) error
	latest        func(context.Context) error
	closing       bool
	required      bool
	wake          chan struct{}
	done          chan struct{}
	stop          context.CancelCauseFunc
	cancel        context.CancelCauseFunc
	turnCtx       context.Context
	writeErr      error
	authorityLost error
}

func Start(ctx context.Context, authorityLost error, report func(error)) (context.Context, *Queue) {
	return start(ctx, authorityLost, report, false)
}

func StartRequired(ctx context.Context, authorityLost error, report func(error)) (context.Context, *Queue) {
	return start(ctx, authorityLost, report, true)
}

func start(ctx context.Context, authorityLost error, report func(error), required bool) (context.Context, *Queue) {
	turnCtx, cancelTurn := context.WithCancelCause(ctx)
	observationCtx := turnCtx
	if required {
		observationCtx = context.WithoutCancel(turnCtx)
	}
	observationCtx, stop := context.WithCancelCause(observationCtx)
	q := &Queue{required: required, authorityLost: authorityLost, turnCtx: turnCtx, cancel: cancelTurn, stop: stop,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(q.done)
		for observationCtx.Err() == nil {
			write, required, closing := q.next()
			if write == nil && closing {
				return
			}
			if write == nil {
				select {
				case <-observationCtx.Done():
					return
				case <-q.wake:
				}
				continue
			}
			err := write(observationCtx)
			if err == nil {
				continue
			}
			if errors.Is(err, authorityLost) {
				q.writeErr = err
				cancelTurn(err)
				return
			}
			if required {
				q.writeErr = fmt.Errorf("persist required turn observation: %w", err)
				return
			}
			if observationCtx.Err() == nil {
				report(err)
			}
		}
	}()
	return turnCtx, q
}

func (q *Queue) Observe(write func(context.Context) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closing && (q.required || len(q.pending) < 32) {
		q.pending = append(q.pending, write)
		q.signal()
	}
}

func (q *Queue) ObserveLatest(write func(context.Context) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closing {
		q.latest = write
		q.signal()
	}
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) next() (func(context.Context) error, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 0 {
		write := q.pending[0]
		q.pending[0] = nil
		q.pending = q.pending[1:]
		return write, q.required, q.closing
	}
	write := q.latest
	q.latest = nil
	return write, false, q.closing
}

func (q *Queue) Finish(ctx context.Context) error {
	defer q.cancel(nil)
	defer q.stop(nil)
	var drainErr error
	if q.required {
		q.mu.Lock()
		q.closing = true
		q.mu.Unlock()
		q.signal()
		select {
		case <-q.done:
		case <-ctx.Done():
			drainErr = fmt.Errorf("drain required turn observations: %w", context.Cause(ctx))
			q.stop(drainErr)
		}
	} else {
		q.stop(nil)
	}
	<-q.done
	if cause := context.Cause(q.turnCtx); q.writeErr == nil && errors.Is(cause, q.authorityLost) {
		return errors.Join(drainErr, cause)
	}
	return errors.Join(drainErr, q.writeErr)
}
