package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) RecordWorkflowPhaseEvent(ctx context.Context, attrs WorkflowPhaseEvent) (int64, error) {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return 0, errors.New("project_id is required")
	}
	phaseType := strings.TrimSpace(string(attrs.PhaseType))
	if phaseType == "" {
		return 0, errors.New("phase_type is required")
	}
	phaseName := strings.TrimSpace(attrs.PhaseName)
	if phaseName == "" {
		return 0, errors.New("phase_name is required")
	}
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}
	finishedAt, err := optionalTimestamp("finished_at", attrs.FinishedAt)
	if err != nil {
		return 0, err
	}

	eventAt := attrs.StartedAt
	if !attrs.FinishedAt.IsZero() {
		eventAt = attrs.FinishedAt
	}
	metadataJSON := strings.TrimSpace(attrs.MetadataJSON)
	if metadataJSON == "" {
		metadataJSON = "{}"
	}

	event, err := s.queries.CreateWorkflowPhaseEvent(ctx, sqlc.CreateWorkflowPhaseEventParams{
		ProjectID:         projectID,
		RunID:             nullPositiveInt64(attrs.RunID),
		SessionID:         nullPositiveInt64(attrs.SessionID),
		IssueID:           nullString(attrs.IssueID),
		Identifier:        nullString(attrs.Identifier),
		IssueURL:          nullString(attrs.IssueURL),
		PrNumber:          nullOptionalInt64(attrs.PRNumber),
		PhaseType:         phaseType,
		PhaseName:         phaseName,
		PreviousPhaseName: nullString(attrs.PreviousPhaseName),
		Reason:            nullString(attrs.Reason),
		Status:            nullString(attrs.Status),
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		DurationSeconds:   workflowEventDuration(attrs),
		EventDay:          eventAt.UTC().Format("2006-01-02"),
		CommandName:       nullString(attrs.CommandName),
		ExitCode:          nullOptionalInt64(attrs.ExitCode),
		Turns:             nonNegative(attrs.Turns),
		InputTokens:       nonNegative(attrs.InputTokens),
		OutputTokens:      nonNegative(attrs.OutputTokens),
		TotalTokens:       nonNegative(attrs.TotalTokens),
		EndpointFamily:    nullString(attrs.EndpointFamily),
		MetadataJson:      metadataJSON,
	})
	if err != nil {
		return 0, fmt.Errorf("recording workflow phase event: %w", err)
	}
	return event.ID, nil
}

func (s *sqliteStore) WorkflowMetricsReport(ctx context.Context, query WorkflowMetricsQuery) (WorkflowMetricsReport, error) {
	from, err := optionalTimestamp("from", query.From)
	if err != nil {
		return WorkflowMetricsReport{}, err
	}
	to, err := optionalTimestamp("to", query.To)
	if err != nil {
		return WorkflowMetricsReport{}, err
	}
	if from.Valid && to.Valid && from.String >= to.String {
		return WorkflowMetricsReport{}, errors.New("from must be before to")
	}

	rows, err := s.queries.WorkflowPhaseDurationRows(ctx, sqlc.WorkflowPhaseDurationRowsParams{
		ProjectID: nullString(query.ProjectID),
		FromTime:  from,
		ToTime:    to,
	})
	if err != nil {
		return WorkflowMetricsReport{}, fmt.Errorf("reading workflow metrics report: %w", err)
	}

	metricRows := make([]workflowMetricRow, 0, len(rows))
	for _, row := range rows {
		event, err := workflowPhaseEventFromRow(row)
		if err != nil {
			return WorkflowMetricsReport{}, err
		}
		metricRows = append(metricRows, workflowMetricRow{event: event})
	}

	return workflowMetricsReport(metricRows), nil
}

