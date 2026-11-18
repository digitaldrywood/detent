package advicetest_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice/advicetest"
)

func TestFixtureShape(t *testing.T) {
	t.Parallel()
	summaries, usages := advicetest.Summaries(), advicetest.Usages()
	if len(summaries) != 24 || len(usages) != 10 {
		t.Fatalf("fixture has %d hours and %d runs", len(summaries), len(usages))
	}
	for index, summary := range summaries {
		if !summary.Hour.Equal(advicetest.Start().Add(time.Duration(index)*time.Hour)) || summary.MemoryTotalBytes != 64*runneradvice.CapacityGiB || summary.MemoryAvailableMinBytes != 56*runneradvice.CapacityGiB || summary.DiskFreeMinBytes != 400*runneradvice.CapacityGiB || summary.DiskTotalMinBytes != 512*runneradvice.CapacityGiB || summary.SampleCount != 120 || summary.LogicalCores != 16 || summary.CPUBusySumPercent != 1200 {
			t.Fatalf("hour %d = %+v", index, summary)
		}
	}
	for index, usage := range usages {
		peak := 4 * runneradvice.CapacityGiB
		if index == 9 {
			peak = 40 * runneradvice.CapacityGiB
		}
		if int64(usage.PeakMemoryBytes) != int64(peak) || usage.UserCPUSeconds != 90 || usage.SystemCPUSeconds != 10 || usage.WallSeconds != 100 {
			t.Fatalf("run %d = %+v", index, usage)
		}
	}
}
