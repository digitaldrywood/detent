package runneradvice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaxHostSegments = 128

func ValidHostSummary(s hostmetrics.Summary) bool {
	if s.Hour.IsZero() || !s.Hour.Equal(s.Hour.UTC().Truncate(time.Hour)) || s.SegmentID.IsZero() || s.SampleCount == 0 || s.SampleCount > 120 || s.LogicalCores <= 0 || s.LogicalCores > 65536 {
		return false
	}
	for _, count := range []uint64{s.MemorySampleCount, s.SwapSampleCount, s.PSISomeSampleCount, s.PSIFullSampleCount, s.PressureSampleCount, s.CPUSampleCount, s.LoadSampleCount, s.DiskSampleCount} {
		if count > s.SampleCount {
			return false
		}
	}
	for _, count := range []uint64{s.SampleCount, s.MemoryTotalBytes, s.MemoryAvailableMinBytes, s.MemoryAvailableSumBytes, s.MemorySampleCount, s.SwapUsedMaxBytes, s.SwapSampleCount, s.PSISomeSampleCount, s.PSIFullSampleCount, s.PressureWarnCount, s.PressureCriticalCount, s.PressureSampleCount, s.CPUSampleCount, s.LoadSampleCount, s.DiskFreeMinBytes, s.DiskTotalMinBytes, s.DiskSampleCount} {
		if count > math.MaxInt64 {
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

func PrepareHostMetrics(raw json.RawMessage) []hostmetrics.Summary {
	if len(raw) == 0 {
		return nil
	}
	var segments []json.RawMessage
	if json.Unmarshal(raw, &segments) != nil || len(segments) > 24 {
		slog.Default().Warn("runner host summaries dropped", "reason", "invalid batch")
		return nil
	}
	summaries := make([]hostmetrics.Summary, 0, len(segments))
	for _, segment := range segments {
		var summary hostmetrics.Summary
		if len(segment) > 1024 || json.Unmarshal(segment, &summary) != nil || !ValidHostSummary(summary) {
			slog.Default().Warn("runner host summary dropped", "reason", "invalid summary")
			continue
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

type HostHour struct {
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

func (s *Service) HostHistory(ctx context.Context, organization tracker.OrganizationID, runner string, from, to time.Time) ([]HostHour, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	now := s.deps.Now().UTC().Truncate(time.Hour)
	if from.IsZero() {
		from = now.Add(-167 * time.Hour)
	}
	if to.IsZero() {
		to = now.Add(time.Hour)
	}
	if !from.Before(to) || to.Sub(from) > 720*time.Hour {
		return nil, hubapi.PublicInvalid("Host history range must be positive and at most 30 days")
	}
	from = from.UTC()
	from = maxHostHistoryTime(from, now.Add(-719*time.Hour))
	var segments []hostmetrics.Summary
	if err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		segments, err = r.RunnerHostMetrics(ctx, runner, from, to.UTC())
		return err
	}); err != nil {
		return nil, err
	}
	return AggregateHostHours(segments)
}

func AggregateHostHours(segments []hostmetrics.Summary) ([]HostHour, error) {
	history := make([]HostHour, 0, len(segments))
	for _, segment := range segments {
		if len(history) == 0 || !history[len(history)-1].Hour.Equal(segment.Hour) {
			history = append(history, HostHour{Summary: segment})
			continue
		}
		previous := history[len(history)-1].Summary
		merged, ok := MergeHostSummary(previous, segment)
		if !ok {
			return nil, errors.New("host hour sum overflow")
		}
		history[len(history)-1].Summary = merged
	}
	for i := range history {
		row := &history[i]
		row.MemoryAvailableAverageBytes = hostMetricAverage(float64(row.MemoryAvailableSumBytes), row.MemorySampleCount)
		row.PSISomeAvg10Average = hostMetricAverage(row.PSISomeAvg10Sum, row.PSISomeSampleCount)
		row.PSIFullAvg10Average = hostMetricAverage(row.PSIFullAvg10Sum, row.PSIFullSampleCount)
		row.CPUBusyAveragePercent = hostMetricAverage(row.CPUBusySumPercent, row.CPUSampleCount)
	}
	return history, nil
}

func maxHostHistoryTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
