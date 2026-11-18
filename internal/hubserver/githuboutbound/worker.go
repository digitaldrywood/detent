package githuboutbound

import (
	"context"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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

func (w *Worker) Wake(org tracker.OrganizationID) {
	if org != w.service.deps.Organization {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
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
	process := w.service.deps.Process
	if process == nil && w.service.deps.Backend != nil {
		process = w.service.processNext
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			interval = base
		case <-timer.Ticks:
		}
		var next time.Time
		worked := false
		for range 64 {
			processed, deadline, err := processOutboxStep(ctx, process)
			next = deadline
			if err != nil && ctx.Err() == nil {
				w.service.deps.Logger.WarnContext(ctx, "outbox delivery failed", "error", err)
			}
			worked = worked || processed || err != nil
			if !processed || ctx.Err() != nil {
				break
			}
		}
		var until time.Duration
		if !next.IsZero() {
			until = next.Sub(w.service.deps.Now())
		}
		if worked || !next.IsZero() && until <= 0 {
			interval = base
		} else if interval < time.Minute {
			interval = min(2*interval, time.Minute)
		}
		delay := wait()
		if until > 0 {
			delay = min(delay, until)
		}
		timer.Reset(delay)
	}
}

func processOutboxStep(ctx context.Context, github func(context.Context) (bool, time.Time, error)) (bool, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return false, time.Time{}, err
	}
	if github == nil {
		return false, time.Time{}, nil
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
