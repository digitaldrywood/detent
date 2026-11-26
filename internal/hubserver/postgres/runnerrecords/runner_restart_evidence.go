package runnerrecords

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

func (s *Store) RunnerRollout(ctx context.Context, id string) (runnerstore.Rollout, error) {
	var rollout runnerstore.Rollout
	err := s.db.QueryRow(ctx, `SELECT l.rollout_activation_id,l.rollout_request_id,l.rollout_version,l.rollout_revision
 FROM runner_lifecycle l
 JOIN runner_identities r ON (r.organization_id,r.runner_id)=(l.organization_id,l.runner_id)
 LEFT JOIN runner_update_targets t ON t.organization_id=l.organization_id AND t.runner_id=''
 WHERE l.organization_id=$1 AND l.runner_id=$2 AND r.removed_at IS NULL
 AND l.rollout_activation_id<>'' AND l.rollout_revision=COALESCE(t.revision,0)`, s.organization, id).
		Scan(&rollout.ActivationID, &rollout.RequestID, &rollout.Version, &rollout.OrganizationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return runnerstore.Rollout{}, corestore.ErrNotFound
	}
	return rollout, s.mapError(ctx, err)
}

func (s *Store) RecordRunnerRestartEvidence(ctx context.Context, id, credential string, evidence runnerstore.RestartEvidence, now time.Time) error {
	if err := s.LockRunnerUpdate(ctx, id); err != nil {
		return err
	}
	if err := s.LockOrganizationUpdateTarget(ctx, true); err != nil {
		return err
	}
	rollout, err := s.RunnerRollout(ctx, id)
	if errors.Is(err, corestore.ErrNotFound) {
		return corestore.ErrRevisionConflict
	}
	if err != nil {
		return err
	}
	if rollout.ActivationID != evidence.ActivationID || rollout.Version != evidence.Version {
		return corestore.ErrRevisionConflict
	}
	result, err := s.db.Exec(ctx, `INSERT INTO runner_update_pauses(organization_id,runner_id,version,rollout_revision,activation_id,supervisor_credential_id,supervisor_failed_starts,supervisor_reported_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(organization_id,runner_id,version) DO UPDATE SET rollout_revision=EXCLUDED.rollout_revision,
 activation_id=EXCLUDED.activation_id,supervisor_credential_id=EXCLUDED.supervisor_credential_id,
 supervisor_failed_starts=EXCLUDED.supervisor_failed_starts,supervisor_reported_at=EXCLUDED.supervisor_reported_at
 WHERE runner_update_pauses.activation_id<>EXCLUDED.activation_id
 OR (runner_update_pauses.supervisor_failed_starts>0 AND
     (EXCLUDED.supervisor_failed_starts=0 OR EXCLUDED.supervisor_failed_starts>runner_update_pauses.supervisor_failed_starts))`,
		s.organization, id, rollout.Version, rollout.OrganizationRevision, rollout.ActivationID, credential, evidence.FailedStarts, now)
	if err != nil {
		return s.mapError(ctx, err)
	}
	if result.RowsAffected() != 0 {
		return nil
	}
	var recordedCount int
	err = s.db.QueryRow(ctx, `SELECT supervisor_failed_starts FROM runner_update_pauses
 WHERE organization_id=$1 AND runner_id=$2 AND version=$3 AND activation_id=$4`, s.organization, id, rollout.Version, rollout.ActivationID).Scan(&recordedCount)
	if err != nil {
		return s.mapError(ctx, err)
	}
	if recordedCount != evidence.FailedStarts {
		return corestore.ErrRevisionConflict
	}
	return nil
}
