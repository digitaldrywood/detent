package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

type costPerOutcomeUsage struct {
	projectID   string
	totalTokens int64
	spendUSD    float64
	at          time.Time
}

type costPerOutcomeCompletion struct {
	projectID string
	mergedPR  bool
	at        time.Time
}

const costPerOutcomeUsageSQL = `
SELECT project_id, total_tokens, cost_usd, finished_at
FROM usage_events
WHERE finished_at >= ? AND finished_at < ?
ORDER BY finished_at, id`

const costPerOutcomeProjectUsageSQL = `
SELECT project_id, total_tokens, cost_usd, finished_at
FROM usage_events
WHERE project_id = ? AND finished_at >= ? AND finished_at < ?
ORDER BY finished_at, id`

const costPerOutcomeCompletionsSQL = `
SELECT project_id, pr_number, completed_at
FROM efficiency_receipts
WHERE completed_at >= ? AND completed_at < ?
ORDER BY completed_at, id`

const costPerOutcomeProjectCompletionsSQL = `
SELECT project_id, pr_number, completed_at
FROM efficiency_receipts
WHERE project_id = ? AND completed_at >= ? AND completed_at < ?
ORDER BY completed_at, id`

func (s *sqliteStore) CostPerOutcome(ctx context.Context, query efficiency.CostPerOutcomeQuery) (efficiency.CostPerOutcomeReport, error) {
	if err := validateCostPerOutcomeQuery(query); err != nil {
		return efficiency.CostPerOutcomeReport{}, err
	}
	usage, err := s.costPerOutcomeUsage(ctx, query)
	if err != nil {
		return efficiency.CostPerOutcomeReport{}, err
	}
	completions, err := s.costPerOutcomeCompletions(ctx, query)
	if err != nil {
		return efficiency.CostPerOutcomeReport{}, err
	}
	return aggregateCostPerOutcome(query, usage, completions)
}

