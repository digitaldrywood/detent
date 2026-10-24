package hostmetrics

import (
	"context"
	"math"
	"runtime"
	"slices"
	"sync"
	"time"
)

const (
	SampleInterval = 30 * time.Second
	retainedHours  = 24
)

type cpuCounters struct {
	total float64
	idle  float64
}

type reading struct {
	cpu             cpuCounters
	memoryTotal     uint64
	memoryAvailable uint64
	swapUsed        uint64
	psiSome         float64
	psiFull         float64
	pressureLevel   uint32
	load1           float64
	diskFree        uint64
	diskTotal       uint64
	cpuOK           bool
	memoryOK        bool
	swapOK          bool
	psiSomeOK       bool
	psiFullOK       bool
	pressureOK      bool
	loadOK          bool
	diskOK          bool
}

type Collector struct {
	read     func(context.Context) reading
	current  Summary
	pending  []Summary
	previous cpuCounters
	cpuOK    bool
	mu       sync.Mutex
}

func New(root string, started time.Time) *Collector {
	return &Collector{
		read:    func(ctx context.Context) reading { return readHost(ctx, root) },
		current: Summary{SegmentID: started.UTC(), LogicalCores: runtime.NumCPU()},
		pending: make([]Summary, 0, retainedHours),
	}
}

func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		at := time.Now().UTC()
		c.record(at, c.read(ctx))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Collector) Summaries() []Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pending)
}

func (c *Collector) Acknowledge(sent []Summary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = slices.DeleteFunc(c.pending, func(summary Summary) bool {
		return slices.Contains(sent, summary)
	})
}

func (c *Collector) Close(at time.Time) []Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finish(!at.UTC().Truncate(time.Hour).After(c.current.Hour))
	return slices.Clone(c.pending)
}

func (c *Collector) finish(partial bool) {
	if c.current.SampleCount > 0 {
		c.current.Partial = partial
		if len(c.pending) == retainedHours {
			c.pending = slices.Delete(c.pending, 0, 1)
		}
		c.pending = append(c.pending, c.current)
	}
	c.current = Summary{SegmentID: c.current.SegmentID, LogicalCores: c.current.LogicalCores}
}

func (c *Collector) record(at time.Time, sample reading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hour := at.UTC().Truncate(time.Hour)
	if !c.current.Hour.IsZero() && !hour.Equal(c.current.Hour) {
		c.finish(false)
	}
	c.current.Hour = hour
	s := &c.current
	if sample.memoryOK || sample.swapOK || sample.cpuOK || sample.loadOK || sample.diskOK || sample.psiSomeOK || sample.psiFullOK || sample.pressureOK {
		s.SampleCount++
	}
	if sample.memoryOK {
		if s.MemorySampleCount == 0 {
			s.MemoryAvailableMinBytes = sample.memoryAvailable
		} else {
			s.MemoryAvailableMinBytes = min(s.MemoryAvailableMinBytes, sample.memoryAvailable)
		}
		s.MemoryTotalBytes = sample.memoryTotal
		s.MemoryAvailableSumBytes += sample.memoryAvailable
		s.MemorySampleCount++
	}
	if sample.swapOK {
		s.SwapUsedMaxBytes = max(s.SwapUsedMaxBytes, sample.swapUsed)
		s.SwapSampleCount++
	}
	if sample.psiSomeOK {
		s.PSISomeAvg10Sum += sample.psiSome
		s.PSISomeSampleCount++
	}
	if sample.psiFullOK {
		s.PSIFullAvg10Sum += sample.psiFull
		s.PSIFullSampleCount++
	}
	if sample.pressureOK {
		s.PressureSampleCount++
		switch sample.pressureLevel {
		case 2:
			s.PressureWarnCount++
		case 4:
			s.PressureCriticalCount++
		}
	}
	if c.cpuOK && sample.cpuOK {
		if busy, ok := cpuBusy(c.previous, sample.cpu); ok {
			s.CPUBusySumPercent += busy
			s.CPUBusyMaxPercent = max(s.CPUBusyMaxPercent, busy)
			s.CPUSampleCount++
		}
	}
	c.previous, c.cpuOK = sample.cpu, sample.cpuOK
	if sample.loadOK {
		s.Load1Max = max(s.Load1Max, sample.load1)
		s.LoadSampleCount++
	}
	if sample.diskOK {
		if s.DiskSampleCount == 0 {
			s.DiskFreeMinBytes, s.DiskTotalMinBytes = sample.diskFree, sample.diskTotal
		} else {
			s.DiskFreeMinBytes = min(s.DiskFreeMinBytes, sample.diskFree)
			s.DiskTotalMinBytes = min(s.DiskTotalMinBytes, sample.diskTotal)
		}
		s.DiskSampleCount++
	}
}

func cpuBusy(previous, current cpuCounters) (float64, bool) {
	total, idle := current.total-previous.total, current.idle-previous.idle
	if total <= 0 || idle < 0 || idle > total || math.IsNaN(total) || math.IsNaN(idle) || math.IsInf(total, 0) || math.IsInf(idle, 0) {
		return 0, false
	}
	return 100 * (total - idle) / total, true
}
