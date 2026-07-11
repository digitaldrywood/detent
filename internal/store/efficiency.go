package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

func (s *sqliteStore) CompleteEfficiencyReceipt(ctx context.Context, completion efficiency.Completion) (efficiency.Receipt, error) {
	completion.ProjectID = strings.TrimSpace(completion.ProjectID)
	completion.IssueID = strings.TrimSpace(completion.IssueID)
	completion.Identifier = strings.TrimSpace(completion.Identifier)
	completion.IssueURL = strings.TrimSpace(completion.IssueURL)
	if completion.ProjectID == "" {
		return efficiency.Receipt{}, errors.New("project_id is required")
	}
	if completion.IssueID == "" {
		return efficiency.Receipt{}, errors.New("issue_id is required")
	}
	if completion.CompletedAt.IsZero() {
		return efficiency.Receipt{}, errors.New("completed_at is required")
	}

	receipt := efficiency.Receipt{
		ProjectID:   completion.ProjectID,
		IssueID:     completion.IssueID,
		Identifier:  completion.Identifier,
		IssueURL:    completion.IssueURL,
		PRNumber:    completion.PRNumber,
		CompletedAt: completion.CompletedAt.UTC(),
	}
	if err := s.readEfficiencyRawTotals(ctx, &receipt); err != nil {
		return efficiency.Receipt{}, err
	}
	if receipt.FirstDispatchedAt.IsZero() {
		receipt.FirstDispatchedAt = receipt.CompletedAt
	}
	receipt.WallSeconds = nonNegativeDurationSeconds(receipt.FirstDispatchedAt, receipt.CompletedAt)
	normalizeEfficiencyDwell(&receipt)
	receipt.Redispatches = max(receipt.Attempts-1, 0)

	baseline, err := s.efficiencyBaseline(ctx, receipt.ProjectID, receipt.IssueID)
	if err != nil {
		return efficiency.Receipt{}, err
	}
	receipt.TokensBaseline = baseline.tokens
	receipt.SessionsBaseline = baseline.sessions
	receipt.DwellBaselineSeconds = baseline.dwell
	thresholds := normalizeEfficiencyThresholds(completion.Thresholds)
	receipt.TokensAnomaly = exceedsBaseline(float64(receipt.TotalTokens), baseline.tokens, thresholds.TokensMultiple)
	receipt.SessionsAnomaly = exceedsBaseline(float64(receipt.Sessions), baseline.sessions, thresholds.SessionsMultiple)
	receipt.DwellAnomaly = exceedsBaseline(float64(receipt.WallSeconds), baseline.dwell, thresholds.DwellMultiple)

	if err := s.upsertEfficiencyReceipt(ctx, receipt); err != nil {
		return efficiency.Receipt{}, err
	}
	return receipt, nil
}

func (s *sqliteStore) readEfficiencyRawTotals(ctx context.Context, receipt *efficiency.Receipt) error {
	row := s.db.QueryRowContext(ctx, `
SELECT
  COUNT(*),
  COALESCE(SUM(input_tokens), 0),
  COALESCE(SUM(cached_input_tokens), 0),
  COALESCE(SUM(output_tokens), 0),
  COALESCE(SUM(reasoning_output_tokens), 0),
  COALESCE(SUM(total_tokens), 0),
  COALESCE(MIN(session.started_at), '')
FROM codex_sessions AS session
LEFT JOIN work_attempts AS attempt ON attempt.id = session.work_attempt_id
WHERE (attempt.project_id = ? OR attempt.project_id IS NULL)
  AND ((? != '' AND session.issue_id = ?)
   OR (? != '' AND session.identifier = ?)
   OR (? != '' AND session.issue_url = ?))`,
		receipt.ProjectID,
		receipt.IssueID, receipt.IssueID,
		receipt.Identifier, receipt.Identifier,
		receipt.IssueURL, receipt.IssueURL,
	)
	var firstSession string
	if err := row.Scan(
		&receipt.Sessions,
		&receipt.InputTokens,
		&receipt.CachedInputTokens,
		&receipt.OutputTokens,
		&receipt.ReasoningOutputTokens,
		&receipt.TotalTokens,
		&firstSession,
	); err != nil {
		return fmt.Errorf("reading efficiency session totals: %w", err)
	}

	row = s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(started_at), '')
