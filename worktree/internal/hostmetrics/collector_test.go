package hostmetrics

import (
	"context"
	"math"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestCPUBusy(t *testing.T) {
	for _, test := range []struct {
		name     string
		previous cpuCounters
		current  cpuCounters
		want     float64
		valid    bool
	}{
		{"half busy", cpuCounters{100, 60}, cpuCounters{120, 70}, 50, true},
		{"fully busy", cpuCounters{100, 60}, cpuCounters{120, 60}, 100, true},
		{"idle", cpuCounters{100, 60}, cpuCounters{120, 80}, 0, true},
		{"multiple cores", cpuCounters{1000, 300}, cpuCounters{1120, 390}, 25, true},
		{"unchanged", cpuCounters{100, 60}, cpuCounters{100, 60}, 0, false},
		{"counter reset", cpuCounters{100, 60}, cpuCounters{20, 10}, 0, false},
		{"idle reset", cpuCounters{100, 60}, cpuCounters{120, 50}, 0, false},
		{"idle exceeds elapsed", cpuCounters{100, 60}, cpuCounters{120, 90}, 0, false},
		{"nonfinite total", cpuCounters{}, cpuCounters{math.Inf(1), 0}, 0, false},
		{"nonfinite idle", cpuCounters{}, cpuCounters{100, math.NaN()}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := cpuBusy(test.previous, test.current)
			if got != test.want || valid != test.valid {
				t.Fatalf("cpuBusy = (%v, %v), want (%v, %v)", got, valid, test.want, test.valid)
			}
		})
	}
}

func TestHourlySummaries(t *testing.T) {
	hour := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	segment := hour.Add(10 * time.Minute)
	memory := reading{memoryOK: true, memoryTotal: 1000, memoryAvailable: 600}
	for _, test := range []struct {
		name    string
		times   []time.Duration
		samples []reading
		closeAt time.Duration
		want    []Summary
	}{
		{
			name:  "partial hour aggregates all fields",
			times: []time.Duration{10 * time.Minute, 11 * time.Minute},
			samples: []reading{
				{memoryOK: true, memoryTotal: 1000, memoryAvailable: 600, swapOK: true, swapUsed: 50, cpuOK: true, cpu: cpuCounters{100, 40}, psiSomeOK: true, psiSome: 3, psiFullOK: true, psiFull: 1, loadOK: true, load1: 2, diskOK: true, diskFree: 400, diskTotal: 900},
				{memoryOK: true, memoryTotal: 1000, memoryAvailable: 300, swapOK: true, swapUsed: 20, cpuOK: true, cpu: cpuCounters{120, 50}, psiSomeOK: true, psiSome: 5, psiFullOK: true, psiFull: 2, loadOK: true, load1: 1, diskOK: true, diskFree: 200, diskTotal: 800},
			},
			closeAt: 12 * time.Minute,
			want:    []Summary{{Hour: hour, SegmentID: segment, LogicalCores: 4, Partial: true, SampleCount: 2, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 300, MemoryAvailableSumBytes: 900, MemorySampleCount: 2, SwapUsedMaxBytes: 50, SwapSampleCount: 2, PSISomeAvg10Sum: 8, PSISomeSampleCount: 2, PSIFullAvg10Sum: 3, PSIFullSampleCount: 2, CPUBusySumPercent: 50, CPUBusyMaxPercent: 50, CPUSampleCount: 1, Load1Max: 2, LoadSampleCount: 2, DiskFreeMinBytes: 200, DiskTotalMinBytes: 800, DiskSampleCount: 2}},
		},
		{
			name:    "UTC rollover and missing hours",
			times:   []time.Duration{59 * time.Minute, time.Hour, 3 * time.Hour},
			samples: []reading{memory, memory, memory},
			closeAt: 3*time.Hour + time.Minute,
			want: []Summary{
				{Hour: hour, SegmentID: segment, LogicalCores: 4, SampleCount: 1, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 600, MemoryAvailableSumBytes: 600, MemorySampleCount: 1},
				{Hour: hour.Add(time.Hour), SegmentID: segment, LogicalCores: 4, SampleCount: 1, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 600, MemoryAvailableSumBytes: 600, MemorySampleCount: 1},
				{Hour: hour.Add(3 * time.Hour), SegmentID: segment, LogicalCores: 4, Partial: true, SampleCount: 1, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 600, MemoryAvailableSumBytes: 600, MemorySampleCount: 1},
			},
		},
		{
			name:  "unsupported readings leave gaps and reset CPU baseline",
			times: []time.Duration{10 * time.Minute, 11 * time.Minute, 12 * time.Minute, 13 * time.Minute},
			samples: []reading{
				{cpuOK: true, cpu: cpuCounters{100, 40}},
				{},
				{cpuOK: true, cpu: cpuCounters{200, 80}, memoryOK: true, memoryTotal: 1000, memoryAvailable: 0},
				{cpuOK: true, cpu: cpuCounters{220, 100}},
			},
			closeAt: 14 * time.Minute,
			want:    []Summary{{Hour: hour, SegmentID: segment, LogicalCores: 4, Partial: true, SampleCount: 3, MemoryTotalBytes: 1000, MemorySampleCount: 1, CPUSampleCount: 1}},
		},
		{
			name:  "CPU sums and maximum survive hour rollover",
			times: []time.Duration{59 * time.Minute, time.Hour, time.Hour + time.Minute},
			samples: []reading{
				{cpuOK: true, cpu: cpuCounters{100, 40}},
				{cpuOK: true, cpu: cpuCounters{120, 50}},
				{cpuOK: true, cpu: cpuCounters{140, 55}},
			},
			closeAt: time.Hour + 2*time.Minute,
			want: []Summary{
				{Hour: hour, SegmentID: segment, LogicalCores: 4, SampleCount: 1},
				{Hour: hour.Add(time.Hour), SegmentID: segment, LogicalCores: 4, Partial: true, SampleCount: 2, CPUBusySumPercent: 125, CPUBusyMaxPercent: 75, CPUSampleCount: 2},
			},
		},
		{
			name:    "macOS pressure counts never populate PSI",
			times:   []time.Duration{10 * time.Minute, 11 * time.Minute, 12 * time.Minute, 13 * time.Minute},
			samples: []reading{{pressureOK: true, pressureLevel: 1}, {pressureOK: true, pressureLevel: 2}, {}, {pressureOK: true, pressureLevel: 4}},
			closeAt: 14 * time.Minute,
			want:    []Summary{{Hour: hour, SegmentID: segment, LogicalCores: 4, Partial: true, SampleCount: 3, PressureSampleCount: 3, PressureWarnCount: 1, PressureCriticalCount: 1}},
		},
		{
			name:  "unsupported tick still closes preceding hour",
			times: []time.Duration{59 * time.Minute, time.Hour}, samples: []reading{memory, {}}, closeAt: time.Hour + time.Minute,
			want: []Summary{{Hour: hour, SegmentID: segment, LogicalCores: 4, SampleCount: 1, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 600, MemoryAvailableSumBytes: 600, MemorySampleCount: 1}},
		},
		{
			name:  "shutdown after boundary closes completed hour",
			times: []time.Duration{59 * time.Minute}, samples: []reading{memory}, closeAt: time.Hour,
			want: []Summary{{Hour: hour, SegmentID: segment, LogicalCores: 4, SampleCount: 1, MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 600, MemoryAvailableSumBytes: 600, MemorySampleCount: 1}},
		},
		{name: "all unsupported", times: []time.Duration{time.Minute}, samples: []reading{{}}, closeAt: 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Collector{current: Summary{SegmentID: segment, LogicalCores: 4}}
			for i, sample := range test.samples {
				c.record(hour.Add(test.times[i]).In(time.FixedZone("local", -5*3600)), sample)
			}
			got := c.Close(hour.Add(test.closeAt))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("summaries = %+v, want %+v", got, test.want)
			}
			if again := c.Close(hour.Add(test.closeAt)); !reflect.DeepEqual(again, test.want) {
				t.Fatalf("second close duplicated records: %+v", again)
			}
		})
	}
}

func TestCollectorLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		duration time.Duration
		count    uint64
	}{
		{"initial sample survives immediate stop", 0, 1},
		{"30 second background cadence", 65 * time.Second, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				c := &Collector{read: func(context.Context) reading { return reading{memoryOK: true, memoryTotal: 100, memoryAvailable: 50} }}
				done := make(chan struct{})
				go func() { c.Run(ctx); close(done) }()
				synctest.Wait()
				time.Sleep(test.duration)
				cancel()
				<-done
				got := c.Close(time.Now())
				if len(got) != 1 || got[0].SampleCount != test.count || !got[0].Partial {
					t.Fatalf("shutdown summaries = %+v, want one partial with %d samples", got, test.count)
				}
			})
		})
	}
}

func TestSummaryRetentionAndAcknowledgement(t *testing.T) {
	hour := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	c := &Collector{current: Summary{SegmentID: hour}}
	for i := range retainedHours + 2 {
		c.record(hour.Add(time.Duration(i)*time.Hour), reading{memoryOK: true, memoryTotal: 100, memoryAvailable: 50})
	}
	pending := c.Summaries()
	if len(pending) != retainedHours || !pending[0].Hour.Equal(hour.Add(time.Hour)) {
		t.Fatalf("retained summaries = %d, first = %v", len(pending), pending[0].Hour)
	}
	c.Close(hour.Add((retainedHours + 2) * time.Hour))
	c.Acknowledge(pending)
	remaining := c.Summaries()
	if len(remaining) != 1 || !remaining[0].Hour.Equal(hour.Add((retainedHours+1)*time.Hour)) {
		t.Fatalf("acknowledgement lost newly closed record: %+v", remaining)
	}
	remaining[0].SampleCount = 999
	if c.Summaries()[0].SampleCount != 1 {
		t.Fatal("caller mutated retained summary")
	}
}

func TestGracefulRestartSegments(t *testing.T) {
	hour := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	var summaries []Summary
	for _, offset := range []time.Duration{time.Minute, 30 * time.Minute} {
		c := New(t.TempDir(), hour.Add(offset))
		c.record(hour.Add(offset), reading{memoryOK: true, memoryTotal: 100, memoryAvailable: 50})
		summaries = append(summaries, c.Close(hour.Add(offset+time.Minute))...)
	}
	if len(summaries) != 2 || !summaries[0].Hour.Equal(summaries[1].Hour) || summaries[0].SegmentID.Equal(summaries[1].SegmentID) || summaries[0].MemorySampleCount+summaries[1].MemorySampleCount != 2 || summaries[0].MemoryAvailableSumBytes+summaries[1].MemoryAvailableSumBytes != 100 {
		t.Fatalf("restart lost mergeable segments: %+v", summaries)
	}
}

func BenchmarkSample(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	sample := readHost(ctx, root)
	if !sample.memoryOK || !sample.cpuOK || !sample.swapOK || !sample.loadOK || !sample.diskOK {
		b.Fatal("benchmark requires working memory, CPU, swap, load and disk readings")
	}
	c := &Collector{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c.record(time.Now(), readHost(ctx, root))
	}
}
