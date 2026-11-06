package postgres

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxRunnerHostSegmentsPerHour = 128
const maxRunnerHostHistoryHours = 720

func (s *session) RunnerHostMetrics(ctx context.Context, runnerID string, from, to time.Time) ([]hostmetrics.Summary, error) {
	if runnerID == "" || from.IsZero() || to.IsZero() || !from.Before(to) || to.Sub(from) > maxRunnerHostHistoryHours*time.Hour {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT hour,segment_id,partial,sample_count,logical_cores,memory_total_bytes,memory_available_min_bytes,memory_available_sum_bytes,memory_sample_count,swap_used_max_bytes,swap_sample_count,psi_some_avg10_sum,psi_some_sample_count,psi_full_avg10_sum,psi_full_sample_count,pressure_warn_count,pressure_critical_count,pressure_sample_count,cpu_busy_sum_percent,cpu_busy_max_percent,cpu_sample_count,load1_max,load_sample_count,disk_free_min_bytes,disk_total_min_bytes,disk_sample_count FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2 AND hour >= $3 AND hour < $4 ORDER BY hour,segment_id LIMIT $5`, s.organization, runnerID, from.UTC(), to.UTC(), maxRunnerHostSegmentsPerHour*maxRunnerHostHistoryHours+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	segments := make([]hostmetrics.Summary, 0)
	for rows.Next() {
		var segment hostmetrics.Summary
		var sampleCount, memoryTotal, memoryMin, memorySum, memoryCount, swapMax, swapCount int64
		var psiSomeCount, psiFullCount, pressureWarn, pressureCritical, pressureCount int64
		var cpuCount, loadCount, diskFree, diskTotal, diskCount int64
		if err := rows.Scan(&segment.Hour, &segment.SegmentID, &segment.Partial, &sampleCount, &segment.LogicalCores, &memoryTotal, &memoryMin, &memorySum, &memoryCount, &swapMax, &swapCount, &segment.PSISomeAvg10Sum, &psiSomeCount, &segment.PSIFullAvg10Sum, &psiFullCount, &pressureWarn, &pressureCritical, &pressureCount, &segment.CPUBusySumPercent, &segment.CPUBusyMaxPercent, &cpuCount, &segment.Load1Max, &loadCount, &diskFree, &diskTotal, &diskCount); err != nil {
			return nil, mapError(ctx, err)
		}
		segment.SampleCount = uint64(sampleCount)
		segment.MemoryTotalBytes = uint64(memoryTotal)
		segment.MemoryAvailableMinBytes = uint64(memoryMin)
		segment.MemoryAvailableSumBytes = uint64(memorySum)
		segment.MemorySampleCount = uint64(memoryCount)
		segment.SwapUsedMaxBytes = uint64(swapMax)
		segment.SwapSampleCount = uint64(swapCount)
		segment.PSISomeSampleCount = uint64(psiSomeCount)
		segment.PSIFullSampleCount = uint64(psiFullCount)
		segment.PressureWarnCount = uint64(pressureWarn)
		segment.PressureCriticalCount = uint64(pressureCritical)
		segment.PressureSampleCount = uint64(pressureCount)
		segment.CPUSampleCount = uint64(cpuCount)
		segment.LoadSampleCount = uint64(loadCount)
		segment.DiskFreeMinBytes = uint64(diskFree)
		segment.DiskTotalMinBytes = uint64(diskTotal)
		segment.DiskSampleCount = uint64(diskCount)
		segments = append(segments, segment)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(segments) > maxRunnerHostSegmentsPerHour*maxRunnerHostHistoryHours {
		return nil, corestore.ErrInvalidPage
	}
	return segments, nil
}

func (s *session) AppendRunnerHostMetricSegments(ctx context.Context, runnerID string, segments []hostmetrics.Summary, now time.Time) ([]hostmetrics.Acknowledgment, error) {
	if runnerID == "" || now.IsZero() || len(segments) > 24 {
		return nil, runnerstore.ErrInvalidRecord
	}
	ordered := append([]hostmetrics.Summary(nil), segments...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Hour.Equal(ordered[j].Hour) {
			return ordered[i].SegmentID.Before(ordered[j].SegmentID)
		}
		return ordered[i].Hour.Before(ordered[j].Hour)
	})
	for _, segment := range ordered {
		if !validRunnerHostMetricSegment(segment) {
			return nil, runnerstore.ErrInvalidRecord
		}
	}
	acknowledged := make([]hostmetrics.Acknowledgment, 0, len(ordered))
	for _, segment := range ordered {
		if segment.Hour.After(now.UTC().Truncate(time.Hour)) || segment.SegmentID.After(now) {
			continue
		}
		acknowledged = append(acknowledged, hostmetrics.Acknowledgment{Hour: segment.Hour, SegmentID: segment.SegmentID})
		if !segment.Hour.After(now.UTC().Truncate(time.Hour).Add(-720 * time.Hour)) {
			continue
		}
		lock := runnerHostMetricLockKey(s.organization, runnerID, segment.Hour)
		if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return nil, mapError(ctx, err)
		}
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2 AND hour=$3 AND segment_id=$4)`, s.organization, runnerID, segment.Hour, segment.SegmentID).Scan(&exists); err != nil {
			return nil, mapError(ctx, err)
		}
		if exists {
			continue
		}
		var count int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2 AND hour=$3`, s.organization, runnerID, segment.Hour).Scan(&count); err != nil {
			return nil, mapError(ctx, err)
		}
		if count >= maxRunnerHostSegmentsPerHour {
			continue
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO runner_host_metric_segments (organization_id,runner_id,hour,segment_id,partial,sample_count,logical_cores,memory_total_bytes,memory_available_min_bytes,memory_available_sum_bytes,memory_sample_count,swap_used_max_bytes,swap_sample_count,psi_some_avg10_sum,psi_some_sample_count,psi_full_avg10_sum,psi_full_sample_count,pressure_warn_count,pressure_critical_count,pressure_sample_count,cpu_busy_sum_percent,cpu_busy_max_percent,cpu_sample_count,load1_max,load_sample_count,disk_free_min_bytes,disk_total_min_bytes,disk_sample_count) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`, s.organization, runnerID, segment.Hour, segment.SegmentID, segment.Partial, int64(segment.SampleCount), segment.LogicalCores, int64(segment.MemoryTotalBytes), int64(segment.MemoryAvailableMinBytes), int64(segment.MemoryAvailableSumBytes), int64(segment.MemorySampleCount), int64(segment.SwapUsedMaxBytes), int64(segment.SwapSampleCount), segment.PSISomeAvg10Sum, int64(segment.PSISomeSampleCount), segment.PSIFullAvg10Sum, int64(segment.PSIFullSampleCount), int64(segment.PressureWarnCount), int64(segment.PressureCriticalCount), int64(segment.PressureSampleCount), segment.CPUBusySumPercent, segment.CPUBusyMaxPercent, int64(segment.CPUSampleCount), segment.Load1Max, int64(segment.LoadSampleCount), int64(segment.DiskFreeMinBytes), int64(segment.DiskTotalMinBytes), int64(segment.DiskSampleCount)); err != nil {
			return nil, mapError(ctx, err)
		}
	}
	return acknowledged, nil
}

