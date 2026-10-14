package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxHostSegments = 128

func validHostSummary(s hostmetrics.Summary, now time.Time) bool {
	if s.Hour.IsZero() || !s.Hour.Equal(s.Hour.UTC().Truncate(time.Hour)) || s.Hour.After(now.UTC().Truncate(time.Hour)) || s.SegmentID.IsZero() || s.SegmentID.After(now) || s.SampleCount == 0 || s.SampleCount > 120 || s.LogicalCores <= 0 || s.LogicalCores > 65536 {
		return false
	}
	for _, count := range []uint64{s.MemorySampleCount, s.SwapSampleCount, s.PSISomeSampleCount, s.PSIFullSampleCount, s.PressureSampleCount, s.CPUSampleCount, s.LoadSampleCount, s.DiskSampleCount} {
		if count > s.SampleCount {
			return false
		}
	}
	if s.PressureWarnCount > s.PressureSampleCount || s.PressureCriticalCount > s.PressureSampleCount-s.PressureWarnCount {
		return false
	}
	if s.MemoryAvailableMinBytes > s.MemoryTotalBytes || s.DiskFreeMinBytes > s.DiskTotalMinBytes {
		return false
	}
	if s.MemorySampleCount == 0 && (s.MemoryTotalBytes != 0 || s.MemoryAvailableMinBytes != 0 || s.MemoryAvailableSumBytes != 0) {
		return false
	}
	if s.MemorySampleCount > 0 && (float64(s.MemoryAvailableSumBytes)/float64(s.MemorySampleCount) > float64(s.MemoryTotalBytes) || float64(s.MemoryAvailableSumBytes)/float64(s.MemorySampleCount) < float64(s.MemoryAvailableMinBytes)) {
		return false
	}
	if s.SwapSampleCount == 0 && s.SwapUsedMaxBytes != 0 || s.DiskSampleCount == 0 && (s.DiskFreeMinBytes != 0 || s.DiskTotalMinBytes != 0) {
		return false
	}
	for _, metric := range []struct {
		sum   float64
		count uint64
		limit float64
	}{
		{s.PSISomeAvg10Sum, s.PSISomeSampleCount, 100}, {s.PSIFullAvg10Sum, s.PSIFullSampleCount, 100}, {s.CPUBusySumPercent, s.CPUSampleCount, 100}, {s.CPUBusyMaxPercent, s.CPUSampleCount, 100}, {s.Load1Max, s.LoadSampleCount, 1e9},
	} {
		if math.IsNaN(metric.sum) || math.IsInf(metric.sum, 0) || metric.sum < 0 || metric.sum > metric.limit*float64(metric.count) {
			return false
		}
	}
	return s.CPUBusyMaxPercent <= 100 && s.CPUBusyMaxPercent <= s.CPUBusySumPercent && s.CPUBusySumPercent <= s.CPUBusyMaxPercent*float64(s.CPUSampleCount)
}

func mergeHostSummary(a, b hostmetrics.Summary) (hostmetrics.Summary, bool) {
	pairs := []struct {
		target *uint64
		value  uint64
	}{
		{&a.SampleCount, b.SampleCount}, {&a.MemoryAvailableSumBytes, b.MemoryAvailableSumBytes}, {&a.MemorySampleCount, b.MemorySampleCount}, {&a.SwapSampleCount, b.SwapSampleCount}, {&a.PSISomeSampleCount, b.PSISomeSampleCount}, {&a.PSIFullSampleCount, b.PSIFullSampleCount}, {&a.PressureWarnCount, b.PressureWarnCount}, {&a.PressureCriticalCount, b.PressureCriticalCount}, {&a.PressureSampleCount, b.PressureSampleCount}, {&a.CPUSampleCount, b.CPUSampleCount}, {&a.LoadSampleCount, b.LoadSampleCount}, {&a.DiskSampleCount, b.DiskSampleCount},
	}
	if b.MemorySampleCount > 0 {
		if a.MemorySampleCount == 0 {
			a.MemoryAvailableMinBytes = b.MemoryAvailableMinBytes
		} else {
			a.MemoryAvailableMinBytes = min(a.MemoryAvailableMinBytes, b.MemoryAvailableMinBytes)
		}
		a.MemoryTotalBytes = max(a.MemoryTotalBytes, b.MemoryTotalBytes)
	}
	if b.DiskSampleCount > 0 {
		if a.DiskSampleCount == 0 {
			a.DiskFreeMinBytes = b.DiskFreeMinBytes
			a.DiskTotalMinBytes = b.DiskTotalMinBytes
		} else {
			a.DiskFreeMinBytes = min(a.DiskFreeMinBytes, b.DiskFreeMinBytes)
			a.DiskTotalMinBytes = min(a.DiskTotalMinBytes, b.DiskTotalMinBytes)
		}
	}
	for _, pair := range pairs {
		if math.MaxUint64-*pair.target < pair.value {
			return a, false
		}
		*pair.target += pair.value
	}
	a.PSISomeAvg10Sum += b.PSISomeAvg10Sum
	a.PSIFullAvg10Sum += b.PSIFullAvg10Sum
	a.CPUBusySumPercent += b.CPUBusySumPercent
	a.CPUBusyMaxPercent = max(a.CPUBusyMaxPercent, b.CPUBusyMaxPercent)
	a.SwapUsedMaxBytes = max(a.SwapUsedMaxBytes, b.SwapUsedMaxBytes)
	a.Load1Max = max(a.Load1Max, b.Load1Max)
	a.Partial = a.Partial || b.Partial
	a.LogicalCores = max(a.LogicalCores, b.LogicalCores)
	return a, true
}

