package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

type ProjectLifetimeUsage struct {
	ProjectID       string
	CompletedIssues int64
	MeanSessions    float64
	MeanTokens      float64
	P95Sessions     int64
	P95Tokens       int64
}

type ProjectLifetimeUsageStore interface {
	ProjectLifetimeUsage(context.Context, string) (ProjectLifetimeUsage, error)
}

type ProjectLifetimeUsageQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *sqliteStore) ProjectLifetimeUsage(ctx context.Context, projectID string) (ProjectLifetimeUsage, error) {
	return QueryProjectLifetimeUsage(ctx, s.db, projectID)
}

func QueryProjectLifetimeUsage(ctx context.Context, queryer ProjectLifetimeUsageQuerier, projectID string) (ProjectLifetimeUsage, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ProjectLifetimeUsage{}, ErrProjectRequired
	}
	rows, err := queryer.QueryContext(ctx, `
SELECT sessions, total_tokens
FROM efficiency_receipts
WHERE project_id = ? AND in_progress = 0
ORDER BY id`, projectID)
	if err != nil {
		return ProjectLifetimeUsage{}, fmt.Errorf("querying project lifetime usage: %w", err)
	}
	defer rows.Close()

	sessions := make([]int64, 0)
	tokens := make([]int64, 0)
	var sessionTotal int64
	var tokenTotal int64
	for rows.Next() {
		var issueSessions int64
		var issueTokens int64
		if err := rows.Scan(&issueSessions, &issueTokens); err != nil {
			return ProjectLifetimeUsage{}, fmt.Errorf("scanning project lifetime usage: %w", err)
		}
		sessions = append(sessions, issueSessions)
		tokens = append(tokens, issueTokens)
		sessionTotal += issueSessions
		tokenTotal += issueTokens
	}
	if err := rows.Err(); err != nil {
		return ProjectLifetimeUsage{}, fmt.Errorf("reading project lifetime usage: %w", err)
	}

	usage := ProjectLifetimeUsage{ProjectID: projectID, CompletedIssues: int64(len(sessions))}
	if len(sessions) == 0 {
		return usage, nil
	}
	usage.MeanSessions = float64(sessionTotal) / float64(len(sessions))
	usage.MeanTokens = float64(tokenTotal) / float64(len(tokens))
	usage.P95Sessions = nearestRankPercentile(sessions, 0.95)
	usage.P95Tokens = nearestRankPercentile(tokens, 0.95)
	return usage, nil
}

func nearestRankPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
