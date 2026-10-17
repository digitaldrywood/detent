package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func TestRunnerSlotRecommendation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		period   string
		change   func([]recommendationHour, []recommendationRun) ([]recommendationHour, []recommendationRun)
		want     *slotRange
		hours    float64
		runs     int
		pressure int
		disk     uint64
	}{
		{"dedicated", "dedicated", nil, &slotRange{14, 14}, 24, 10, 0, 0},
		{"shared day", "person_hours", nil, &slotRange{12, 12}, 24, 10, 0, 0},
		{"shared night", "free_hours", nil, &slotRange{14, 14}, 24, 10, 0, 0},
		{"warn pressure", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureWarnCount = 1
			h[0].Slots = 6
			return h, r
		}, &slotRange{5, 5}, 24, 10, 1, 0},
		{"critical pressure", "person_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureCriticalCount = 1
			h[0].Slots = 4
			return h, r
		}, &slotRange{3, 3}, 24, 10, 1, 0},
		{"PSI above ten", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PSISomeAvg10Sum = 1320
			h[0].PSISomeSampleCount = 120
			h[0].Slots = 3
			return h, r
		}, &slotRange{2, 2}, 24, 10, 1, 0},
		{"PSI exactly ten", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PSISomeAvg10Sum = 1200
			h[0].PSISomeSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap idle", "free_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 0
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap fractional", "free_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 0.5
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap single slot", "free_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 1
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"unchanged swap full slots", "free_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			h[1].Slots = 3
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"isolated swap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"idle warn", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureWarnCount = 1
			h[0].Slots = 0
			return h, r
		}, &slotRange{13, 13}, 24, 10, 1, 0},
		{"fractional critical", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureCriticalCount = 1
			h[0].Slots = 0.5
			return h, r
		}, &slotRange{13, 13}, 24, 10, 1, 0},
		{"sampled swap growth", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &slotRange{13, 13}, 24, 10, 1, 0},
		{"swap growth below threshold", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = 2*capacityGiB - 1
			h[1].SwapSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"declining swap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = 2 * capacityGiB
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth across a gap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			for i := 1; i < len(h); i++ {
				h[i].Hour = h[i].Hour.Add(time.Hour)
			}
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth without previous sample", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 0
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"swap growth without current sample", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = 0
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].SwapSampleCount = 0
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"swap near counter maximum", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = ^uint64(0) - 1
			h[0].SwapSampleCount = 120
			h[1].SwapUsedMaxBytes = ^uint64(0)
			h[1].SwapSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"propagated swap growth", "free_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].OverCapacity = true
			return h, r
		}, &slotRange{13, 13}, 24, 10, 1, 0},
		{"CPU exhaustion", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].LogicalCores = 1
			return h, r
		}, &slotRange{0, 0}, 24, 10, 0, 0},
		{"memory exhaustion", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].MemoryTotalBytes = 2 * capacityGiB
			h[0].MemoryAvailableMinBytes = capacityGiB
			return h, r
		}, &slotRange{0, 0}, 24, 10, 0, 0},
		{"pressure at one occupied slot", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].Slots = 1
			h[0].PressureWarnCount = 1
			return h, r
		}, &slotRange{0, 0}, 24, 10, 1, 0},
		{"pressure consumes last resource slot", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].LogicalCores = 2
			h[0].PressureCriticalCount = 1
			return h, r
		}, &slotRange{0, 0}, 24, 10, 1, 0},
		{"hours insufficient", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			return h[:23], r
		}, nil, 23, 10, 0, 0},
		{"runs insufficient", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			return h, r[:9]
		}, nil, 24, 9, 0, 0},
		{"partial hours", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := range h {
				h[i].MemorySampleCount = 60
			}
			return h, r
		}, nil, 12, 10, 0, 0},
		{"disk below reserve", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].DiskFreeMinBytes = 20 * capacityGiB
			return h, r
		}, &slotRange{0, 0}, 24, 10, 0, 0},
		{"CPU cap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := range r {
				r[i].Usage.UserCPUSeconds = 190
			}
			return h, r
		}, &slotRange{7, 7}, 24, 10, 0, 0},
		{"measured headroom range", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].MemoryAvailableMinBytes = 24 * capacityGiB
			h[0].Slots = 2
			return h, r
		}, &slotRange{8, 14}, 24, 10, 0, 0},
		{"measured disk cap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[1].DiskFreeMinBytes = 280 * capacityGiB
			h[1].Slots = 4
			return h, r
		}, &slotRange{10, 10}, 24, 10, 0, 30 * capacityGiB},
		{"idle disk growth before a lease", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := 1; i < len(h); i++ {
				h[i].DiskFreeMinBytes = 280 * capacityGiB
			}
			h[2].Slots = 1
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"first disk observation occupied", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].DiskFreeMinBytes = 280 * capacityGiB
			h[0].Slots = 1
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"disk growth across a gap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := 1; i < len(h); i++ {
				h[i].Hour = h[i].Hour.Add(time.Hour)
				h[i].DiskFreeMinBytes = 280 * capacityGiB
			}
			h[1].Slots = 1
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0, 0},
		{"disk cap preserves unrelated background usage", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[1].DiskFreeMinBytes = 140 * capacityGiB
			h[2].DiskFreeMinBytes = 100 * capacityGiB
			h[2].Slots = 2
			return h, r
		}, &slotRange{3, 3}, 24, 10, 0, 20 * capacityGiB},
		{"warn without disk", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.DiskSampleCount = 0
			hour.PressureWarnCount = 1
			hour.Slots = 6
			return append(h, hour), r
		}, &slotRange{5, 5}, 24, 10, 1, 0},
		{"critical without CPU", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.CPUSampleCount = 0
			hour.PressureCriticalCount = 1
			hour.Slots = 4
			return append(h, hour), r
		}, &slotRange{3, 3}, 24, 10, 1, 0},
		{"PSI without memory", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			hour := h[len(h)-1]
			hour.Hour = hour.Hour.Add(time.Hour)
			hour.MemorySampleCount = 0
			hour.PSISomeSampleCount = 1
			hour.PSISomeAvg10Sum = 11
			hour.Slots = 3
			return append(h, hour), r
		}, &slotRange{2, 2}, 24, 10, 1, 0},
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
			if got.P90PeakMemoryBytes != 4*capacityGiB {
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
	hours, runs := []recommendationHour{}, []recommendationRun{}
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
			out, err := recommendRunner(runnerauth.Routing{MachineUse: machineUse}, hours, runs)
			if err != nil {
				t.Fatal(err)
			}
			if machineUse == "dedicated" {
				if len(out.Periods) != 1 || out.Periods[0].Period != "dedicated" || out.Periods[0].HoursCovered != 72 || out.Periods[0].RunsObserved != 20 || out.Periods[0].Slots == nil || *out.Periods[0].Slots != (slotRange{14, 14}) {
					t.Fatalf("periods=%+v", out.Periods)
				}
			} else if len(out.Periods) != 2 || out.Periods[0].Period != "person_hours" || out.Periods[0].HoursCovered != 42 || out.Periods[0].RunsObserved != 10 || out.Periods[0].Slots == nil || *out.Periods[0].Slots != (slotRange{12, 12}) || out.Periods[1].Period != "free_hours" || out.Periods[1].HoursCovered != 30 || out.Periods[1].RunsObserved != 10 || out.Periods[1].Slots == nil || *out.Periods[1].Slots != (slotRange{14, 14}) {
				t.Fatalf("periods=%+v", out.Periods)
			}
		})
	}
}

