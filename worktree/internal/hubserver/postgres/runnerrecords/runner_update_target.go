package runnerrecords

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

func (s *Store) RunnerUpdateTarget(ctx context.Context, id string) (runnerstore.UpdateTarget, error) {
	var result runnerstore.UpdateTarget
	err := s.db.QueryRow(ctx, `SELECT version,revision FROM runner_update_targets WHERE organization_id=$1 AND runner_id=$2`, s.organization, id).Scan(&result.Version, &result.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	return result, s.mapError(ctx, err)
}

func (s *Store) SetRunnerUpdateTarget(ctx context.Context, id string, target runnerstore.UpdateTarget) error {
	if target.Revision < 0 {
		return runnerstore.ErrInvalidRecord
	}
	if id != "" {
		if _, err := s.identity(ctx, id); err != nil {
			return err
		}
	}
	result, err := s.db.Exec(ctx, `INSERT INTO runner_update_targets(organization_id,runner_id,version,revision) SELECT $1,$2,$3,1 WHERE $4::bigint=0 ON CONFLICT(organization_id,runner_id) DO NOTHING`, s.organization, id, target.Version, target.Revision)
	if err != nil {
		return s.mapError(ctx, err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	result, err = s.db.Exec(ctx, `UPDATE runner_update_targets SET version=$3,revision=revision+1 WHERE organization_id=$1 AND runner_id=$2 AND revision=$4`, s.organization, id, target.Version, target.Revision)
	if err != nil {
		return s.mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *Store) RunnerVersionFailed(ctx context.Context, version string) (bool, error) {
	var failed bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runner_lifecycle_transitions WHERE organization_id=$1 AND version=$2 AND consecutive_failed_starts>=3)`, s.organization, version).Scan(&failed)
	return failed, s.mapError(ctx, err)
}
