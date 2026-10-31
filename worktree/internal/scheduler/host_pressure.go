package scheduler

import (
	"sync"

	"github.com/digitaldrywood/detent/internal/hostpressure"
)

type HostPressureSnapshot struct {
	Memory hostpressure.MemoryPressure
	IO     hostpressure.IOPressure
	CPU    hostpressure.CPUPressure
}

type HostPressure struct {
	mu       sync.Mutex
	snapshot HostPressureSnapshot
	observed bool
}

func (p *HostPressure) Update(update func(*HostPressureSnapshot)) HostPressureSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	update(&p.snapshot)
	p.observed = true
	return p.snapshot
}

func (p *HostPressure) Capacity() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := p.snapshot
	if snapshot.Memory.DispatchHeld || snapshot.IO.DispatchHeld || snapshot.CPU.DispatchHeld {
		return 0, p.observed
	}
	capacity := -1
	if snapshot.IO.CapacityConstrained {
		capacity = snapshot.IO.EffectiveMaxConcurrentAgents
	}
	if snapshot.CPU.CapacityConstrained && (capacity < 0 || snapshot.CPU.EffectiveMaxConcurrentAgents < capacity) {
		capacity = snapshot.CPU.EffectiveMaxConcurrentAgents
	}
	return capacity, p.observed
}

func (r *PoolRegistry) HostPressure() *HostPressure { return &r.pressure }

func (g *projectPoolGate) HostPressure() *HostPressure { return g.registry.HostPressure() }

func (g *GlobalDispatchGate) HostPressure() *HostPressure {
	if g.pressureOwner != nil {
		return g.pressureOwner
	}
	return &g.pressure
}
