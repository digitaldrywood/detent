package store

import (
	"context"
	"time"
)

func (s *sqliteStore) IssueContractRolloutAt(ctx context.Context) (time.Time, error) {
	var at string
	if err := s.db.QueryRowContext(ctx, "SELECT activated_at FROM issue_contract_rollout").Scan(&at); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, at)
}