func (s *sqliteStore) IssueWorkflowTimeline(ctx context.Context, identity IssueIdentity) (WorkflowTimeline, error) {
	identity = normalizeIssueIdentity(identity)
	if identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		return WorkflowTimeline{Events: []WorkflowPhaseEvent{}}, nil
	}

	rows, err := s.queries.IssueWorkflowTimelineRows(ctx, sqlc.IssueWorkflowTimelineRowsParams{
		IssueID:    nullString(identity.IssueID),
		Identifier: nullString(identity.Identifier),
		IssueURL:   nullString(identity.IssueURL),
	})
	if err != nil {
		return WorkflowTimeline{}, fmt.Errorf("reading workflow timeline: %w", err)
	}

	timeline := WorkflowTimeline{Events: make([]WorkflowPhaseEvent, 0, len(rows))}
	for _, row := range rows {
		event, err := workflowPhaseEventFromRow(row)
		if err != nil {
			return WorkflowTimeline{}, err
		}
		timeline.Events = append(timeline.Events, event)
	}
	return timeline, nil
}

type workflowMetricRow struct {
	event WorkflowPhaseEvent
}

type workflowMetricBucket struct {
	projectID      string
	phaseType      string
	phaseName      string
	endpointFamily string
	durations      []int64
	inputTokens    int64
	outputTokens   int64
	totalTokens    int64
	turns          int64
}

func workflowMetricsReport(rows []workflowMetricRow) WorkflowMetricsReport {
	buckets := map[string]*workflowMetricBucket{}
	for _, row := range rows {
		event := row.event
		if event.DurationSeconds < 0 {
			continue
		}
		key := workflowMetricKey(event)
		bucket, ok := buckets[key]
		if !ok {
			bucket = &workflowMetricBucket{
				projectID:      event.ProjectID,
				phaseType:      string(event.PhaseType),
				phaseName:      event.PhaseName,
				endpointFamily: event.EndpointFamily,
			}
			buckets[key] = bucket
		}
		bucket.durations = append(bucket.durations, event.DurationSeconds)
		bucket.inputTokens += event.InputTokens
		bucket.outputTokens += event.OutputTokens
		bucket.totalTokens += event.TotalTokens
		bucket.turns += event.Turns
	}

	metrics := make([]WorkflowPhaseMetric, 0, len(buckets))
	for _, bucket := range buckets {
		metrics = append(metrics, workflowPhaseMetricFromBucket(bucket))
	}
	sort.SliceStable(metrics, func(i, j int) bool {
		if metrics[i].ProjectID != metrics[j].ProjectID {
			return metrics[i].ProjectID < metrics[j].ProjectID
		}
		if metrics[i].PhaseType != metrics[j].PhaseType {
			return metrics[i].PhaseType < metrics[j].PhaseType
		}
		if metrics[i].PhaseName != metrics[j].PhaseName {
			return metrics[i].PhaseName < metrics[j].PhaseName
		}
		return metrics[i].EndpointFamily < metrics[j].EndpointFamily
	})

	report := WorkflowMetricsReport{
		Lanes:     []WorkflowPhaseMetric{},
		SubPhases: []WorkflowPhaseMetric{},
	}
	for _, metric := range metrics {
		if metric.PhaseType == string(WorkflowPhaseTypeLane) {
			report.Lanes = append(report.Lanes, metric)
			continue
		}
		report.SubPhases = append(report.SubPhases, metric)
	}
	return report
}

func workflowMetricKey(event WorkflowPhaseEvent) string {
	parts := []string{
		event.ProjectID,
		string(event.PhaseType),
		event.PhaseName,
	}
	if event.EndpointFamily != "" {
		parts = append(parts, event.EndpointFamily)
	}
	return strings.Join(parts, "\x00")
}

