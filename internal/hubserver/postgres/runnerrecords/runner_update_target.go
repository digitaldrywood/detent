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
		if err := s.LockRunnerUpdate(ctx, id); err != nil {
			return err
		}
		if _, err := s.identity(ctx, id); err != nil {
			return err
		}
	} else if err := s.LockOrganizationUpdateTarget(ctx, true); err != nil {
		return err
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

const currentHoldOfferSQL = `(d.hold_revision=0
 OR (d.organization_hold AND d.hold_revision=o.revision AND d.version=o.version)
 OR (NOT d.organization_hold AND d.hold_organization_revision=COALESCE(o.revision,0) AND COALESCE(o.version,'')='' AND d.hold_revision=t.revision AND d.version=t.version))`

const pauseScopeSQL = `p.organization_id=candidate.organization_id AND p.version=candidate.version AND p.rollout_revision=candidate.revision`

const pausedForRunnerSQL = `EXISTS(SELECT 1 FROM runner_update_pauses p
 WHERE p.organization_id=candidate.organization_id AND p.runner_id=candidate.runner_id AND p.version=candidate.version AND p.rollout_revision=candidate.revision)
 OR COALESCE(
 (SELECT p.machine_id FROM runner_update_pauses p WHERE ` + pauseScopeSQL + ` ORDER BY p.machine_id ASC LIMIT 1)
 <> (SELECT p.machine_id FROM runner_update_pauses p WHERE ` + pauseScopeSQL + ` ORDER BY p.machine_id DESC LIMIT 1), false)`

func (s *Store) RunnerVersionFailed(ctx context.Context, runner, version string) (bool, error) {
	var failed bool
	err := s.db.QueryRow(ctx, `SELECT `+pausedForRunnerSQL+`
 FROM (SELECT $1::text AS organization_id,$2::text AS runner_id,$3::text AS version,
 COALESCE((SELECT revision FROM runner_update_targets WHERE organization_id=$1 AND runner_id=''),0) AS revision) candidate`, s.organization, runner, version).Scan(&failed)
	return failed, s.mapError(ctx, err)
}

func (s *Store) LockOrganizationUpdateTarget(ctx context.Context, exclusive bool) error {
	query := `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR SHARE`
	if exclusive {
		query = `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR NO KEY UPDATE`
	}
	var id string
	return s.mapError(ctx, s.db.QueryRow(ctx, query, s.organization).Scan(&id))
}