func TestRunnerCapacityRecommendationReadOnly(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	other := sharedRunner(t, r)
	issue := f.create(t, "capacity evidence")
	hours, runs := recommendationFixture()
	for _, runnerID := range []string{r.binding.RunnerID, other.binding.RunnerID} {
		for i, hour := range hours {
			hour.SwapSampleCount = 120
			hour.SwapUsedMaxBytes = capacityGiB
			if runnerID == other.binding.RunnerID && i > 0 {
				hour.SwapUsedMaxBytes = 2 * capacityGiB
			}
			raw, err := json.Marshal(hour.Summary)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?, '[]')`, f.project.OrganizationID, runnerID, formatHubTime(hour.Hour), string(raw)); err != nil {
				t.Fatal(err)
			}
		}
		hostHours, err := readRecommendationHostHours(t.Context(), f.service.database.db, runnerauth.Runner{OrganizationID: f.project.OrganizationID, Binding: runnerauth.Binding{RunnerID: runnerID}}, hours[0].Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(hostHours) != 24 {
			t.Fatalf("runner %s has %d host hours, want 24", runnerID, len(hostHours))
		}
		for i, hour := range hostHours {
			wantPressure := runnerID == other.binding.RunnerID && i == 1
			if hour.OverCapacity != wantPressure {
				t.Errorf("runner %s hour %d pressure=%t, want %t", runnerID, i, hour.OverCapacity, wantPressure)
			}
		}
	}
	for i := range 14 {
		run := runs[i%10]
		runnerID := r.binding.RunnerID
		if i == 13 {
			runnerID = other.binding.RunnerID
		}
		status := "succeeded"
		if i == 0 {
			status = "failed"
		}
		if i == 1 {
			status = "cancelled"
		}
		if i >= 10 {
			run.Usage.PeakMemoryBytes = 128 * capacityGiB
		}
		if i == 10 {
			status = "running"
		}
		if i == 11 {
			run.StartedAt = now.Add(-721 * time.Hour)
		}
		if i == 12 {
			status = "interrupted"
		}
		lease := fmt.Sprintf("lease_capacity_%d", i)
		at, end := formatHubTime(run.StartedAt), formatHubTime(run.StartedAt.Add(time.Hour))
		var fence int64
		if err := f.service.database.db.QueryRowContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,id,?,?,?,?,?,?,?,? FROM issues WHERE native_id=? RETURNING fencing_token`, lease, r.binding.MachineID, fmt.Sprintf("capacity_session_%d", i), end, at, at, end, at, end, issue.WorkItemID).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, lease, runnerID); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tracker.NativeRunData{ProcessUsage: &run.Usage})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?,?,?)`, fmt.Sprintf("attempt_capacity_%d", i), f.project.OrganizationID, f.project.ID, issue.WorkItemID, lease, fence, fmt.Sprintf("run_capacity_%d", i), status, string(raw), at, end); err != nil {
			t.Fatal(err)
		}
	}
	var before runnerauth.Runner
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &before)
	if before.MachineUse != "shared" {
		t.Fatalf("undeclared machine use=%q, want shared", before.MachineUse)
	}
	for _, machineUse := range []string{"dedicated", "shared"} {
		change := runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}
		change.MachineUse = machineUse
		response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &before)
		if before.MachineUse != machineUse {
			t.Fatalf("machine use=%q", before.MachineUse)
		}
		read := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity", testHubAdminToken, nil)
		requireNativeStatus(t, read, http.StatusOK)
		var out runnerCapacityRead
		decodeHubResponse(t, read, &out)
		if out.Desired != before.CapacityLimit || !out.Recommendation.Advisory || out.Recommendation.MachineUse != machineUse {
			t.Fatalf("capacity=%+v", out)
		}
		if machineUse == "dedicated" {
			p := out.Recommendation.Periods[0]
			if len(out.Recommendation.Periods) != 1 || p.HoursCovered != 24 || p.RunsObserved != 10 || p.SlotHours != 12 || p.Slots == nil || *p.Slots != (slotRange{14, 14}) {
				t.Fatalf("period=%+v", p)
			}
		} else if len(out.Recommendation.Periods) != 2 || out.Recommendation.Periods[0].Status != "insufficient_evidence" || out.Recommendation.Periods[1].Status != "insufficient_evidence" {
			t.Fatalf("periods=%+v", out.Recommendation.Periods)
		}
		var after runnerauth.Runner
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("capacity read changed runner: before=%+v after=%+v", before, after)
		}
		legacy := runnerauth.RoutingChange{ExpectedRevision: after.Revision, Routing: after.Routing}
		legacy.MachineUse = ""
		legacy.Routing = legacy.Normalized()
		response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &before)
		if before.MachineUse != machineUse {
			t.Fatalf("legacy edit reset designation: %+v", before.Routing)
		}
	}
}

func TestRunnerRecommendationLeaseHistoryCost(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read)
	r.enroll(t)
	issue := f.create(t, "lease history")
	db := f.service.database.db
	from := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	hours, _ := recommendationFixture()
	for _, hour := range hours {
		raw, err := json.Marshal(hour.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?, '[]')`, f.project.OrganizationID, r.binding.RunnerID, formatHubTime(hour.Hour), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, lease := range []struct {
		name     string
		start    string
		expires  string
		released string
	}{
		{"before window", "2026-09-13T23:30:00Z", "2026-09-14T00:30:00Z", ""},
		{"late release", "2026-09-14T00:15:00Z", "2026-09-14T01:30:00Z", "2026-09-14T02:30:00Z"},
		{"early release", "2026-09-14T04:00:00Z", "2026-09-14T06:00:00Z", "2026-09-14T05:00:00Z"},
		{"expired before window", "2026-09-13T22:00:00Z", "2026-09-13T23:00:00Z", "2026-09-14T04:00:00Z"},
		{"ends at window start", "2026-09-13T23:00:00Z", "2026-09-14T00:00:00Z", ""},
		{"begins at window end", "2026-09-15T00:00:00Z", "2026-09-15T01:00:00Z", ""},
	} {
		item := f.create(t, lease.name)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,id,?,?,?,?,?,nullif(?,''),?,? FROM issues WHERE native_id=?`, lease.name, r.binding.MachineID, lease.name, lease.expires, lease.start, lease.start, lease.released, lease.start, lease.expires, item.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, lease.name, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
	}
	previous := 0
	for _, history := range []int{1, 64, 256} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			for i := previous; i < history; i++ {
				at := formatHubTime(from.Add(-time.Duration(i+1) * 24 * time.Hour))
				lease := fmt.Sprintf("old_capacity_lease_%d", i)
				if _, err := db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,id,?,?,?,?,?,?,?,? FROM issues WHERE native_id=?`, lease, r.binding.MachineID, lease, at, at, at, at, at, at, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, lease, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			previous = history
			q := &runtimeReadQuery{nativeQueryer: db}
			got, err := readRecommendationHours(t.Context(), q, runnerauth.Runner{Binding: r.binding, OrganizationID: f.project.OrganizationID}, from, to)
			if err != nil {
				t.Fatal(err)
			}
			wantSlots := []float64{1.25, 0.5, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			if len(got) != len(wantSlots) {
				t.Fatalf("host hours=%d, want 24", len(got))
			}
			for i, hour := range got {
				if hour.Slots != wantSlots[i] {
					t.Fatalf("hour %d slot-hours=%v, want %v", i, hour.Slots, wantSlots[i])
				}
			}
			for i, statement := range q.statements {
				if !strings.Contains(statement, "FROM leases") {
					continue
				}
				rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, q.arguments[i]...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var plans []string
				for rows.Next() {
					var id, parent, unused int
					var plan string
					if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
						t.Fatal(err)
					}
					plans = append(plans, plan)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				plan := strings.Join(plans, "\n")
				t.Logf("lease query plan with %d historical rows:\n%s", history, plan)
				if !strings.Contains(plan, "machine_id=? AND <expr>>?") || strings.Contains(plan, "SCAN l") {
					t.Fatalf("lease read is not bounded by machine and overlap window:\n%s", plan)
				}
			}
		})
	}
}

