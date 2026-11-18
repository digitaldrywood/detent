package githuboutbound

import (
	"context"
	"sync"
	"time"
)

type Worker struct {
	service *Service
	cancel  context.CancelFunc
	wake    chan struct{}
	wait    sync.WaitGroup
}

type Timer struct {
	Ticks <-chan time.Time
	Reset func(time.Duration)
	Stop  func()
}

func NewWorker(service *Service) *Worker {
	return &Worker{
		service: service,
		wake:    make(chan struct{}, 1),
	}
}

func (w *Worker) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.wait.Add(1)
	go func() {
		defer w.wait.Done()
		w.run(ctx)
	}()
}

func (w *Worker) run(ctx context.Context) {
	interval := w.service.deps.OutboxPollInterval
	base := interval
	wait := func() time.Duration {
		return RetryDelay(1, interval, interval, w.service.deps.Jitter())
	}
	timer := w.service.deps.NewTimer(wait())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			interval = base
		case <-timer.Ticks:
		}
		worked := false
		for range 64 {
			var github func(context.Context) (bool, error)
			if w.service.deps.Backend != nil {
				github = w.service.Process
				if w.service.deps.Process != nil {
					github = w.service.deps.Process
				}
			}
			processed, err := processOutboxStep(ctx, github)
			if err != nil && ctx.Err() == nil {
				w.service.deps.Logger.WarnContext(ctx, "outbox delivery failed", "error", err)
			}
			worked = worked || processed
			if !processed || ctx.Err() != nil {
				break
			}
		}
		if worked {
			interval = base
		} else if interval < time.Minute {
			interval = min(2*interval, time.Minute)
		}
		timer.Reset(wait())
	}
}

func processOutboxStep(ctx context.Context, github func(context.Context) (bool, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if github == nil {
		return false, nil
	}
	return github(ctx)
}

func (w *Worker) Stop() {
	if w == nil {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	w.wait.Wait()
}