func workflowPhaseMetricFromBucket(bucket *workflowMetricBucket) WorkflowPhaseMetric {
	sort.Slice(bucket.durations, func(i, j int) bool {
		return bucket.durations[i] < bucket.durations[j]
	})

	total := int64(0)
	for _, duration := range bucket.durations {
		total += duration
	}
	count := int64(len(bucket.durations))
	average := int64(0)
	if count > 0 {
		average = total / count
	}

	return WorkflowPhaseMetric{
		ProjectID:      bucket.projectID,
		PhaseType:      bucket.phaseType,
		PhaseName:      bucket.phaseName,
		Count:          count,
		TotalSeconds:   total,
		AverageSeconds: average,
		P50Seconds:     percentileSeconds(bucket.durations, 0.50),
		P90Seconds:     percentileSeconds(bucket.durations, 0.90),
		P95Seconds:     percentileSeconds(bucket.durations, 0.95),
		InputTokens:    bucket.inputTokens,
		OutputTokens:   bucket.outputTokens,
		TotalTokens:    bucket.totalTokens,
		Turns:          bucket.turns,
		EndpointFamily: bucket.endpointFamily,
	}
}

func percentileSeconds(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	if percentile <= 0 {
		return values[0]
	}
	if percentile >= 1 {
		return values[len(values)-1]
	}
	index := int(percentile*float64(len(values))+0.999999999) - 1
	if index < 0 {
		return values[0]
	}
	if index >= len(values) {
		return values[len(values)-1]
	}
	return values[index]
}

func workflowEventDuration(attrs WorkflowPhaseEvent) int64 {
	if attrs.DurationSeconds > 0 {
		return attrs.DurationSeconds
	}
	if attrs.StartedAt.IsZero() || attrs.FinishedAt.IsZero() || attrs.FinishedAt.Before(attrs.StartedAt) {
		return 0
	}
	return int64(attrs.FinishedAt.Sub(attrs.StartedAt) / time.Second)
}

func optionalTimestamp(name string, value time.Time) (sql.NullString, error) {
	if value.IsZero() {
		return sql.NullString{}, nil
	}
	text, err := requiredTimestamp(name, value)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: text, Valid: true}, nil
}

func workflowPhaseEventFromRow(row sqlc.WorkflowPhaseEvent) (WorkflowPhaseEvent, error) {
	startedAt, err := parseTimestamp("started_at", row.StartedAt)
	if err != nil {
		return WorkflowPhaseEvent{}, err
	}
	var finishedAt time.Time
	if row.FinishedAt.Valid {
		finishedAt, err = parseTimestamp("finished_at", row.FinishedAt.String)
		if err != nil {
			return WorkflowPhaseEvent{}, err
		}
	}

	event := WorkflowPhaseEvent{
		ID:                row.ID,
		ProjectID:         strings.TrimSpace(row.ProjectID),
		IssueID:           row.IssueID.String,
		Identifier:        row.Identifier.String,
		IssueURL:          row.IssueURL.String,
		PhaseType:         WorkflowPhaseType(row.PhaseType),
		PhaseName:         row.PhaseName,
		PreviousPhaseName: row.PreviousPhaseName.String,
		Reason:            row.Reason.String,
		Status:            row.Status.String,
		StartedAt:         startedAt.UTC(),
		FinishedAt:        finishedAt.UTC(),
		DurationSeconds:   nonNegative(row.DurationSeconds),
		CommandName:       row.CommandName.String,
		Turns:             nonNegative(row.Turns),
		InputTokens:       nonNegative(row.InputTokens),
		OutputTokens:      nonNegative(row.OutputTokens),
		TotalTokens:       nonNegative(row.TotalTokens),
		EndpointFamily:    row.EndpointFamily.String,
		MetadataJSON:      row.MetadataJson,
	}
	if row.RunID.Valid {
		event.RunID = row.RunID.Int64
	}
	if row.SessionID.Valid {
		event.SessionID = row.SessionID.Int64
	}
	if row.PrNumber.Valid {
		value := row.PrNumber.Int64
		event.PRNumber = &value
	}
	if row.ExitCode.Valid {
		value := row.ExitCode.Int64
		event.ExitCode = &value
	}
	return event, nil
}
