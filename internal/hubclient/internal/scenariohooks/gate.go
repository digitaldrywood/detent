package scenariohooks

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/runner"
)

type Gate struct {
	closed atomic.Bool
	opened chan struct{}
	once   sync.Once
}

func NewGate(t *testing.T) *Gate {
	g := &Gate{opened: make(chan struct{})}
	t.Cleanup(g.Open)
	return g
}

func (g *Gate) Close() { g.closed.Store(true) }

func (g *Gate) Open() { g.once.Do(func() { close(g.opened) }) }

func (g *Gate) Pass(id string) string {
	if g.closed.Load() {
		<-g.opened
	}
	return id
}

type GatedScheduler struct {
	*dispatch.Scheduler
	Gate *Gate
}

func (s GatedScheduler) RunExecution(id string) runner.Execution {
	return s.Scheduler.RunExecution(s.Gate.Pass(id))
}
