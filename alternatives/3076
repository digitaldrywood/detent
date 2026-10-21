package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ScratchRetentionState resolves the random scratch UUID via the session's
// recorded path, including already-reaped processes. UUIDs are not attempt IDs.
func (s *sqliteStore) ScratchRetentionState(ctx context.Context, path string) (bool, bool, error) {
	terminal, err := s.queries.ScratchRetentionState(ctx, sql.NullString{String: path, Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read scratch ownership: %w", err)
	}
	return true, terminal != 0, nil
}
