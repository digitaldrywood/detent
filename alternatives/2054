package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const capacityGiB = 1024 * 1024 * 1024

type runnerCapacityRead struct {
	runnerauth.CapacityView
	Recommendation   runnerRecommendation    `json:"recommendation"`
	ProviderCapacity []providercapacity.View `json:"provider_capacity,omitempty"`
}

type runnerRecommendation struct {
	MachineUse string              `json:"machine_use"`
	Timezone   string              `json:"timezone"`
	From       time.Time           `json:"from"`
	To         time.Time           `json:"to"`
	Partial    bool                `json:"partial"`
	Advisory   bool                `json:"advisory"`
	Periods    []recommendedPeriod `json:"periods"`
	Suggested  suggestedLimit      `json:"suggested_limit"`
}

type suggestedLimit struct {
	Limit          *int     `json:"limit,omitempty"`
	Current        int      `json:"current"`
	Direction      string   `json:"direction"`
	HoursCovered   int      `json:"hours_covered"`
	SaturatedHours int      `json:"saturated_hours"`
	Reasons        []string `json:"reasons"`
}

type recommendedPeriod struct {
	Period             string     `json:"period"`
	Status             string     `json:"status"`
	Slots              *slotRange `json:"slots,omitempty"`
	HoursCovered       float64    `json:"hours_covered"`
	RunsObserved       int        `json:"runs_observed"`
	OverCapacityHours  int        `json:"over_capacity_hours"`
	P90PeakMemoryBytes uint64     `json:"p90_peak_memory_bytes"`
	P90CPUCores        float64    `json:"p90_cpu_cores"`
	MemoryReserveBytes uint64     `json:"memory_reserve_bytes"`
	CoreReserve        int        `json:"core_reserve"`
	DiskReserveBytes   uint64     `json:"disk_reserve_bytes"`
	DiskBytesPerSlot   uint64     `json:"disk_bytes_per_slot"`
	SlotHours          float64    `json:"slot_hours"`
}

type slotRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type recommendationHour struct {
	hostmetrics.Summary
	Slots        float64
	OverCapacity bool
}

type recommendationRun struct {
	StartedAt time.Time
	Usage     procgroup.Usage
}