func TestRunnerSuggestedLimit(t *testing.T) {
	t.Parallel()
	studioCPU := []float64{68, 62, 48, 55, 77, 100, 100, 99, 100, 98, 100, 100, 100, 100, 94, 95, 87, 34, 37, 45, 34, 33, 30, 36, 70, 61, 28, 69, 77, 67, 63, 53, 39, 39, 29, 16, 31, 22, 36, 48, 81, 63, 87, 60, 77, 80, 75, 81, 65, 74, 69, 76, 73, 58, 73, 62, 55, 60, 68, 68, 63, 68, 78, 73, 69, 69, 83, 77, 80, 77, 76, 84, 71, 93, 88, 91, 68, 62, 70, 56, 47, 36, 31, 39, 41, 54, 63, 50, 67, 73, 42}
	studioWarn := "1110000000000000111100010000000000000000000000000000000000000000000000000000000000000000000"
	hostHours := func(n int, edit func(int, *recommendationHour)) []recommendationHour {
		start := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
		hours := make([]recommendationHour, n)
		for i := range hours {
			hours[i] = recommendationHour{Summary: hostmetrics.Summary{
				Hour: start.Add(time.Duration(i) * time.Hour), LogicalCores: 16,
				MemoryTotalBytes: 128 * capacityGiB, MemoryAvailableMinBytes: 48 * capacityGiB, MemorySampleCount: 120,
				CPUBusySumPercent: 40 * 120, CPUSampleCount: 120,
				DiskFreeMinBytes: 600 * capacityGiB, DiskTotalMinBytes: 2000 * capacityGiB, DiskSampleCount: 120,
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
		hours      []recommendationHour
		direction  string
		limit      int
		reasons    []string
	}{
		{"studio overload lowers to clean level with disk reason", "UTC", "", 8, hostHours(len(studioCPU), func(i int, h *recommendationHour) {
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
		{"memory pressure alone lowers by one without a clean level", "UTC", "", 8, hostHours(48, func(i int, h *recommendationHour) {
			h.Slots = 8
			if i%4 == 0 {
				h.PressureCriticalCount = 1
			}
		}), "lower", 7, []string{"Memory pressure or growing swap in 12 of 48 busy hours."}},
		{"swap growth counts as memory saturation", "UTC", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			h.SwapSampleCount = 120
			h.SwapUsedMaxBytes = uint64(i) * 2 * capacityGiB
		}), "lower", 5, []string{"Memory pressure or growing swap in 47 of 48 busy hours."}},
		{"unsampled previous swap hour is not growth", "UTC", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			h.SwapUsedMaxBytes = 3 * capacityGiB
			if i%2 == 1 {
				h.SwapSampleCount = 120
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours."}},
		{"isolated hot CPU hour is not saturation", "UTC", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			if i%2 == 0 {
				h.CPUBusySumPercent = 95 * 120
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours."}},
		{"pressure while idle is not saturation", "UTC", "", 6, hostHours(48, func(_ int, h *recommendationHour) {
			h.Slots = 0
			h.PressureWarnCount = 5
		}), "hold", 6, []string{"No work ran in the 48 covered hours."}},
		{"saturation counts against busy hours only", "UTC", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			h.Slots = 0
			if i < 4 {
				h.Slots = 4
				h.PressureWarnCount = 1
			}
		}), "lower", 5, []string{"Memory pressure or growing swap in 4 of 4 busy hours."}},
		{"saturated at one job holds at the minimum", "UTC", "", 1, hostHours(48, func(_ int, h *recommendationHour) {
			h.Slots = 1
			h.PressureCriticalCount = 1
		}), "hold", 1, []string{"Memory pressure or growing swap in 48 of 48 busy hours.", "It already runs the minimum of 1 job."}},
		{"raise stops at the routing maximum", "UTC", "", 10000, hostHours(48, func(_ int, h *recommendationHour) { h.Slots = 10000 }), "hold", 10000, []string{"Saturated in 0 of 48 busy hours."}},
		{"low disk reports the runner-local date without changing the limit", "America/Chicago", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			if i == 30 {
				h.DiskFreeMinBytes = 12 * capacityGiB
			}
		}), "hold", 6, []string{"Saturated in 0 of 48 busy hours.", "Free disk fell to 12.9 GB on Oct 11."}},
		{"low disk counts without CPU samples", "UTC", "", 6, hostHours(48, func(i int, h *recommendationHour) {
			if i == 47 {
				h.CPUSampleCount = 0
				h.DiskFreeMinBytes = capacityGiB
			}
		}), "hold", 6, []string{"Saturated in 0 of 47 busy hours.", "Free disk fell to 1.1 GB on Oct 12."}},
		{"full slots without saturation raise by one", "UTC", "", 4, hostHours(40, func(i int, h *recommendationHour) {
			if i%10 < 3 {
				h.Slots = 3.8
			} else {
				h.Slots = 1
			}
		}), "raise", 5, []string{"Every slot was busy in 12 of 40 hours without saturation."}},
		{"idle host never raises", "UTC", "", 4, hostHours(72, func(_ int, h *recommendationHour) { h.Slots = 0 }), "hold", 4, []string{"No work ran in the 72 covered hours."}},
		{"shared runner ignores saturation in person hours", "UTC", "shared", 6, hostHours(72, func(_ int, h *recommendationHour) {
			if hour := h.Hour.Hour(); hour >= 8 && hour < 22 {
				h.PressureWarnCount = 1
			}
		}), "hold", 6, []string{"Saturated in 0 of 30 busy hours."}},
		{"dedicated runner counts the same person-hour saturation", "UTC", "dedicated", 6, hostHours(72, func(_ int, h *recommendationHour) {
			if hour := h.Hour.Hour(); hour >= 8 && hour < 22 {
				h.PressureWarnCount = 1
			}
		}), "lower", 5, []string{"Memory pressure or growing swap in 42 of 72 busy hours."}},
		{"shared runner keeps disk and adjacency from person hours", "UTC", "shared", 6, hostHours(72, func(_ int, h *recommendationHour) {
			hour := h.Hour.Hour()
			if hour == 13 {
				h.DiskFreeMinBytes = capacityGiB
			}
			if hour == 21 || hour == 22 {
				h.CPUBusySumPercent = 95 * 120
			}
		}), "lower", 5, []string{"CPU stayed at 90% or more for 2+ hours running in 3 of 30 busy hours.", "Free disk fell to 1.1 GB on Oct 11."}},
		{"shared runner needs 24 free hours", "UTC", "shared", 6, hostHours(48, nil), "insufficient_evidence", -1, []string{"20 of the 24 covered hours needed so far."}},
		{"stopped runner holds", "UTC", "", 0, hostHours(48, nil), "hold", 0, []string{"The runner is stopped at 0 jobs at once."}},
		{"uncovered hours do not count", "UTC", "", 6, hostHours(30, func(i int, h *recommendationHour) {
			if i < 10 {
				h.CPUSampleCount = 0
			}
		}), "insufficient_evidence", -1, []string{"20 of the 24 covered hours needed so far."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := suggestLimit(test.current, test.timezone, test.hours, nil)
			if test.machineUse != "" {
				out, err := recommendRunner(runnerauth.Routing{CapacityLimit: test.current, MachineUse: test.machineUse, Availability: runnerauth.Availability{Timezone: test.timezone}}, test.hours, nil)
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
