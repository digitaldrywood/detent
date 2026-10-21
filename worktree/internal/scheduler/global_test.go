package scheduler_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestGlobalSchedulerDoesNotExposeCountingSemaphoreCounters(t *testing.T) {
	t.Parallel()

	global := scheduler.NewStrictPriority(scheduler.Config{Capacity: 1})
	if _, ok := global.(interface{ Counters() scheduler.Counters }); ok {
		t.Fatalf("global scheduler exposes counting semaphore Counters()")
	}
}

func newGlobalScheduler(t *testing.T, cfg scheduler.Config) scheduler.GlobalScheduler {
	t.Helper()

	sched, err := scheduler.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	global, ok := sched.(scheduler.GlobalScheduler)
	if !ok {
		t.Fatalf("NewFromConfig() returned %T, want GlobalScheduler", sched)
	}
	return global
}
