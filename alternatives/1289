package scenariohooks

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/runner"
)

type GatedScheduler struct {
	*dispatch.Scheduler
	closed atomic.Bool
	opened chan struct{}
	once   sync.Once
}

func NewGatedScheduler(t *testing.T, scheduler *dispatch.Scheduler) *GatedScheduler {
	g := &GatedScheduler{Scheduler: scheduler, opened: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *GatedScheduler) RunExecution(id string) runner.Execution {
	if g.closed.Load() {
		<-g.opened
	}
	return g.Scheduler.RunExecution(id)
}

func (g *GatedScheduler) Hold() *dispatch.Scheduler {
	g.closed.Store(true)
	return g.Scheduler
}

func (g *GatedScheduler) Release(then func()) {
	g.open()
	then()
}

func (g *GatedScheduler) open() { g.once.Do(func() { close(g.opened) }) }