func runnerHostMetricLockKey(organization tracker.OrganizationID, runnerID string, hour time.Time) string {
	return string(organization) + ":runner-host-metric:" + runnerID + ":" + hour.UTC().Format(time.RFC3339Nano)
}

func validRunnerHostMetricSegment(segment hostmetrics.Summary) bool {
	if segment.Hour.IsZero() || !segment.Hour.Equal(segment.Hour.UTC().Truncate(time.Hour)) || segment.SegmentID.IsZero() || segment.SampleCount == 0 || segment.SampleCount > 120 || segment.LogicalCores <= 0 || segment.LogicalCores > 65536 {
		return false
	}
	for _, count := range []uint64{segment.SampleCount, segment.MemoryTotalBytes, segment.MemoryAvailableMinBytes, segment.MemoryAvailableSumBytes, segment.MemorySampleCount, segment.SwapUsedMaxBytes, segment.SwapSampleCount, segment.PSISomeSampleCount, segment.PSIFullSampleCount, segment.PressureWarnCount, segment.PressureCriticalCount, segment.PressureSampleCount, segment.CPUSampleCount, segment.LoadSampleCount, segment.DiskFreeMinBytes, segment.DiskTotalMinBytes, segment.DiskSampleCount} {
		if count > math.MaxInt64 {
			return false
		}
	}
	for _, count := range []uint64{segment.MemorySampleCount, segment.SwapSampleCount, segment.PSISomeSampleCount, segment.PSIFullSampleCount, segment.PressureSampleCount, segment.CPUSampleCount, segment.LoadSampleCount, segment.DiskSampleCount} {
		if count > segment.SampleCount {
			return false
		}
	}
	if segment.PressureWarnCount > segment.PressureSampleCount || segment.PressureCriticalCount > segment.PressureSampleCount-segment.PressureWarnCount || segment.MemoryAvailableMinBytes > segment.MemoryTotalBytes || segment.DiskFreeMinBytes > segment.DiskTotalMinBytes {
		return false
	}
	if segment.MemorySampleCount == 0 && (segment.MemoryTotalBytes != 0 || segment.MemoryAvailableMinBytes != 0 || segment.MemoryAvailableSumBytes != 0) || segment.MemorySampleCount > 0 && (float64(segment.MemoryAvailableSumBytes)/float64(segment.MemorySampleCount) > float64(segment.MemoryTotalBytes) || float64(segment.MemoryAvailableSumBytes)/float64(segment.MemorySampleCount) < float64(segment.MemoryAvailableMinBytes)) {
		return false
	}
	if segment.SwapSampleCount == 0 && segment.SwapUsedMaxBytes != 0 || segment.DiskSampleCount == 0 && (segment.DiskFreeMinBytes != 0 || segment.DiskTotalMinBytes != 0) {
		return false
	}
	for _, metric := range []struct {
		sum   float64
		count uint64
		limit float64
	}{{segment.PSISomeAvg10Sum, segment.PSISomeSampleCount, 100}, {segment.PSIFullAvg10Sum, segment.PSIFullSampleCount, 100}, {segment.CPUBusySumPercent, segment.CPUSampleCount, 100}, {segment.CPUBusyMaxPercent, segment.CPUSampleCount, 100}, {segment.Load1Max, segment.LoadSampleCount, 1e9}} {
		if math.IsNaN(metric.sum) || math.IsInf(metric.sum, 0) || metric.sum < 0 || metric.sum > metric.limit*float64(metric.count) {
			return false
		}
	}
	return segment.CPUBusyMaxPercent <= 100 && segment.CPUBusyMaxPercent <= segment.CPUBusySumPercent && segment.CPUBusySumPercent <= segment.CPUBusyMaxPercent*float64(segment.CPUSampleCount)
}