FROM work_attempts
WHERE project_id = ?
  AND ((? != '' AND issue_id = ?)
    OR (? != '' AND identifier = ?)
    OR (? != '' AND issue_url = ?))`,
		receipt.ProjectID,
		receipt.IssueID, receipt.IssueID,
		receipt.Identifier, receipt.Identifier,
		receipt.IssueURL, receipt.IssueURL,
	)
	var firstAttempt string
	if err := row.Scan(&receipt.Attempts, &firstAttempt); err != nil {
		return fmt.Errorf("reading efficiency attempt totals: %w", err)
	}

	row = s.db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN phase_type = 'lane' AND status = 'exited' AND lower(trim(phase_name)) IN ('in progress', 'rework', 'todo') THEN duration_seconds ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN phase_type = 'lane' AND status = 'exited' AND lower(trim(phase_name)) IN ('human review', 'review', 'gate wait') THEN duration_seconds ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN phase_type = 'lane' AND status = 'exited' AND lower(trim(phase_name)) = 'merging' THEN duration_seconds ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN phase_type = 'lane' AND status = 'exited' AND lower(trim(phase_name)) = 'blocked' THEN duration_seconds ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN phase_type = 'lane' AND status = 'entered' AND lower(reason) LIKE '%circuit_breaker%' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN phase_type = 'review' AND phase_name = 'ci_rerun' THEN 1 ELSE 0 END), 0)
FROM workflow_phase_events
WHERE workflow_phase_events.project_id = ?
  AND ((? != '' AND workflow_phase_events.issue_id = ?)
    OR (? != '' AND workflow_phase_events.identifier = ?)
    OR (? != '' AND workflow_phase_events.issue_url = ?))`,
		receipt.ProjectID,
		receipt.IssueID, receipt.IssueID,
		receipt.Identifier, receipt.Identifier,
		receipt.IssueURL, receipt.IssueURL,
	)
	if err := row.Scan(
		&receipt.WorkingSeconds,
		&receipt.GateWaitSeconds,
		&receipt.MergeTrainSeconds,
		&receipt.ParkedSeconds,
		&receipt.BreakerTrips,
		&receipt.CIReruns,
	); err != nil {
		return fmt.Errorf("reading efficiency dwell totals: %w", err)
	}

	row = s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(cost_usd), 0)
FROM usage_events
WHERE project_id = ?
  AND ((? != '' AND issue_id = ?) OR (? != '' AND identifier = ?))`,
		receipt.ProjectID,
		receipt.IssueID, receipt.IssueID,
		receipt.Identifier, receipt.Identifier,
	)
	if err := row.Scan(&receipt.EstimatedCostUSD); err != nil {
		return fmt.Errorf("reading efficiency cost total: %w", err)
	}

	first, err := earliestEfficiencyTime(firstSession, firstAttempt)
	if err != nil {
		return err
	}
	receipt.FirstDispatchedAt = first
	return nil
}

type efficiencyBaseline struct {
	tokens   float64
	sessions float64
	dwell    float64
}

func (s *sqliteStore) efficiencyBaseline(ctx context.Context, projectID string, issueID string) (efficiencyBaseline, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
  COALESCE(AVG(total_tokens), 0),
  COALESCE(AVG(sessions), 0),
  COALESCE(AVG(wall_seconds), 0)
FROM efficiency_receipts
WHERE project_id = ? AND issue_id != ?`, projectID, issueID)
	var baseline efficiencyBaseline
	if err := row.Scan(&baseline.tokens, &baseline.sessions, &baseline.dwell); err != nil {
		return efficiencyBaseline{}, fmt.Errorf("reading efficiency baseline: %w", err)
	}
	return baseline, nil
}

