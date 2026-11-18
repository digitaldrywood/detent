package runneradvice

import (
	"math"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
)

func MergeHostSummary(a, b hostmetrics.Summary) (hostmetrics.Summary, bool) {
	pairs := []struct {
		target *uint64
		value  uint64
	}{
		{&a.SampleCount, b.SampleCount}, {&a.MemoryAvailableSumBytes, b.MemoryAvailableSumBytes}, {&a.MemorySampleCount, b.MemorySampleCount}, {&a.SwapSampleCount, b.SwapSampleCount}, {&a.PSISomeSampleCount, b.PSISomeSampleCount}, {&a.PSIFullSampleCount, b.PSIFullSampleCount}, {&a.PressureWarnCount, b.PressureWarnCount}, {&a.PressureCriticalCount, b.PressureCriticalCount}, {&a.PressureSampleCount, b.PressureSampleCount}, {&a.CPUSampleCount, b.CPUSampleCount}, {&a.LoadSampleCount, b.LoadSampleCount}, {&a.DiskSampleCount, b.DiskSampleCount},
	}
	if b.MemorySampleCount > 0 {
		if a.MemorySampleCount == 0 {
			a.MemoryAvailableMinBytes = b.MemoryAvailableMinBytes
		} else {
			a.MemoryAvailableMinBytes = min(a.MemoryAvailableMinBytes, b.MemoryAvailableMinBytes)
		}
		a.MemoryTotalBytes = max(a.MemoryTotalBytes, b.MemoryTotalBytes)
	}
	if b.DiskSampleCount > 0 {
		if a.DiskSampleCount == 0 {
			a.DiskFreeMinBytes = b.DiskFreeMinBytes
			a.DiskTotalMinBytes = b.DiskTotalMinBytes
		} else {
			a.DiskFreeMinBytes = min(a.DiskFreeMinBytes, b.DiskFreeMinBytes)
			a.DiskTotalMinBytes = min(a.DiskTotalMinBytes, b.DiskTotalMinBytes)
		}
	}
	for _, pair := range pairs {
		if math.MaxUint64-*pair.target < pair.value {
			return a, false
		}
		*pair.target += pair.value
	}
	a.PSISomeAvg10Sum += b.PSISomeAvg10Sum
	a.PSIFullAvg10Sum += b.PSIFullAvg10Sum
	a.CPUBusySumPercent += b.CPUBusySumPercent
	a.CPUBusyMaxPercent = max(a.CPUBusyMaxPercent, b.CPUBusyMaxPercent)
	a.SwapUsedMaxBytes = max(a.SwapUsedMaxBytes, b.SwapUsedMaxBytes)
	a.Load1Max = max(a.Load1Max, b.Load1Max)
	a.Partial = a.Partial || b.Partial
	a.LogicalCores = max(a.LogicalCores, b.LogicalCores)
	return a, true
}
