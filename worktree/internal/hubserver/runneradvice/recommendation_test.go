package runneradvice

import (
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice/advicetest"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func recommendationFixture() ([]Hour, []Run) {
	hours := make([]Hour, 0, 24)
	for _, summary := range advicetest.Summaries() {
		hours = append(hours, Hour{Summary: summary})
	}
	runs := make([]Run, 0, 10)
	for i, usage := range advicetest.Usages() {
		runs = append(runs, Run{StartedAt: advicetest.Start().Add(time.Duration(i) * time.Hour), Usage: usage})
	}
	return hours, runs
}

func TestRunnerSlotRecommendation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		period   string
		change   func([]Hour, []Run) ([]Hour, []Run)
		want     *SlotRange
		hours    float64
		runs     int
		pressure int
		disk     uint64
	}{
		{"dedicated", "dedicated", nil, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"shared day", "person_hours", nil, &SlotRange{12, 12}, 24, 10, 0, 0},
		{"shared night", "free_hours", nil, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"warn pressure", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PressureWarnCount = 1
			h[0].Slots = 6
			return h, r
		}, &SlotRange{5, 5}, 24, 10, 1, 0},
		{"critical pressure", "person_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PressureCriticalCount = 1
			h[0].Slots = 4
			return h, r
		}, &SlotRange{3, 3}, 24, 10, 1, 0},
		{"PSI above ten", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PSISomeAvg10Sum = 1320
			h[0].PSISomeSampleCount = 120
			h[0].Slots = 3
			return h, r
		}, &SlotRange{2, 2}, 24, 10, 1, 0},
		{"PSI exactly ten", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PSISomeAvg10Sum = 1200
			h[0].PSISomeSampleCount = 120
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap idle", "free_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 0
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap fractional", "free_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 0.5
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap single slot", "free_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 1
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap full slots", "free_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 3
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"isolated swap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"idle warn", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PressureWarnCount = 1
			h[0].Slots = 0
			return h, r
		}, &SlotRange{13, 13}, 24, 10, 1, 0},
		{"fractional critical", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].PressureCriticalCount = 1
			h[0].Slots = 0.5
			return h, r
		}, &SlotRange{13, 13}, 24, 10, 1, 0},
		{"sampled swap growth", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &SlotRange{13, 13}, 24, 10, 1, 0},
		{"swap growth below threshold", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = 2*CapacityGiB - 1
			h[1].SwapSampleCount = 120
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"declining swap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = 2 * CapacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth across a gap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			for i := 1; i < len(h); i++ {
				h[i].Hour = h[i].Hour.Add(time.Hour)
			}
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth without previous sample", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 0
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth without current sample", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = CapacityGiB
			h[1].SwapSampleCount = 0
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"swap near counter maximum", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].SwapUsedMaxBytes = ^uint64(0) - 1
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = ^uint64(0)
			h[1].SwapSampleCount = 120
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"propagated swap growth", "free_hours", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].OverCapacity = true
			return h, r
		}, &SlotRange{13, 13}, 24, 10, 1, 0},
		{"CPU exhaustion", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].LogicalCores = 1
			return h, r
		}, &SlotRange{0, 0}, 24, 10, 0, 0},
		{"memory exhaustion", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].MemoryTotalBytes = 2 * CapacityGiB
			h[0].MemoryAvailableMinBytes = CapacityGiB
			return h, r
		}, &SlotRange{0, 0}, 24, 10, 0, 0},
		{"pressure at one occupied slot", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].Slots = 1
			h[0].PressureWarnCount = 1
			return h, r
		}, &SlotRange{0, 0}, 24, 10, 1, 0},
		{"pressure consumes last resource slot", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].LogicalCores = 2
			h[0].PressureCriticalCount = 1
			return h, r
		}, &SlotRange{0, 0}, 24, 10, 1, 0},
		{"hours insufficient", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			return h[:23], r
		}, nil, 23, 10, 0, 0},
		{"runs insufficient", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			return h, r[:9]
		}, nil, 24, 9, 0, 0},
		{"partial hours", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			for i := range h {
				h[i].MemorySampleCount = 60
			}
			return h, r
		}, nil, 12, 10, 0, 0},
		{"disk below reserve", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].DiskFreeMinBytes = 20 * CapacityGiB
			return h, r
		}, &SlotRange{0, 0}, 24, 10, 0, 0},
		{"CPU cap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			for i := range r {
				r[i].Usage.UserCPUSeconds = 190
			}
			return h, r
		}, &SlotRange{7, 7}, 24, 10, 0, 0},
		{"measured headroom range", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].MemoryAvailableMinBytes = 24 * CapacityGiB
			h[0].Slots = 2
			return h, r
		}, &SlotRange{8, 14}, 24, 10, 0, 0},
		{"measured disk cap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[1].DiskFreeMinBytes = 280 * CapacityGiB
			h[1].Slots = 4
			return h, r
		}, &SlotRange{10, 10}, 24, 10, 0, 30 * CapacityGiB},
		{"idle disk growth before a lease", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			for i := 1; i < len(h); i++ {
				h[i].DiskFreeMinBytes = 280 * CapacityGiB
			}
			h[2].Slots = 1
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"first disk observation occupied", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[0].DiskFreeMinBytes = 280 * CapacityGiB
			h[0].Slots = 1
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"disk growth across a gap", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			for i := 1; i < len(h); i++ {
				h[i].Hour = h[i].Hour.Add(time.Hour)
				h[i].DiskFreeMinBytes = 280 * CapacityGiB
			}
			h[1].Slots = 1
			return h, r
		}, &SlotRange{14, 14}, 24, 10, 0, 0},
		{"disk cap preserves unrelated background usage", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			h[1].DiskFreeMinBytes = 140 * CapacityGiB
			h[2].DiskFreeMinBytes = 100 * CapacityGiB
			h[2].Slots = 2
			return h, r
		}, &SlotRange{3, 3}, 24, 10, 0, 20 * CapacityGiB},
		{"warn without disk", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.DiskSampleCount = 0
			hour.PressureWarnCount = 1
			hour.Slots = 6
			return append(h, hour), r
		}, &SlotRange{5, 5}, 24, 10, 1, 0},
		{"critical without CPU", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.CPUSampleCount = 0
			hour.PressureCriticalCount = 1
			hour.Slots = 4
			return append(h, hour), r
		}, &SlotRange{3, 3}, 24, 10, 1, 0},
		{"PSI without memory", "dedicated", func(h []Hour, r []Run) ([]Hour, []Run) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.MemorySampleCount = 0
			hour.PSISomeSampleCount = 1
			hour.PSISomeAvg10Sum = 11
			hour.Slots = 3
			return append(h, hour), r
		}, &SlotRange{2, 2}, 24, 10, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hours, runs := recommendationFixture()
			if test.change != nil {
				hours, runs = test.change(hours, runs)
			}
			peaks, cores := []float64{}, []float64{}
			for _, run := range runs {
				peaks = append(peaks, float64(run.Usage.PeakMemoryBytes))
				cores = append(cores, (run.Usage.UserCPUSeconds+run.Usage.SystemCPUSeconds)/run.Usage.WallSeconds)
			}
			got := recommendPeriod(test.period, hours, peaks, cores)
			if !reflect.DeepEqual(got.Slots, test.want) || got.HoursCovered != test.hours || got.RunsObserved != test.runs || got.OverCapacityHours != test.pressure || got.DiskBytesPerSlot != test.disk {
				t.Fatalf("recommendation=%+v slots=%+v, want slots=%+v hours=%v runs=%d pressure=%d disk=%d", got, got.Slots, test.want, test.hours, test.runs, test.pressure, test.disk)
			}
			if test.want == nil && got.Status != "insufficient_evidence" || test.want != nil && got.Status != "recommended" {
				t.Fatalf("status=%q", got.Status)
			}
			if got.P90PeakMemoryBytes != 4*CapacityGiB {
				t.Fatalf("p90 peak=%d, want 4294967296", got.P90PeakMemoryBytes)
			}
		})
	}
}