func (s *sqliteStore) upsertEfficiencyReceipt(ctx context.Context, receipt efficiency.Receipt) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO efficiency_receipts (
  project_id, issue_id, identifier, issue_url, pr_number, sessions, attempts,
  input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens, total_tokens,
  estimated_cost_usd, first_dispatched_at, completed_at, wall_seconds,
  working_seconds, gate_wait_seconds, merge_train_seconds, parked_seconds,
  redispatches, breaker_trips, ci_reruns, tokens_baseline, sessions_baseline,
  dwell_baseline_seconds, tokens_anomaly, sessions_anomaly, dwell_anomaly
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id, issue_id) DO UPDATE SET
  identifier = excluded.identifier,
  issue_url = excluded.issue_url,
  pr_number = excluded.pr_number,
  sessions = excluded.sessions,
  attempts = excluded.attempts,
  input_tokens = excluded.input_tokens,
  cached_input_tokens = excluded.cached_input_tokens,
  output_tokens = excluded.output_tokens,
  reasoning_output_tokens = excluded.reasoning_output_tokens,
  total_tokens = excluded.total_tokens,
  estimated_cost_usd = excluded.estimated_cost_usd,
  first_dispatched_at = excluded.first_dispatched_at,
  completed_at = excluded.completed_at,
  wall_seconds = excluded.wall_seconds,
  working_seconds = excluded.working_seconds,
  gate_wait_seconds = excluded.gate_wait_seconds,
  merge_train_seconds = excluded.merge_train_seconds,
  parked_seconds = excluded.parked_seconds,
  redispatches = excluded.redispatches,
  breaker_trips = excluded.breaker_trips,
  ci_reruns = excluded.ci_reruns,
  tokens_baseline = excluded.tokens_baseline,
  sessions_baseline = excluded.sessions_baseline,
  dwell_baseline_seconds = excluded.dwell_baseline_seconds,
  tokens_anomaly = excluded.tokens_anomaly,
  sessions_anomaly = excluded.sessions_anomaly,
  dwell_anomaly = excluded.dwell_anomaly`,
		receipt.ProjectID, receipt.IssueID, nullText(receipt.Identifier), nullText(receipt.IssueURL), receipt.PRNumber,
		receipt.Sessions, receipt.Attempts, receipt.InputTokens, receipt.CachedInputTokens, receipt.OutputTokens,
		receipt.ReasoningOutputTokens, receipt.TotalTokens, receipt.EstimatedCostUSD,
		receipt.FirstDispatchedAt.UTC().Format(time.RFC3339Nano), receipt.CompletedAt.UTC().Format(time.RFC3339Nano), receipt.WallSeconds,
		receipt.WorkingSeconds, receipt.GateWaitSeconds, receipt.MergeTrainSeconds, receipt.ParkedSeconds,
		receipt.Redispatches, receipt.BreakerTrips, receipt.CIReruns, receipt.TokensBaseline, receipt.SessionsBaseline,
		receipt.DwellBaselineSeconds, receipt.TokensAnomaly, receipt.SessionsAnomaly, receipt.DwellAnomaly,
	)
	if err != nil {
		return fmt.Errorf("persisting efficiency receipt: %w", err)
	}
	return nil
}

func (s *sqliteStore) EfficiencyReceipt(ctx context.Context, projectID string, issueID string, identifier string) (efficiency.Receipt, error) {
	row := s.db.QueryRowContext(ctx, efficiencyReceiptSelect+`
WHERE project_id = ?
  AND ((? != '' AND issue_id = ?) OR (? != '' AND identifier = ?))
ORDER BY completed_at DESC, id DESC
LIMIT 1`, strings.TrimSpace(projectID), strings.TrimSpace(issueID), strings.TrimSpace(issueID), strings.TrimSpace(identifier), strings.TrimSpace(identifier))
	receipt, err := scanEfficiencyReceipt(row)
	if err != nil {
		return efficiency.Receipt{}, fmt.Errorf("reading efficiency receipt: %w", err)
	}
	return receipt, nil
}

func (s *sqliteStore) ListEfficiencyReceipts(ctx context.Context, query efficiency.Query) ([]efficiency.Receipt, error) {
	args := []any{strings.TrimSpace(query.ProjectID), timestampFilter(query.From), timestampFilter(query.To)}
	statement := efficiencyReceiptSelect + `
WHERE (? = '' OR project_id = ?)
  AND (? = '' OR completed_at >= ?)
  AND (? = '' OR completed_at < ?)
