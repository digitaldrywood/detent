package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

// CardHistory contains durable facts that must not depend on runtime history limits.
// Days use UTC, consistently across the dashboard and API.
type CardHistory struct {
	AttemptsToday int64
	LaneReason    string
	LaneReasonAt  *time.Time
}

type CardHistoryStore interface {
	IssueCardHistory(context.Context, IssueIdentity, time.Time) (CardHistory, error)
}

func (s *sqliteStore) IssueCardHistory(ctx context.Context, issue IssueIdentity, now time.Time) (CardHistory, error) {
	issue = normalizeIssueIdentity(issue)
	if issue.ProjectID == "" {
		return CardHistory{}, ErrProjectRequired
	}
	day := now.UTC().Truncate(24 * time.Hour)
	var result CardHistory
	var err error
	result.AttemptsToday, err = s.queries.IssueCardAttemptsToday(ctx, sqlc.IssueCardAttemptsTodayParams{
		ProjectID: issue.ProjectID, IssueID: issue.IssueID, Identifier: issue.Identifier, IssueURL: issue.IssueURL,
		FromTime: day.Format(time.RFC3339Nano), ToTime: day.Add(24 * time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		return CardHistory{}, fmt.Errorf("count card attempts: %w", err)
	}
	row, err := s.queries.IssueCardLaneReason(ctx, sqlc.IssueCardLaneReasonParams{
		ProjectID: issue.ProjectID, IssueID: issue.IssueID, Identifier: issue.Identifier, IssueURL: issue.IssueURL,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return CardHistory{}, fmt.Errorf("read card lane reason: %w", err)
	}
	result.LaneReason = row.Reason
	at, err := parseTimestamp("recorded_at", row.RecordedAt)
	if err != nil {
		return CardHistory{}, err
	}
	result.LaneReasonAt = &at
	return result, nil
}