func (s *Service) mergeRunnerHostMetrics(ctx context.Context, tx *sql.Tx, scope nativeScope, raw json.RawMessage, now time.Time) []hostmetrics.Acknowledgment {
	if len(raw) == 0 {
		return nil
	}
	var segments []json.RawMessage
	if json.Unmarshal(raw, &segments) != nil || len(segments) > 24 {
		slog.Default().Warn("runner host summaries dropped", "reason", "invalid batch")
		return nil
	}
	acknowledged := make([]hostmetrics.Acknowledgment, 0, len(segments))
	for _, segment := range segments {
		var summary hostmetrics.Summary
		if len(segment) > 1024 || json.Unmarshal(segment, &summary) != nil || !validHostSummary(summary, now) {
			slog.Default().Warn("runner host summary dropped", "reason", "invalid summary")
			continue
		}
		if summary.Hour.After(now.UTC().Truncate(time.Hour).Add(-720 * time.Hour)) {
			if err := storeRunnerHostSegment(ctx, tx, scope, summary); err != nil {
				slog.Default().Warn("runner host summary dropped", "error", err)
				continue
			}
		}
		acknowledged = append(acknowledged, hostmetrics.Acknowledgment{Hour: summary.Hour, SegmentID: summary.SegmentID})
	}
	return acknowledged
}

func storeRunnerHostSegment(ctx context.Context, tx *sql.Tx, scope nativeScope, summary hostmetrics.Summary) error {
	hour := formatHubTime(summary.Hour)
	var stored, rawIDs string
	ids := []time.Time{}
	aggregate := summary
	err := tx.QueryRowContext(ctx, "SELECT summary_json, segment_ids_json FROM runner_host_hours WHERE organization_id=? AND runner_id=? AND hour=?", scope.organization, scope.credential.Runner.RunnerID, hour).Scan(&stored, &rawIDs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read host hour: %w", err)
	}
	if err == nil {
		if json.Unmarshal([]byte(stored), &aggregate) != nil || json.Unmarshal([]byte(rawIDs), &ids) != nil {
			return errors.New("invalid stored host hour")
		}
		if slices.ContainsFunc(ids, func(id time.Time) bool { return id.Equal(summary.SegmentID) }) {
			return nil
		}
		if len(ids) == maxHostSegments {
			slog.Default().Warn("runner host summary dropped", "reason", "hour segment limit")
			return nil
		}
		var ok bool
		aggregate, ok = mergeHostSummary(aggregate, summary)
		if !ok {
			return errors.New("host hour sum overflow")
		}
	}
	ids = append(ids, summary.SegmentID)
	rawAggregate, err := json.Marshal(aggregate)
	if err != nil {
		return err
	}
	rawSegments, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?,?)
ON CONFLICT(organization_id,runner_id,hour) DO UPDATE SET summary_json=excluded.summary_json,segment_ids_json=excluded.segment_ids_json`, scope.organization, scope.credential.Runner.RunnerID, hour, string(rawAggregate), string(rawSegments))
	return err
}

type runnerHostHour struct {
	hostmetrics.Summary
	SegmentID                   *time.Time `json:"segment_id,omitempty"`
	MemoryAvailableAverageBytes *float64   `json:"memory_available_average_bytes,omitempty"`
	PSISomeAvg10Average         *float64   `json:"psi_some_avg10_average,omitempty"`
	PSIFullAvg10Average         *float64   `json:"psi_full_avg10_average,omitempty"`
	CPUBusyAveragePercent       *float64   `json:"cpu_busy_average_percent,omitempty"`
}

func hostMetricAverage(sum float64, count uint64) *float64 {
	if count == 0 {
		return nil
	}
	value := sum / float64(count)
	return &value
}

func (s *Service) readRunnerHostHistory(ctx context.Context, organization tracker.OrganizationID, runner string, from, to time.Time) ([]runnerHostHour, error) {
	now := s.config.now().UTC().Truncate(time.Hour)
	if from.IsZero() {
		from = now.Add(-167 * time.Hour)
	}
	if to.IsZero() {
		to = now.Add(time.Hour)
	}
	if !from.Before(to) || to.Sub(from) > 720*time.Hour {
		return nil, nativeInvalid("Host history range must be positive and at most 30 days")
	}
	from = from.UTC()
	from = maxHostHistoryTime(from, now.Add(-719*time.Hour))
	rows, err := s.database.reader.QueryContext(ctx, `SELECT summary_json FROM runner_host_hours WHERE organization_id=? AND runner_id=? AND hour>=? AND hour<? ORDER BY hour LIMIT 720`, organization, runner, formatHubTime(from), formatHubTime(to.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := []runnerHostHour{}
	for rows.Next() {
		var raw string
		var hour runnerHostHour
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &hour.Summary); err != nil {
			return nil, err
		}
		hour.MemoryAvailableAverageBytes = hostMetricAverage(float64(hour.MemoryAvailableSumBytes), hour.MemorySampleCount)
		hour.PSISomeAvg10Average = hostMetricAverage(hour.PSISomeAvg10Sum, hour.PSISomeSampleCount)
		hour.PSIFullAvg10Average = hostMetricAverage(hour.PSIFullAvg10Sum, hour.PSIFullSampleCount)
		hour.CPUBusyAveragePercent = hostMetricAverage(hour.CPUBusySumPercent, hour.CPUSampleCount)
		history = append(history, hour)
	}
	return history, rows.Err()
}

func maxHostHistoryTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