func personHours(r runnerauth.Routing) activehours.Config {
	windows := []string{}
	for window, slots := range r.Availability.WindowSlots {
		if slots < r.CapacityLimit {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		windows = []string{"Mon-Sun 08:00-22:00"}
	}
	timezone := r.Availability.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	return activehours.Config{Timezone: timezone, Windows: windows}
}

func personHour(r runnerauth.Routing, at time.Time) (bool, error) {
	status, err := activehours.Evaluate(personHours(r), at, time.Time{})
	return status.WindowOpen, err
}

func personHostHour(r runnerauth.Routing, at time.Time) (bool, error) {
	status, err := activehours.Evaluate(personHours(r), at, time.Time{})
	return status.WindowOpen || status.NextOpen.After(at) && status.NextOpen.Before(at.Add(time.Hour)), err
}

func recommendRunner(r runnerauth.Routing, hours []recommendationHour, runs []recommendationRun) (runnerRecommendation, error) {
	r = r.Normalized()
	if r.MachineUse == "" {
		r.MachineUse = "shared"
	}
	out := runnerRecommendation{MachineUse: r.MachineUse, Timezone: r.Availability.Timezone, Advisory: true}
	if out.Timezone == "" {
		out.Timezone = "UTC"
	}
	periods := []string{"dedicated"}
	if r.MachineUse == "shared" {
		periods = []string{"person_hours", "free_hours"}
	}
	personHours := make([]bool, len(hours))
	if r.MachineUse == "shared" {
		for i, hour := range hours {
			person, err := personHostHour(r, hour.Hour)
			if err != nil {
				return out, err
			}
			personHours[i] = person
		}
	}
	for _, period := range periods {
		selected := []recommendationHour{}
		peaks, cores := []float64{}, []float64{}
		for i, hour := range hours {
			if period == "dedicated" || personHours[i] == (period == "person_hours") {
				selected = append(selected, hour)
			}
		}
		for _, run := range runs {
			person := false
			if period != "dedicated" {
				var err error
				person, err = personHour(r, run.StartedAt)
				if err != nil {
					return out, err
				}
			}
			if (period == "dedicated" || person == (period == "person_hours")) && run.Usage.PeakMemoryBytes > 0 && run.Usage.WallSeconds > 0 {
				peaks = append(peaks, float64(run.Usage.PeakMemoryBytes))
				cores = append(cores, (run.Usage.UserCPUSeconds+run.Usage.SystemCPUSeconds)/run.Usage.WallSeconds)
			}
		}
		out.Periods = append(out.Periods, recommendPeriod(period, selected, peaks, cores))
	}
	out.Suggested = suggestLimit(r.CapacityLimit, out.Timezone, hours, personHours)
	return out, nil
}

func hostPressured(hour recommendationHour) bool {
	return hour.PressureWarnCount > 0 || hour.PressureCriticalCount > 0 || hour.PSISomeSampleCount > 0 && hour.PSISomeAvg10Sum/float64(hour.PSISomeSampleCount) > 10
}

func swapGrowing(hour, previous recommendationHour) bool {
	return hour.Hour.Sub(previous.Hour) == time.Hour && hour.SwapSampleCount > 0 && previous.SwapSampleCount > 0 && hour.SwapUsedMaxBytes > previous.SwapUsedMaxBytes && hour.SwapUsedMaxBytes-previous.SwapUsedMaxBytes >= capacityGiB
}

func suggestLimit(current int, timezone string, hours []recommendationHour, skip []bool) suggestedLimit {
	out := suggestedLimit{Current: current, Direction: "insufficient_evidence", Reasons: []string{}}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		location = time.UTC
	}
	cpuHot := func(hour recommendationHour) bool {
		return hour.CPUSampleCount > 0 && hour.CPUBusySumPercent/float64(hour.CPUSampleCount) >= 90
	}
	follows := func(later, earlier int) bool {
		return earlier >= 0 && later < len(hours) && hours[later].Hour.Sub(hours[earlier].Hour) == time.Hour
	}
	type level struct{ hours, saturated int }
	levels := map[int]level{}
	busyHours, cpuHours, memoryHours, fullHours := 0, 0, 0, 0
	var lowDisk *recommendationHour
	for i, hour := range hours {
		if hour.DiskSampleCount > 0 && hour.DiskFreeMinBytes*10 < hour.DiskTotalMinBytes && (lowDisk == nil || hour.DiskFreeMinBytes < lowDisk.DiskFreeMinBytes) {
			lowDisk = &hours[i]
		}
		if hour.MemorySampleCount == 0 || hour.CPUSampleCount == 0 || skip != nil && skip[i] {
			continue
		}
		out.HoursCovered++
		if hour.Slots <= 0 {
			continue
		}
		busyHours++
		swapGrew := i > 0 && swapGrowing(hour, hours[i-1])
		memory := hostPressured(hour) || swapGrew
		cpu := cpuHot(hour) && (follows(i, i-1) && cpuHot(hours[i-1]) || follows(i+1, i) && cpuHot(hours[i+1]))
		slots := int(math.Round(hour.Slots))
		l := levels[slots]
		l.hours++
		if memory || cpu {
			out.SaturatedHours++
			l.saturated++
			if cpu {
				cpuHours++
			}
			if memory {
				memoryHours++
			}
		} else if current > 0 && hour.Slots >= 0.9*float64(current) {
			fullHours++
		}
		levels[slots] = l
	}
	diskReason := func() {
		if lowDisk != nil && len(out.Reasons) < 3 {
			out.Reasons = append(out.Reasons, fmt.Sprintf("Free disk fell to %.1f GB on %s.", float64(lowDisk.DiskFreeMinBytes)/1e9, lowDisk.Hour.In(location).Format("Jan 2")))
		}
	}
	if out.HoursCovered < 24 {
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d of the 24 covered hours needed so far.", out.HoursCovered))
		diskReason()
		return out
	}
	limit := current
	switch {
	case current <= 0:
		out.Reasons = append(out.Reasons, "The runner is stopped at 0 jobs at once.")
	case out.SaturatedHours*20 > busyHours:
		limit = current - 1
		for slots := current - 1; slots >= max(1, current-2); slots-- {
			if l := levels[slots]; l.hours >= 3 && l.saturated*20 <= l.hours {
				limit = slots
				break
			}
		}
		limit = max(1, limit)
		if cpuHours > 0 {
			out.Reasons = append(out.Reasons, fmt.Sprintf("CPU stayed at 90%% or more for 2+ hours running in %d of %d busy hours.", cpuHours, busyHours))
		}
		if memoryHours > 0 {
			out.Reasons = append(out.Reasons, fmt.Sprintf("Memory pressure or growing swap in %d of %d busy hours.", memoryHours, busyHours))
		}
		if limit == current {
			out.Reasons = append(out.Reasons, "It already runs the minimum of 1 job.")
		}
	case current < 10000 && fullHours*4 >= out.HoursCovered:
		limit = current + 1
		out.Reasons = append(out.Reasons, fmt.Sprintf("Every slot was busy in %d of %d hours without saturation.", fullHours, out.HoursCovered))
	case busyHours == 0:
		out.Reasons = append(out.Reasons, fmt.Sprintf("No work ran in the %d covered hours.", out.HoursCovered))
	default:
		out.Reasons = append(out.Reasons, fmt.Sprintf("Saturated in %d of %d busy hours.", out.SaturatedHours, busyHours))
	}
	diskReason()
	out.Direction = "hold"
	if limit < current {
		out.Direction = "lower"
	} else if limit > current {
		out.Direction = "raise"
	}
	out.Limit = &limit
	return out
}

