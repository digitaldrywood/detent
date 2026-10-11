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
	}{
		{"dedicated", "dedicated", nil, &slotRange{14, 14}, 24, 10, 0},
		{"shared day", "person_hours", nil, &slotRange{12, 12}, 24, 10, 0},
		{"shared night", "free_hours", nil, &slotRange{14, 14}, 24, 10, 0},
		{"warn pressure", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureWarnCount = 1
			h[0].Slots = 6
			return h, r
		}, &slotRange{5, 5}, 24, 10, 1},
		{"critical pressure", "person_hours", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PressureCriticalCount = 1
			h[0].Slots = 4
			return h, r
		}, &slotRange{3, 3}, 24, 10, 1},
		{"PSI above ten", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PSISomeAvg10Sum = 1320
			h[0].PSISomeSampleCount = 120
			h[0].Slots = 3
			return h, r
		}, &slotRange{2, 2}, 24, 10, 1},
		{"PSI exactly ten", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].PSISomeAvg10Sum = 1200
			h[0].PSISomeSampleCount = 120
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0},
		{"sustained swap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			h[1].SwapUsedMaxBytes = capacityGiB
			h[1].Slots = 5
			return h, r
		}, &slotRange{4, 4}, 24, 10, 1},
		{"isolated swap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].SwapUsedMaxBytes = capacityGiB
			return h, r
		}, &slotRange{14, 14}, 24, 10, 0},
		{"hours insufficient", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			return h[:23], r
		}, nil, 23, 10, 0},
		{"runs insufficient", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			return h, r[:9]
		}, nil, 24, 9, 0},
		{"partial hours", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := range h {
				h[i].MemorySampleCount = 60
			}
			return h, r
		}, nil, 12, 10, 0},
		{"disk below reserve", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].DiskFreeMinBytes = 20 * capacityGiB
			return h, r
		}, &slotRange{0, 0}, 24, 10, 0},
		{"CPU cap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			for i := range r {
				r[i].Usage.UserCPUSeconds = 190
			}
			return h, r
		}, &slotRange{7, 7}, 24, 10, 0},
		{"measured headroom range", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].MemoryAvailableMinBytes = 24 * capacityGiB
			h[0].Slots = 2
			return h, r
		}, &slotRange{8, 14}, 24, 10, 0},
		{"measured disk cap", "dedicated", func(h []recommendationHour, r []recommendationRun) ([]recommendationHour, []recommendationRun) {
			h[0].DiskFreeMinBytes = 280 * capacityGiB
			h[0].Slots = 4
			return h, r
		}, &slotRange{10, 10}, 24, 10, 0},
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
			if !reflect.DeepEqual(got.Slots, test.want) || got.HoursCovered != test.hours || got.RunsObserved != test.runs || got.OverCapacityHours != test.pressure {
				t.Fatalf("recommendation=%+v slots=%+v, want slots=%+v hours=%v runs=%d pressure=%d", got, got.Slots, test.want, test.hours, test.runs, test.pressure)
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
	for _, hour := range hours {
		raw, err := json.Marshal(hour.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?, '[]')`, f.project.OrganizationID, r.binding.RunnerID, formatHubTime(hour.Hour), string(raw)); err != nil {
			t.Fatal(err)
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