func TestRunnerRecommendationPeriods(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		routing runnerauth.Routing
		at      string
		person  bool
	}{
		{"default morning", runnerauth.Routing{}, "2026-09-14T08:00:00Z", true},
		{"default evening end", runnerauth.Routing{}, "2026-09-14T22:00:00Z", false},
		{"runner timezone", runnerauth.Routing{Availability: runnerauth.Availability{Timezone: "America/Chicago"}}, "2026-09-14T12:00:00Z", false},
		{"timezone day", runnerauth.Routing{Availability: runnerauth.Availability{Timezone: "America/Chicago"}}, "2026-09-14T13:00:00Z", true},
		{"lower slot window", runnerauth.Routing{CapacityLimit: 8, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 10:00-12:00"}, WindowSlots: map[string]int{"Mon-Fri 10:00-12:00": 2}}}, "2026-09-14T09:00:00Z", false},
		{"lower window open", runnerauth.Routing{CapacityLimit: 8, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 10:00-12:00"}, WindowSlots: map[string]int{"Mon-Fri 10:00-12:00": 2}}}, "2026-09-14T10:00:00Z", true},
		{"equal slot window uses default", runnerauth.Routing{CapacityLimit: 8, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 10:00-12:00"}, WindowSlots: map[string]int{"Mon-Fri 10:00-12:00": 8}}}, "2026-09-14T09:00:00Z", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, test.at)
			if err != nil {
				t.Fatal(err)
			}
			got, err := personHour(test.routing, at)
			if err != nil || got != test.person {
				t.Fatalf("person=%t err=%v, want %t", got, err, test.person)
			}
		})
	}
	t.Run("host hour overlaps a short person window", func(t *testing.T) {
		routing := runnerauth.Routing{CapacityLimit: 8, Availability: runnerauth.Availability{Timezone: "UTC", WindowSlots: map[string]int{"Mon-Fri 10:15-10:45": 2}}}
		at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
		got, err := personHostHour(routing, at)
		if err != nil || !got {
			t.Fatalf("person host hour=%t error=%v", got, err)
		}
	})
	baseHours, baseRuns := recommendationFixture()
	hours, runs := []Hour{}, []Run{}
	for i := range 72 {
		hour := baseHours[0]
		hour.Hour = hour.Hour.Add(time.Duration(i) * time.Hour)
		hours = append(hours, hour)
	}
	for i := range 20 {
		run := baseRuns[0]
		run.StartedAt = run.StartedAt.Add(time.Duration(i%3) * 24 * time.Hour)
		if i < 10 {
			run.StartedAt = run.StartedAt.Add(8 * time.Hour)
		}
		runs = append(runs, run)
	}
	for _, machineUse := range []string{"", "shared", "dedicated"} {
		t.Run("partition "+machineUse, func(t *testing.T) {
			out, err := Recommend(runnerauth.Routing{MachineUse: machineUse}, hours, runs)
			if err != nil {
				t.Fatal(err)
			}
			if machineUse == "dedicated" {
				if len(out.Periods) != 1 || out.Periods[0].Period != "dedicated" || out.Periods[0].HoursCovered != 72 || out.Periods[0].RunsObserved != 20 || out.Periods[0].Slots == nil || *out.Periods[0].Slots != (SlotRange{14, 14}) {
					t.Fatalf("periods=%+v", out.Periods)
				}
			} else if len(out.Periods) != 2 || out.Periods[0].Period != "person_hours" || out.Periods[0].HoursCovered != 42 || out.Periods[0].RunsObserved != 10 || out.Periods[0].Slots == nil || *out.Periods[0].Slots != (SlotRange{12, 12}) || out.Periods[1].Period != "free_hours" || out.Periods[1].HoursCovered != 30 || out.Periods[1].RunsObserved != 10 || out.Periods[1].Slots == nil || *out.Periods[1].Slots != (SlotRange{14, 14}) {
				t.Fatalf("periods=%+v", out.Periods)
			}
		})
	}
}

func TestRunnerSuggestedLimit(t *testing.T) {
	t.Parallel()
	studioCPU := []float64{68, 62, 48, 55, 77, 100, 100, 99, 100, 98, 100, 100, 100, 100, 94, 95, 87, 34, 37, 45, 34, 33, 30, 36, 70, 61, 28, 69, 77, 67, 63, 53, 39, 39, 29, 16, 31, 22, 36, 48, 81, 63, 87, 60, 77, 80, 75, 81, 65, 74, 69, 76, 73, 58, 73, 62, 55, 60, 68, 68, 63, 68, 78, 73, 69, 69, 83, 77, 80, 77, 76, 84, 71, 93, 88, 91, 68, 62, 70, 56, 47, 36, 31, 39, 41, 54, 63, 50, 67, 73, 42}
	studioWarn := "1110000000000000111100010000000000000000000000000000000000000000000000000000000000000000000"
	hostHours := func(n int, edit func(int, *Hour)) []Hour {
		start := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
		hours := make([]Hour, n)
		for i := range hours {
			hours[i] = Hour{Summary: hostmetrics.Summary{
				Hour: start.Add(time.Duration(i) * time.Hour), LogicalCores: 16,
				MemoryTotalBytes: 128 * CapacityGiB, MemoryAvailableMinBytes: 48 * CapacityGiB, MemorySampleCount: 120,
				CPUBusySumPercent: 40 * 120, CPUSampleCount: 120,
				DiskFreeMinBytes: 600 * CapacityGiB, DiskTotalMinBytes: 2000 * CapacityGiB, DiskSampleCount: 120,
			}, Slots: 4}
			if edit != nil {
				edit(i, &hours[i])
			}
		}
		return hours
	}
	for _, test := range []struct {
		name       string
		timezone   string
		machineUse string
		current    int
		hours      []Hour
		direction  string
		limit      int
		reasons    []string
	}{
		{"studio overload lowers to clean level with disk reason", "UTC", "", 8, hostHours(len(studioCPU), func(i int, h *Hour) {
			h.CPUBusySumPercent = studioCPU[i] * 120
			h.Slots = 6
			if studioCPU[i] >= 90 || studioWarn[i] == '1' {
				h.Slots = 8
			}
			if studioWarn[i] == '1' {
				h.PressureWarnCount = 10
			}
			if i == 17 {
				h.DiskFreeMinBytes = 0
			}
		}), "lower", 6, []string{"CPU stayed at 90% or more for 2+ hours running in 11 of 91 busy hours.", "Memory pressure or growing swap in 8 of 91 busy hours.", "Free disk fell to 0.0 GB on Oct 11."}},
		{"memory pressure alone lowers by one without a clean level", "UTC", "", 8, hostHours(48, func(i int, h *Hour) {
			h.Slots = 8
			if i%4 == 0 {
				h.PressureCriticalCount = 1
			}
		}), "lower", 7, []string{"Memory pressure or growing swap in 12 of 48 busy hours."}},
		{"swap growth counts as memory saturation", "UTC", "", 6, hostHours(48, func(i int, h *Hour) {
			h.SwapSampleCount = 120
			h.SwapUsedMaxBytes = uint64(i) * 2 * CapacityGiB
		}), "lower", 5, []string{"Memory pressure or growing swap in 47 of 48 busy hours."}},
		{"unsampled previous swap hour is not growth", "UTC", "", 6, hostHours(48, func(i int, h *Hour) {
			h.SwapUsedMaxBytes = 3 * CapacityGiB
			if i%2 == 1 {
				h.SwapSampleCount = 120
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours."}},
		{"isolated hot CPU hour is not saturation", "UTC", "", 6, hostHours(48, func(i int, h *Hour) {
			if i%2 == 0 {
				h.CPUBusySumPercent = 95 * 120
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours."}},
		{"pressure while idle is not saturation", "UTC", "", 6, hostHours(48, func(_ int, h *Hour) {
			h.Slots = 0
			h.PressureWarnCount = 5
		}), "hold", 6, []string{"No work ran in the 48 covered hours."}},
		{"saturation counts against busy hours only", "UTC", "", 6, hostHours(48, func(i int, h *Hour) {
			h.Slots = 0
			if i < 4 {
				h.Slots = 4
				h.PressureWarnCount = 1
			}
		}), "lower", 5, []string{"Memory pressure or growing swap in 4 of 4 busy hours."}},
		{"saturated at one job holds at the minimum", "UTC", "", 1, hostHours(48, func(_ int, h *Hour) {
			h.Slots = 1
			h.PressureCriticalCount = 1
		}), "hold", 1, []string{"Memory pressure or growing swap in 48 of 48 busy hours.", "It already runs the minimum of 1 job."}},
		{"raise stops at the routing maximum", "UTC", "", 10000, hostHours(48, func(_ int, h *Hour) { h.Slots = 10000 }), "hold", 10000, []string{"Saturated in 0 of 48 busy hours."}},
		{"low disk reports the runner-local date without changing the limit", "America/Chicago", "", 6, hostHours(48, func(i int, h *Hour) {
			if i == 30 {
				h.DiskFreeMinBytes = 12 * CapacityGiB
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours.", "Free disk fell to 12.9 GB on Oct 11."}},
		{"low disk counts without CPU samples", "UTC", "", 6, hostHours(48, func(i int, h *Hour) {
			if i == 47 {
				h.CPUSampleCount = 0
				h.DiskFreeMinBytes = CapacityGiB
			}
		}), "hold", 6, []string{"Saturated in 0 of 47 busy hours.", "Free disk fell to 1.1 GB on Oct 12."}},
		{"full slots without saturation raise by one", "UTC", "", 4, hostHours(40, func(i int, h *Hour) {
			if i%10 < 3 {
				h.Slots = 3.8
			} else {
				h.Slots = 1
			}
		}), "raise", 5, []string{"Every slot was busy in 12 of 40 hours without saturation."}},
		{"idle host never raises", "UTC", "", 4, hostHours(72, func(_ int, h *Hour) { h.Slots = 0 }), "hold", 4, []string{"No work ran in the 72 covered hours."}},
		{"shared runner ignores saturation in person hours", "UTC", "shared", 6, hostHours(72, func(_ int, h *Hour) {
			if hour := h.Hour.Hour(); hour >= 8 && hour < 22 {
				h.PressureWarnCount = 1
			}
		}), "hold", 6, []string{"Saturated in 0 of 30 busy hours."}},
		{"dedicated runner counts the same person-hour saturation", "UTC", "dedicated", 6, hostHours(72, func(_ int, h *Hour) {
			if hour := h.Hour.Hour(); hour >= 8 && hour < 22 {
				h.PressureWarnCount = 1
			}
		}), "lower", 5, []string{"Memory pressure or growing swap in 42 of 72 busy hours."}},
		{"shared runner keeps disk and adjacency from person hours", "UTC", "shared", 6, hostHours(72, func(_ int, h *Hour) {
			hour := h.Hour.Hour()
			if hour == 13 {
				h.DiskFreeMinBytes = CapacityGiB
			}
			if hour == 21 || hour == 22 {
				h.CPUBusySumPercent = 95 * 120
			}
		}), "lower", 5, []string{"CPU stayed at 90% or more for 2+ hours running in 3 of 30 busy hours.", "Free disk fell to 1.1 GB on Oct 11."}},
		{"shared runner needs 24 free hours", "UTC", "shared", 6, hostHours(48, nil), "insufficient_evidence", -1, []string{"20 of the 24 covered hours needed so far."}},
		{"stopped runner holds", "UTC", "", 0, hostHours(48, nil), "hold", 0, []string{"The runner is stopped at 0 jobs at once."}},
		{"uncovered hours do not count", "UTC", "", 6, hostHours(30, func(i int, h *Hour) {
			if i < 10 {
				h.CPUSampleCount = 0
			}
		}), "insufficient_evidence", -1, []string{"20 of the 24 covered hours needed so far."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := suggestLimit(test.current, test.timezone, test.hours, nil)
			if test.machineUse != "" {
				out, err := Recommend(runnerauth.Routing{CapacityLimit: test.current, MachineUse: test.machineUse, Availability: runnerauth.Availability{Timezone: test.timezone}}, test.hours, nil)
				if err != nil {
					t.Fatal(err)
				}
				got = out.Suggested
			}
			limit := -1
			if got.Limit != nil {
				limit = *got.Limit
			}
			if got.Direction != test.direction || limit != test.limit || got.Current != test.current || !reflect.DeepEqual(got.Reasons, test.reasons) {
				t.Fatalf("suggestion=%+v limit=%d, want direction=%s limit=%d reasons=%q", got, limit, test.direction, test.limit, test.reasons)
			}
		})
	}
}

func TestRunnerRecommendationLeaseHistoryCost(t *testing.T) {
	hours, _ := recommendationFixture()
	from := hours[0].Hour
	cases := []struct {
		name       string
		start, end time.Time
	}{
		{"before window", from.Add(-30 * time.Minute), from.Add(30 * time.Minute)},
		{"late release", from.Add(15 * time.Minute), from.Add(90 * time.Minute)},
		{"early release", from.Add(4 * time.Hour), from.Add(5 * time.Hour)},
		{"expired before window", from.Add(-2 * time.Hour), from.Add(-time.Hour)},
		{"ends at window start", from.Add(-time.Hour), from},
		{"begins at window end", from.Add(24 * time.Hour), from.Add(25 * time.Hour)},
	}
	facts := reportstore.RunnerCapacityFacts{}
	for _, c := range cases {
		facts.Occupancy = append(facts.Occupancy, reportstore.RunnerOccupancy{LeaseID: c.name, From: c.start, To: c.end})
	}
	if _, err := FromFacts(runnerauth.Routing{CapacityLimit: 4, MachineUse: "dedicated"}, hours, facts); err != nil {
		t.Fatal(err)
	}
	for i, hour := range hours {
		want := float64(0)
		switch i {
		case 0:
			want = 1.25
		case 1:
			want = .5
		case 4:
			want = 1
		}
		if hour.Slots != want {
			t.Fatalf("hour %d slot-hours=%v,want %v", i, hour.Slots, want)
		}
	}
}