func capacityP90(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[int(math.Ceil(0.9*float64(len(values))))-1]
}

func recommendPeriod(period string, hours []recommendationHour, peaks, cores []float64) recommendedPeriod {
	out := recommendedPeriod{Period: period, Status: "insufficient_evidence", RunsObserved: len(peaks), CoreReserve: 1}
	if period == "person_hours" {
		out.CoreReserve = 2
	}
	peak, cpuPeak := capacityP90(peaks), capacityP90(cores)
	cpu := max(1, cpuPeak)
	out.P90PeakMemoryBytes, out.P90CPUCores = uint64(peak), cpuPeak
	diskPerSlot := []float64{}
	for i, hour := range hours {
		if i == 0 || hour.DiskSampleCount == 0 || hour.Slots == 0 {
			continue
		}
		previous := hours[i-1]
		if previous.DiskSampleCount == 0 || hour.Hour.Sub(previous.Hour) != time.Hour || hour.DiskTotalMinBytes != previous.DiskTotalMinBytes {
			continue
		}
		growth := max(0, float64(previous.DiskFreeMinBytes)-float64(hour.DiskFreeMinBytes))
		diskPerSlot = append(diskPerSlot, growth/hour.Slots)
	}
	out.DiskBytesPerSlot = uint64(capacityP90(diskPerSlot))
	lower, upper, observedUpper := 10000, 10000, 0
	pressureCap := 10000
	for i, hour := range hours {
		swapping := i > 0 && swapGrowing(hour, hours[i-1])
		pressure := hour.OverCapacity || swapping || hostPressured(hour)
		if pressure {
			out.OverCapacityHours++
			if hour.Slots >= 1 {
				pressureCap = min(pressureCap, max(0, int(math.Ceil(hour.Slots))-1))
			}
		}
		if hour.MemorySampleCount == 0 || hour.CPUSampleCount == 0 || hour.DiskSampleCount == 0 {
			continue
		}
		covered := min(1, float64(min(hour.MemorySampleCount, hour.CPUSampleCount, hour.DiskSampleCount))/120)
		out.HoursCovered += covered
		out.SlotHours += hour.Slots
		reserve := max(2*capacityGiB, float64(hour.MemoryTotalBytes)*0.1)
		if period == "person_hours" {
			reserve = max(8*capacityGiB, float64(hour.MemoryTotalBytes)*0.25)
		}
		diskReserve := max(20*capacityGiB, float64(hour.DiskTotalMinBytes)*0.15)
		out.MemoryReserveBytes = max(out.MemoryReserveBytes, uint64(math.Ceil(reserve)))
		out.DiskReserveBytes = max(out.DiskReserveBytes, uint64(math.Ceil(diskReserve)))
		if peak == 0 {
			continue
		}
		memoryLimit := int(math.Floor(max(0, float64(hour.MemoryTotalBytes)-reserve) / peak))
		coreLimit := int(math.Floor(max(0, float64(hour.LogicalCores-out.CoreReserve)) / cpu))
		limit := min(memoryLimit, coreLimit, 10000)
		if float64(hour.DiskFreeMinBytes) < diskReserve {
			limit = 0
		} else if out.DiskBytesPerSlot > 0 {
			diskBackground := max(0, float64(hour.DiskTotalMinBytes)-float64(hour.DiskFreeMinBytes)-hour.Slots*float64(out.DiskBytesPerSlot))
			limit = min(limit, int(math.Floor((float64(hour.DiskTotalMinBytes)-diskBackground-diskReserve)/float64(out.DiskBytesPerSlot))))
		}
		upper = min(upper, limit)
		memoryBackground := max(0, float64(hour.MemoryTotalBytes-hour.MemoryAvailableMinBytes)-hour.Slots*peak)
		cpuBackground := max(0, hour.CPUBusySumPercent/float64(hour.CPUSampleCount)*float64(hour.LogicalCores)/100-hour.Slots*cpu)
		measured := min(limit, int(math.Floor(max(0, float64(hour.MemoryTotalBytes)-max(reserve, memoryBackground))/peak)), int(math.Floor(max(0, float64(hour.LogicalCores)-max(float64(out.CoreReserve), cpuBackground))/cpu)))
		lower = min(lower, measured)
		observedUpper = max(observedUpper, measured)
	}
	if out.HoursCovered < 24 || out.RunsObserved < 10 {
		return out
	}
	upper = min(upper, observedUpper)
	if out.OverCapacityHours > 0 {
		upper = min(max(0, upper-1), pressureCap)
		lower = min(max(0, lower-1), upper)
	}
	out.Status = "recommended"
	out.Slots = &slotRange{Min: max(0, lower), Max: max(0, upper)}
	return out
}

