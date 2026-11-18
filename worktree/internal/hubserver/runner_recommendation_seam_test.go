package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func recommendationFixture() ([]recommendationHour, []recommendationRun) {
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	hours := make([]recommendationHour, 24)
	for i := range hours {
		hours[i] = recommendationHour{Summary: hostmetrics.Summary{
			Hour: start.Add(time.Duration(i) * time.Hour), SampleCount: 120, LogicalCores: 16,
			MemoryTotalBytes: 64 * capacityGiB, MemoryAvailableMinBytes: 56 * capacityGiB, MemorySampleCount: 120,
			CPUBusySumPercent: 1200, CPUSampleCount: 120,
			DiskFreeMinBytes: 400 * capacityGiB, DiskTotalMinBytes: 512 * capacityGiB, DiskSampleCount: 120,
		}}
	}
	runs := make([]recommendationRun, 10)
	for i := range runs {
		runs[i] = recommendationRun{StartedAt: start.Add(time.Duration(i) * time.Hour), Usage: procgroup.Usage{PeakMemoryBytes: 4 * capacityGiB, UserCPUSeconds: 90, SystemCPUSeconds: 10, WallSeconds: 100}}
	}
	runs[9].Usage.PeakMemoryBytes = 40 * capacityGiB
	return hours, runs
}
