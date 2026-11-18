package advicetest

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

const gib = 1024 * 1024 * 1024

func Start() time.Time {
	return time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
}

func Summaries() []hostmetrics.Summary {
	hours := make([]hostmetrics.Summary, 24)
	for i := range hours {
		hours[i] = hostmetrics.Summary{
			Hour: Start().Add(time.Duration(i) * time.Hour), SampleCount: 120, LogicalCores: 16,
			MemoryTotalBytes: 64 * gib, MemoryAvailableMinBytes: 56 * gib, MemorySampleCount: 120,
			CPUBusySumPercent: 1200, CPUSampleCount: 120,
			DiskFreeMinBytes: 400 * gib, DiskTotalMinBytes: 512 * gib, DiskSampleCount: 120,
		}
	}
	return hours
}

func Usages() []procgroup.Usage {
	runs := make([]procgroup.Usage, 10)
	for i := range runs {
		runs[i] = procgroup.Usage{PeakMemoryBytes: 4 * gib, UserCPUSeconds: 90, SystemCPUSeconds: 10, WallSeconds: 100}
	}
	runs[9].PeakMemoryBytes = 40 * gib
	return runs
}