ORDER BY completed_at DESC, id DESC`
	args = []any{args[0], args[0], args[1], args[1], args[2], args[2]}
	if query.Limit > 0 {
		statement += " LIMIT ?"
		args = append(args, query.Limit)
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("listing efficiency receipts: %w", err)
	}
	defer rows.Close()
	receipts := make([]efficiency.Receipt, 0)
	for rows.Next() {
		receipt, err := scanEfficiencyReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning efficiency receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing efficiency receipts: %w", err)
	}
	return receipts, nil
}

func (s *sqliteStore) EfficiencyRollup(ctx context.Context, query efficiency.Query) (efficiency.Rollup, error) {
	to := query.To
	if to.IsZero() {
		to = time.Now().UTC()
	}
	from := query.From
	if from.IsZero() {
		from = to.Add(-30 * 24 * time.Hour)
	}
	if !from.Before(to) {
		return efficiency.Rollup{}, errors.New("efficiency rollup from must be before to")
	}
	duration := to.Sub(from)
	current, err := s.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: query.ProjectID, From: from, To: to})
	if err != nil {
		return efficiency.Rollup{}, err
	}
	baseline, err := s.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: query.ProjectID, From: from.Add(-duration), To: from})
	if err != nil {
		return efficiency.Rollup{}, err
	}
	return efficiency.Rollup{
		From:       from,
		To:         to,
		Current:    efficiencyRollupWindow(current),
		Baseline:   efficiencyRollupWindow(baseline),
		CacheTrend: efficiencyCacheTrend(current),
	}, nil
}

const efficiencyReceiptSelect = `
SELECT project_id, issue_id, COALESCE(identifier, ''), COALESCE(issue_url, ''), pr_number,
  sessions, attempts, input_tokens, cached_input_tokens, output_tokens,
  reasoning_output_tokens, total_tokens, estimated_cost_usd, first_dispatched_at,
  completed_at, wall_seconds, working_seconds, gate_wait_seconds, merge_train_seconds,
  parked_seconds, redispatches, breaker_trips, ci_reruns, tokens_baseline,
  sessions_baseline, dwell_baseline_seconds, tokens_anomaly, sessions_anomaly, dwell_anomaly