func readRunnerRecommendation(ctx context.Context, q nativeQueryer, r runnerauth.Runner, now time.Time) (runnerRecommendation, error) {
	to := now.UTC().Truncate(time.Hour)
	from := to.Add(-719 * time.Hour)
	hours, err := readRecommendationHours(ctx, q, r, from, to)
	if err != nil {
		return runnerRecommendation{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT a.started_at,json_extract(a.data_json,'$.process_usage') FROM native_attempts a JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND lr.runner_id=? AND a.status IN ('succeeded','failed','cancelled') AND a.started_at>=? AND a.updated_at<=? AND json_extract(a.data_json,'$.process_usage.peak_memory_bytes')>0 ORDER BY a.started_at DESC LIMIT 10001`, r.OrganizationID, r.RunnerID, formatHubTime(from), formatHubTime(to))
	if err != nil {
		return runnerRecommendation{}, err
	}
	defer rows.Close()
	runs := []recommendationRun{}
	for rows.Next() {
		var started, raw string
		var run recommendationRun
		if err := rows.Scan(&started, &raw); err != nil {
			return runnerRecommendation{}, err
		}
		run.StartedAt, err = parseTimeValue(started)
		if err != nil {
			return runnerRecommendation{}, err
		}
		if err := json.Unmarshal([]byte(raw), &run.Usage); err != nil {
			return runnerRecommendation{}, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return runnerRecommendation{}, err
	}
	partial := len(runs) > 10000
	if partial {
		runs = runs[:10000]
	}
	out, err := recommendRunner(r.Routing, hours, runs)
	out.From, out.To, out.Partial = from, to, partial
	return out, err
}

func readRecommendationHostHours(ctx context.Context, q nativeQueryer, r runnerauth.Runner, from, to time.Time) ([]recommendationHour, error) {
	rows, err := q.QueryContext(ctx, `SELECT summary_json FROM runner_host_hours WHERE organization_id=? AND runner_id=? AND hour>=? AND hour<? ORDER BY hour LIMIT 720`, r.OrganizationID, r.RunnerID, formatHubTime(from), formatHubTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hours := []recommendationHour{}
	for rows.Next() {
		var raw string
		var hour recommendationHour
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &hour.Summary); err != nil {
			return nil, err
		}
		hours = append(hours, hour)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := 1; i < len(hours); i++ {
		hours[i].OverCapacity = swapGrowing(hours[i], hours[i-1])
	}
	return hours, nil
}

func readRecommendationHours(ctx context.Context, q nativeQueryer, r runnerauth.Runner, from, to time.Time) ([]recommendationHour, error) {
	hours, err := readRecommendationHostHours(ctx, q, r, from, to)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT l.acquired_at,coalesce(l.released_at,l.expires_at),l.expires_at FROM leases l INDEXED BY runner_capacity_lease_window JOIN issues i ON i.id=l.issue_id JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE i.organization_id=? AND l.machine_id=? AND lr.runner_id=? AND julianday(l.acquired_at)<julianday(?) AND min(julianday(coalesce(l.released_at,l.expires_at)),julianday(l.expires_at))>julianday(?)`, r.OrganizationID, r.MachineID, r.RunnerID, formatHubTime(to), formatHubTime(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var acquired, released, expires string
		if err := rows.Scan(&acquired, &released, &expires); err != nil {
			return nil, err
		}
		start, err := parseTimeValue(acquired)
		if err != nil {
			return nil, err
		}
		finish, err := parseTimeValue(released)
		if err != nil {
			return nil, err
		}
		expiry, err := parseTimeValue(expires)
		if err != nil {
			return nil, err
		}
		finish = minTime(finish, expiry)
		first, _ := slices.BinarySearchFunc(hours, start.Truncate(time.Hour), func(hour recommendationHour, at time.Time) int {
			return hour.Hour.Compare(at)
		})
		for i := first; i < len(hours) && hours[i].Hour.Before(finish); i++ {
			overlap := minTime(finish, hours[i].Hour.Add(time.Hour)).Sub(maxTime(start, hours[i].Hour))
			if overlap > 0 {
				hours[i].Slots += overlap.Hours()
			}
		}
	}
	return hours, rows.Err()
}
