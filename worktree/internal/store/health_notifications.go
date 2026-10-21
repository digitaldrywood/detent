package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) ListHealthNotificationStates(ctx context.Context) ([]HealthNotificationState, error) {
	rows, err := s.queries.ListHealthNotificationStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list health notification states: %w", err)
	}
	states := make([]HealthNotificationState, 0, len(rows))
	for _, row := range rows {
		updatedAt, err := parseTimestamp("updated_at", row.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse health notification state %s updated_at: %w", row.Identity, err)
		}
		states = append(states, HealthNotificationState{
			Identity:  row.Identity,
			StateJSON: []byte(row.StateJson),
			UpdatedAt: updatedAt,
		})
	}
	return states, nil
}

func (s *sqliteStore) SaveHealthNotificationStates(ctx context.Context, states []HealthNotificationState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin health notification state transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	queries := s.queries.WithTx(tx)
	for _, state := range states {
		identity := strings.TrimSpace(state.Identity)
		if identity == "" {
			return errors.New("health notification identity is required")
		}
		updatedAt, err := requiredTimestamp("updated_at", state.UpdatedAt)
		if err != nil {
			return err
		}
		if err := queries.UpsertHealthNotificationState(ctx, sqlc.UpsertHealthNotificationStateParams{
			Identity:  identity,
			StateJson: string(state.StateJSON),
			UpdatedAt: updatedAt,
		}); err != nil {
			return fmt.Errorf("save health notification state %s: %w", identity, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit health notification states: %w", err)
	}
	return nil
}