FROM efficiency_receipts`

type receiptScanner interface {
	Scan(...any) error
}

func scanEfficiencyReceipt(scanner receiptScanner) (efficiency.Receipt, error) {
	var receipt efficiency.Receipt
	var prNumber sql.NullInt64
	var firstDispatchedAt string
	var completedAt string
	if err := scanner.Scan(
		&receipt.ProjectID, &receipt.IssueID, &receipt.Identifier, &receipt.IssueURL, &prNumber,
		&receipt.Sessions, &receipt.Attempts, &receipt.InputTokens, &receipt.CachedInputTokens, &receipt.OutputTokens,
		&receipt.ReasoningOutputTokens, &receipt.TotalTokens, &receipt.EstimatedCostUSD, &firstDispatchedAt,
		&completedAt, &receipt.WallSeconds, &receipt.WorkingSeconds, &receipt.GateWaitSeconds, &receipt.MergeTrainSeconds,
		&receipt.ParkedSeconds, &receipt.Redispatches, &receipt.BreakerTrips, &receipt.CIReruns, &receipt.TokensBaseline,
		&receipt.SessionsBaseline, &receipt.DwellBaselineSeconds, &receipt.TokensAnomaly, &receipt.SessionsAnomaly, &receipt.DwellAnomaly,
	); err != nil {
		return efficiency.Receipt{}, err
	}
	if prNumber.Valid {
		receipt.PRNumber = &prNumber.Int64
	}
	var err error
	receipt.FirstDispatchedAt, err = parseStoredTime(firstDispatchedAt)
	if err != nil {
		return efficiency.Receipt{}, fmt.Errorf("parse receipt first_dispatched_at: %w", err)
	}
	receipt.CompletedAt, err = parseStoredTime(completedAt)
	if err != nil {
		return efficiency.Receipt{}, fmt.Errorf("parse receipt completed_at: %w", err)
	}
	return receipt, nil
}

func efficiencyRollupWindow(receipts []efficiency.Receipt) efficiency.RollupWindow {
	window := efficiency.RollupWindow{Issues: int64(len(receipts))}
	if len(receipts) == 0 {
		return window
	}
	tokens := make([]float64, 0, len(receipts))
	costs := make([]float64, 0, len(receipts))
	var inputTokens int64
	var cachedTokens int64
	var sessions int64
	var firstAttempt int64
	for _, receipt := range receipts {
		tokens = append(tokens, float64(receipt.TotalTokens))
		costs = append(costs, receipt.EstimatedCostUSD)
		inputTokens += receipt.InputTokens
		cachedTokens += receipt.CachedInputTokens
		sessions += receipt.Sessions
		if receipt.Attempts == 1 {
			firstAttempt++
		}
		window.Dwell.WorkingSeconds += receipt.WorkingSeconds
		window.Dwell.GateWaitSeconds += receipt.GateWaitSeconds
		window.Dwell.MergeTrainSeconds += receipt.MergeTrainSeconds
		window.Dwell.ParkedSeconds += receipt.ParkedSeconds
		if receipt.Anomalous() {
			window.Anomalies++
		}
	}
	window.TokensPerIssue = receiptPercentiles(tokens)
	window.CostPerIssueUSD = receiptPercentiles(costs)
	if inputTokens > 0 {
		window.CacheShare = float64(cachedTokens) / float64(inputTokens)
	}
	window.SessionsPerIssue = float64(sessions) / float64(len(receipts))
	window.FirstAttemptMergeRate = float64(firstAttempt) / float64(len(receipts))
	return window
}

func receiptPercentiles(values []float64) efficiency.Percentiles {
	if len(values) == 0 {
		return efficiency.Percentiles{}
	}
	sort.Float64s(values)
	return efficiency.Percentiles{P50: percentileValue(values, 0.50), P90: percentileValue(values, 0.90)}
}

func percentileValue(sorted []float64, percentile float64) float64 {
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func efficiencyCacheTrend(receipts []efficiency.Receipt) []efficiency.TrendPoint {
	type totals struct{ input, cached int64 }
	byDay := map[string]totals{}
	for _, receipt := range receipts {
		day := receipt.CompletedAt.UTC().Format("2006-01-02")
		value := byDay[day]
		value.input += receipt.InputTokens
		value.cached += receipt.CachedInputTokens
		byDay[day] = value
	}
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	trend := make([]efficiency.TrendPoint, 0, len(days))
	for _, day := range days {
		value := byDay[day]
		share := 0.0
		if value.input > 0 {
			share = float64(value.cached) / float64(value.input)
		}
		trend = append(trend, efficiency.TrendPoint{Day: day, CacheShare: share})
	}
	return trend
}

func earliestEfficiencyTime(values ...string) (time.Time, error) {
	var earliest time.Time
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		parsed, err := parseStoredTime(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse efficiency dispatch time: %w", err)
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
	}
	return earliest, nil
}

func parseStoredTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func nonNegativeDurationSeconds(from time.Time, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return int64(to.Sub(from) / time.Second)
}

func normalizeEfficiencyDwell(receipt *efficiency.Receipt) {
	if receipt.WallSeconds <= 0 {
		return
	}
	values := []*int64{&receipt.WorkingSeconds, &receipt.GateWaitSeconds, &receipt.MergeTrainSeconds, &receipt.ParkedSeconds}
	var total int64
	for _, value := range values {
		if *value < 0 {
			*value = 0
		}
		total += *value
	}
	if total <= receipt.WallSeconds {
		receipt.WorkingSeconds += receipt.WallSeconds - total
		return
	}
	remaining := receipt.WallSeconds
	for index, value := range values {
		if index == len(values)-1 {
			*value = remaining
			break
		}
		scaled := int64(math.Round(float64(*value) * float64(receipt.WallSeconds) / float64(total)))
		if scaled > remaining {
			scaled = remaining
		}
		*value = scaled
		remaining -= scaled
	}
}

func normalizeEfficiencyThresholds(thresholds efficiency.Thresholds) efficiency.Thresholds {
	if thresholds.TokensMultiple <= 0 {
		thresholds.TokensMultiple = 3
	}
	if thresholds.SessionsMultiple <= 0 {
		thresholds.SessionsMultiple = 3
	}
	if thresholds.DwellMultiple <= 0 {
		thresholds.DwellMultiple = 3
	}
	return thresholds
}

func exceedsBaseline(value float64, baseline float64, multiple float64) bool {
	return baseline > 0 && value > baseline*multiple
}

func timestampFilter(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func nullText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
