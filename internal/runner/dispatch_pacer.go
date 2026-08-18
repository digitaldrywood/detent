package runner

import (
	"context"
	"crypto/rand"
	"math/big"
	"sync"
	"time"
)

type DispatchPacer interface {
	Wait(context.Context) error
}

type StartupDispatchPacerConfig struct {
	MaxStartsPerSecond int
	Jitter             time.Duration
	RampStarts         int
	Sleep              func(context.Context, time.Duration) error
	RandomJitter       func(time.Duration) time.Duration
}

type StartupDispatchPacer struct {
	mu                 sync.Mutex
	maxStartsPerSecond int
	jitter             time.Duration
	rampStarts         int
	started            int
	sleep              func(context.Context, time.Duration) error
	randomJitter       func(time.Duration) time.Duration
}

func NewStartupDispatchPacer(cfg StartupDispatchPacerConfig) *StartupDispatchPacer {
	if cfg.RampStarts < 1 {
		cfg.RampStarts = 1
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepDispatchPacer
	}
	if cfg.RandomJitter == nil {
		cfg.RandomJitter = randomDispatchJitter
	}
	return &StartupDispatchPacer{
		maxStartsPerSecond: cfg.MaxStartsPerSecond,
		jitter:             cfg.Jitter,
		rampStarts:         cfg.RampStarts,
		sleep:              cfg.Sleep,
		randomJitter:       cfg.RandomJitter,
	}
}

func (p *StartupDispatchPacer) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started >= p.rampStarts {
		return nil
	}
	if p.started > 0 {
		delay := time.Duration(0)
		if p.maxStartsPerSecond > 0 {
			delay = time.Second / time.Duration(p.maxStartsPerSecond)
		}
		if p.jitter > 0 {
			delay += p.randomJitter(p.jitter)
		}
		if err := p.sleep(ctx, delay); err != nil {
			return err
		}
	}
	p.started++
	return nil
}

func sleepDispatchPacer(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func randomDispatchJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)))
	if err != nil {
		return 0
	}
	return time.Duration(value.Int64())
}