func (s *session) RunnerAdmissionObservation(ctx context.Context, runnerID string, project tracker.ProjectID) (tracker.NativeAdmissionObservation, error) {
	var row tracker.NativeAdmissionObservation
	err := s.db.QueryRow(ctx, `SELECT runner_revision,received_at,observed_at,policy_id,last_refresh_at,refresh_duration FROM runner_native_admission_observations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project).Scan(&row.RunnerRevision, &row.ReceivedAt, &row.Context.ObservedAt, &row.Context.PolicyID, &row.Context.LastRefreshAt, &row.Context.RefreshDuration)
	if err != nil {
		return tracker.NativeAdmissionObservation{}, mapError(ctx, err)
	}
	rows, err := s.db.Query(ctx, `SELECT kind,value FROM runner_native_admission_selectors WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 ORDER BY kind,position LIMIT $4`, s.organization, runnerID, project, 161)
	if err != nil {
		return tracker.NativeAdmissionObservation{}, mapError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return tracker.NativeAdmissionObservation{}, mapError(ctx, err)
		}
		switch kind {
		case "workflow":
			row.Context.WorkflowStates = append(row.Context.WorkflowStates, value)
		case "author":
			row.Context.Authors = append(row.Context.Authors, value)
		case "assignee":
			row.Context.Assignees = append(row.Context.Assignees, value)
		case "include":
			row.Context.LabelInclude = append(row.Context.LabelInclude, value)
		case "exclude":
			row.Context.LabelExclude = append(row.Context.LabelExclude, value)
		default:
			return tracker.NativeAdmissionObservation{}, corestore.ErrInvalidPage
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return tracker.NativeAdmissionObservation{}, err
	}
	if count > 160 || row.Context.Validate() != nil {
		return tracker.NativeAdmissionObservation{}, corestore.ErrInvalidPage
	}
	return row, nil
}

func (s *session) ReplaceRunnerAdmissionObservation(ctx context.Context, runnerID string, project tracker.ProjectID, row tracker.NativeAdmissionObservation) error {
	if runnerID == "" || project == "" || row.RunnerRevision < 1 || row.ReceivedAt.IsZero() || row.Context.Validate() != nil {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO runner_native_admission_observations (organization_id,runner_id,project_id,runner_revision,received_at,observed_at,policy_id,last_refresh_at,refresh_duration) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (organization_id,runner_id,project_id) DO UPDATE SET runner_revision=EXCLUDED.runner_revision,received_at=EXCLUDED.received_at,observed_at=EXCLUDED.observed_at,policy_id=EXCLUDED.policy_id,last_refresh_at=EXCLUDED.last_refresh_at,refresh_duration=EXCLUDED.refresh_duration`, s.organization, runnerID, project, row.RunnerRevision, row.ReceivedAt, row.Context.ObservedAt, row.Context.PolicyID, row.Context.LastRefreshAt, int64(row.Context.RefreshDuration)); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM runner_native_admission_selectors WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project); err != nil {
		return mapError(ctx, err)
	}
	selectors := []struct {
		kind   string
		values []string
	}{{"workflow", row.Context.WorkflowStates}, {"author", row.Context.Authors}, {"assignee", row.Context.Assignees}, {"include", row.Context.LabelInclude}, {"exclude", row.Context.LabelExclude}}
	for _, selector := range selectors {
		for position, value := range selector.values {
			if _, err := s.db.Exec(ctx, `INSERT INTO runner_native_admission_selectors (organization_id,runner_id,project_id,kind,position,value) VALUES ($1,$2,$3,$4,$5,$6)`, s.organization, runnerID, project, selector.kind, position, value); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	return nil
}

func (s *session) PruneRunnerHostMetrics(ctx context.Context, cutoff time.Time, limit int) error {
	if cutoff.IsZero() || limit < 1 || limit > runnerstore.MaxRunnerHostPruneBatch {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.Exec(ctx, `DELETE FROM runner_host_metric_segments WHERE ctid IN (SELECT ctid FROM runner_host_metric_segments WHERE organization_id=$1 AND hour <= $2 ORDER BY hour,runner_id,segment_id LIMIT $3)`, s.organization, cutoff.UTC(), limit)
	return mapError(ctx, err)
}
