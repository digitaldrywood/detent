package hubtest

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
)

func HostSummary(now time.Time) hostmetrics.Summary {
	return hostmetrics.Summary{
		Hour: now.UTC().Truncate(time.Hour), SegmentID: now.Add(-time.Minute), Partial: true, SampleCount: 2, LogicalCores: 8,
		MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 100, MemoryAvailableSumBytes: 400, MemorySampleCount: 2,
		SwapUsedMaxBytes: 50, SwapSampleCount: 2, PSISomeAvg10Sum: 20, PSISomeSampleCount: 2, PSIFullAvg10Sum: 4, PSIFullSampleCount: 2,
		PressureWarnCount: 1, PressureSampleCount: 2, CPUBusySumPercent: 80, CPUBusyMaxPercent: 60, CPUSampleCount: 2,
		Load1Max: 3, LoadSampleCount: 2, DiskFreeMinBytes: 100, DiskTotalMinBytes: 1000, DiskSampleCount: 2,
	}
}
