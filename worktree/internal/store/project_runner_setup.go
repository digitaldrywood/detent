package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) ProjectRunnerSetupHash(ctx context.Context, runnerID, projectID string) (string, error) {
	hash, err := s.queries.GetProjectRunnerSetupHash(ctx, sqlc.GetProjectRunnerSetupHashParams{RunnerID: runnerID, ProjectID: projectID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read runner project setup hash: %w", err)
	}
	return hash, nil
}

func (s *sqliteStore) SetProjectRunnerSetupHash(ctx context.Context, runnerID, projectID, hash string) error {
	if err := s.queries.SetProjectRunnerSetupHash(ctx, sqlc.SetProjectRunnerSetupHashParams{RunnerID: runnerID, ProjectID: projectID, ContentHash: hash}); err != nil {
		return fmt.Errorf("record runner project setup hash: %w", err)
	}
	return nil
}