func (s *sqliteStore) costPerOutcomeUsage(ctx context.Context, query efficiency.CostPerOutcomeQuery) ([]costPerOutcomeUsage, error) {
	from, to := costPerOutcomeScanBounds(query)
	projectID := strings.TrimSpace(query.ProjectID)
	statement := costPerOutcomeUsageSQL
	args := []any{from, to}
	if projectID != "" {
		statement = costPerOutcomeProjectUsageSQL
		args = []any{projectID, from, to}
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("reading cost-per-outcome usage: %w", err)
	}
	defer rows.Close()

	usage := make([]costPerOutcomeUsage, 0)
	for rows.Next() {
		var sample costPerOutcomeUsage
		var at string
		if err := rows.Scan(&sample.projectID, &sample.totalTokens, &sample.spendUSD, &at); err != nil {
			return nil, fmt.Errorf("scanning cost-per-outcome usage: %w", err)
		}
		sample.at, err = parseStoredTime(at)
		if err != nil {
			return nil, fmt.Errorf("parsing cost-per-outcome usage time: %w", err)
		}
		usage = append(usage, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading cost-per-outcome usage rows: %w", err)
	}
	return usage, nil
}

func (s *sqliteStore) costPerOutcomeCompletions(ctx context.Context, query efficiency.CostPerOutcomeQuery) ([]costPerOutcomeCompletion, error) {
	from, to := costPerOutcomeScanBounds(query)
	projectID := strings.TrimSpace(query.ProjectID)
	statement := costPerOutcomeCompletionsSQL
	args := []any{from, to}
	if projectID != "" {
		statement = costPerOutcomeProjectCompletionsSQL
		args = []any{projectID, from, to}
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("reading cost-per-outcome completions: %w", err)
	}
	defer rows.Close()

	completions := make([]costPerOutcomeCompletion, 0)
	for rows.Next() {
		var completion costPerOutcomeCompletion
		var prNumber sql.NullInt64
		var at string
		if err := rows.Scan(&completion.projectID, &prNumber, &at); err != nil {
			return nil, fmt.Errorf("scanning cost-per-outcome completion: %w", err)
		}
		completion.mergedPR = prNumber.Valid && prNumber.Int64 > 0
		completion.at, err = parseStoredTime(at)
		if err != nil {
			return nil, fmt.Errorf("parsing cost-per-outcome completion time: %w", err)
		}
		completions = append(completions, completion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading cost-per-outcome completion rows: %w", err)
	}
	return completions, nil
}

type costPerOutcomeAccumulator struct {
	current efficiency.CostPerOutcomeMetrics
	trend   []efficiency.CostPerOutcomePoint
}

func aggregateCostPerOutcome(query efficiency.CostPerOutcomeQuery, usage []costPerOutcomeUsage, completions []costPerOutcomeCompletion) (efficiency.CostPerOutcomeReport, error) {
	if err := validateCostPerOutcomeQuery(query); err != nil {
		return efficiency.CostPerOutcomeReport{}, err
	}
	projectID := strings.TrimSpace(query.ProjectID)
	bucketCount := int((query.To.Sub(query.From)-1)/query.Bucket) + 1
	projects := map[string]*costPerOutcomeAccumulator{}
	project := func(id string) *costPerOutcomeAccumulator {
		id = strings.TrimSpace(id)
		if id == "" {
			id = "unassigned"
		}
		if projects[id] != nil {
			return projects[id]
		}
		trend := make([]efficiency.CostPerOutcomePoint, bucketCount)
		for index := range trend {
			from := query.From.Add(time.Duration(index) * query.Bucket)
			to := from.Add(query.Bucket)
			if to.After(query.To) {
				to = query.To
			}
			trend[index] = efficiency.CostPerOutcomePoint{From: from, To: to}
		}
		projects[id] = &costPerOutcomeAccumulator{trend: trend}
		return projects[id]
	}
	withinWindow := func(at time.Time) bool {
		return !at.Before(query.From) && at.Before(query.To)
	}
	bucketIndex := func(at time.Time) int {
		return int(at.Sub(query.From) / query.Bucket)
	}

	for _, sample := range usage {
		if !withinWindow(sample.at) || (projectID != "" && strings.TrimSpace(sample.projectID) != projectID) {
			continue
		}
		value := project(sample.projectID)
		tokens := max(sample.totalTokens, 0)
		spend := max(sample.spendUSD, 0)
		value.current.TotalTokens += tokens
		value.current.SpendUSD += spend
		point := &value.trend[bucketIndex(sample.at)].Metrics
		point.TotalTokens += tokens
		point.SpendUSD += spend
	}
	for _, completion := range completions {
		if !withinWindow(completion.at) || (projectID != "" && strings.TrimSpace(completion.projectID) != projectID) {
			continue
		}
		value := project(completion.projectID)
		value.current.ClosedIssues++
		point := &value.trend[bucketIndex(completion.at)].Metrics
		point.ClosedIssues++
		if completion.mergedPR {
			value.current.MergedPRs++
			point.MergedPRs++
		}
	}

	ids := make([]string, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	report := efficiency.CostPerOutcomeReport{From: query.From, To: query.To, Projects: make([]efficiency.CostPerOutcomeProject, 0, len(ids))}
	for _, id := range ids {
		value, ok := projects[id]
		if !ok || value == nil {
			continue
		}
		finalizeCostPerOutcomeMetrics(&value.current)
		for index := range value.trend {
			finalizeCostPerOutcomeMetrics(&value.trend[index].Metrics)
		}
		report.Projects = append(report.Projects, efficiency.CostPerOutcomeProject{ProjectID: id, Current: value.current, Trend: value.trend})
	}
	return report, nil
}

func validateCostPerOutcomeQuery(query efficiency.CostPerOutcomeQuery) error {
	if query.From.IsZero() || query.To.IsZero() || !query.From.Before(query.To) {
		return errors.New("cost-per-outcome window must have increasing boundaries")
	}
	if query.Bucket <= 0 {
		return errors.New("cost-per-outcome bucket must be positive")
	}
	return nil
}

func costPerOutcomeScanBounds(query efficiency.CostPerOutcomeQuery) (string, string) {
	from := query.From.UTC().Truncate(time.Second).Add(-time.Second)
	to := query.To.UTC().Truncate(time.Second).Add(time.Second)
	return from.Format(time.RFC3339), to.Format(time.RFC3339)
}

func finalizeCostPerOutcomeMetrics(metrics *efficiency.CostPerOutcomeMetrics) {
	if metrics.MergedPRs > 0 {
		metrics.TokensPerMergedPR = float64(metrics.TotalTokens) / float64(metrics.MergedPRs)
		metrics.SpendPerMergedPRUSD = metrics.SpendUSD / float64(metrics.MergedPRs)
	}
	if metrics.ClosedIssues > 0 {
		metrics.TokensPerClosedIssue = float64(metrics.TotalTokens) / float64(metrics.ClosedIssues)
		metrics.SpendPerClosedIssueUSD = metrics.SpendUSD / float64(metrics.ClosedIssues)
	}
}
