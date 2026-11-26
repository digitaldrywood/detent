package runnerrecords

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const lifecycleColumns = `process_id,sequence,state,version,build_revision,target_version,reason,error_code,consecutive_failed_starts,last_transition_at`

type runnerLifecycle struct {
	runnerauth.Lifecycle
	rollout                runnerstore.Rollout
	refusedThroughSequence int64
	processGeneration      int64
	acceptedHistorySlot    int16
	lastReportDigest       string
}

func (s *Store) currentLifecycle(ctx context.Context, id string) (*runnerLifecycle, error) {
	var l runnerLifecycle
	err := s.db.QueryRow(ctx, `SELECT `+lifecycleColumns+`,rollout_request_id,rollout_version,rollout_revision,refused_through_sequence,process_generation,accepted_history_slot,last_report_digest FROM runner_lifecycle WHERE organization_id=$1 AND runner_id=$2`, s.organization, id).
		Scan(&l.ProcessID, &l.Sequence, &l.State, &l.Version, &l.BuildRevision, &l.TargetVersion, &l.Reason, &l.ErrorCode, &l.ConsecutiveFailedStarts, &l.LastTransitionAt,
			&l.rollout.RequestID, &l.rollout.Version, &l.rollout.OrganizationRevision, &l.refusedThroughSequence, &l.processGeneration, &l.acceptedHistorySlot, &l.lastReportDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, corestore.ErrNotFound
	}
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return &l, nil
}

func (s *Store) RunnerLifecycle(ctx context.Context, id string) (*runnerauth.Lifecycle, error) {
	current, err := s.currentLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	return &current.Lifecycle, nil
}

func (s *Store) RunnerLifecycleHistory(ctx context.Context, id string, limit int) ([]runnerstore.LifecycleTransition, error) {
	if limit < 1 || limit > 100 {
		return nil, runnerstore.ErrInvalidRecord
	}
	rows, err := s.db.Query(ctx, `SELECT `+lifecycleColumns+`,refusal,rollout_request_id FROM runner_lifecycle_transitions WHERE organization_id=$1 AND runner_id=$2 ORDER BY last_transition_at DESC,transition_id DESC LIMIT $3`, s.organization, id, limit)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	defer rows.Close()
	result := []runnerstore.LifecycleTransition{}
	for rows.Next() {
		var l runnerstore.LifecycleTransition
		if err := rows.Scan(&l.ProcessID, &l.Sequence, &l.State, &l.Version, &l.BuildRevision, &l.TargetVersion, &l.Reason, &l.ErrorCode, &l.ConsecutiveFailedStarts, &l.LastTransitionAt, &l.Refusal, &l.RolloutRequestID); err != nil {
			return nil, s.mapError(ctx, err)
		}
		result = append(result, l)
	}
	return result, s.mapError(ctx, rows.Err())
}

func (s *Store) RunnerLifecycleReceipt(ctx context.Context, id string, r runnerauth.LifecycleReport, digest string) (runnerstore.LifecycleRefusal, bool, error) {
	var refusal pgtype.Text
	err := s.db.QueryRow(ctx, `SELECT COALESCE(
 (SELECT '' FROM runner_lifecycle
  WHERE organization_id=$1 AND runner_id=$2 AND process_id=$3 AND sequence=$4 AND last_report_digest=$5 AND last_report_digest<>''),
 (SELECT refusal FROM runner_lifecycle_transitions
  WHERE organization_id=$1 AND runner_id=$2 AND process_id=$3 AND sequence=$4 AND report_digest=$5 AND report_digest<>''),
 (SELECT refusal FROM runner_lifecycle_transitions
  WHERE organization_id=$1 AND runner_id=$2 AND refusal_slot=($4::bigint % 64)::smallint AND process_id=$3 AND sequence=$4)
 )`, s.organization, id, r.ProcessID, r.Sequence, digest).Scan(&refusal)
	return runnerstore.LifecycleRefusal(refusal.String), refusal.Valid, s.mapError(ctx, err)
}

func (s *Store) RefuseRunnerLifecycle(ctx context.Context, id string, report runnerauth.LifecycleReport, digest string, refusal runnerstore.LifecycleRefusal, now time.Time) error {
	next := runnerLifecycle{Lifecycle: runnerauth.Lifecycle{LifecycleReport: report, LastTransitionAt: now}}
	args := append(lifecycleArgs(string(s.organization), id, next), digest, refusal)
	_, err := s.db.Exec(ctx, `INSERT INTO runner_lifecycle_transitions(organization_id,runner_id,`+lifecycleColumns+`,report_digest,refusal,refusal_slot)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,($4::bigint % 64)::smallint)
 ON CONFLICT(organization_id,runner_id,refusal_slot) WHERE refusal_slot IS NOT NULL DO UPDATE SET
 transition_id=DEFAULT,process_id=EXCLUDED.process_id,sequence=EXCLUDED.sequence,state=EXCLUDED.state,version=EXCLUDED.version,
 build_revision=EXCLUDED.build_revision,target_version=EXCLUDED.target_version,reason=EXCLUDED.reason,error_code=EXCLUDED.error_code,
 last_transition_at=EXCLUDED.last_transition_at,report_digest=EXCLUDED.report_digest,refusal=EXCLUDED.refusal
 WHERE (runner_lifecycle_transitions.process_id,runner_lifecycle_transitions.sequence)<>(EXCLUDED.process_id,EXCLUDED.sequence)`, args...)
	if err != nil {
		return s.mapError(ctx, err)
	}
	_, err = s.db.Exec(ctx, `UPDATE runner_lifecycle SET refused_through_sequence=$4
 WHERE organization_id=$1 AND runner_id=$2 AND process_id=$3 AND refused_through_sequence<$4 AND sequence<$4`, s.organization, id, report.ProcessID, report.Sequence)
	return s.mapError(ctx, err)
}

func (s *Store) ReportRunnerLifecycle(ctx context.Context, id string, r runnerauth.LifecycleReport, previousGeneration int64, digest string, rollout runnerstore.Rollout, now time.Time) error {
	if r.Validate() != nil || now.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	old, err := s.currentLifecycle(ctx, id)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return err
	}
	next := runnerLifecycle{
		Lifecycle:           runnerauth.Lifecycle{LifecycleReport: r, LastTransitionAt: now},
		acceptedHistorySlot: 63, processGeneration: 1, lastReportDigest: digest,
	}
	if old != nil {
		next.acceptedHistorySlot = old.acceptedHistorySlot
		next.processGeneration = old.processGeneration
	}
	if old != nil && old.ProcessID == r.ProcessID {
		if r.Sequence <= old.Sequence || r.Sequence <= old.refusedThroughSequence || old.Version != r.Version || old.BuildRevision != r.BuildRevision || !lifecycleTransition(old.State, r.State) {
			return corestore.ErrRevisionConflict
		}
		next.ConsecutiveFailedStarts = old.ConsecutiveFailedStarts
		next.refusedThroughSequence = old.refusedThroughSequence
		next.rollout = old.rollout
		if r.State == runnerauth.LifecycleShuttingDown && r.Reason == "update" &&
			(old.rollout.RequestID == "" || r.TargetVersion != old.rollout.Version) {
			return corestore.ErrRevisionConflict
		}
		if r.State == runnerauth.LifecycleUpdating {
			next.rollout = rollout
		}
		if old.State == runnerauth.LifecycleStarting && r.State == runnerauth.LifecycleStartFailed {
			next.ConsecutiveFailedStarts++
		}
	} else {
		if r.State != runnerauth.LifecycleStarting || r.Sequence != 1 {
			return corestore.ErrRevisionConflict
		}
		var generation int64
		if old != nil {
			generation = old.processGeneration
		}
		if previousGeneration != generation {
			if previousGeneration == 0 {
				return runnerstore.LifecycleStartConflict{Generation: generation}
			}
			return corestore.ErrRevisionConflict
		}
		next.processGeneration = generation + 1
		if old != nil && old.State == runnerauth.LifecycleShuttingDown {
			stopped := *old
			stopped.State = runnerauth.LifecycleStopped
			stopped.LastTransitionAt = now
			if err := s.storeRunnerLifecycle(ctx, id, &stopped, ""); err != nil {
				return err
			}
			next.acceptedHistorySlot = stopped.acceptedHistorySlot
		}
		if old != nil && old.rollout.Version == r.Version {
			next.rollout = old.rollout
		}
		if old != nil && old.Version == r.Version {
			next.ConsecutiveFailedStarts = old.ConsecutiveFailedStarts
			if old.State == runnerauth.LifecycleStarting {
				next.ConsecutiveFailedStarts++
			}
		}
		if next.ConsecutiveFailedStarts >= 2 {
			next.ConsecutiveFailedStarts++
			next.State = runnerauth.LifecycleStartFailed
			next.ErrorCode = "repeated_start"
		}
	}
	if r.State == runnerauth.LifecycleReady {
		next.ConsecutiveFailedStarts = 0
		next.rollout = runnerstore.Rollout{}
	}
	if old != nil && old.ProcessID == r.ProcessID {
		prior := old.LifecycleReport
		prior.Sequence = next.Sequence
		if prior == next.LifecycleReport && old.rollout == next.rollout && old.ConsecutiveFailedStarts == next.ConsecutiveFailedStarts {
			_, err := s.db.Exec(ctx, `UPDATE runner_lifecycle SET sequence=$3,last_report_digest=$4 WHERE organization_id=$1 AND runner_id=$2`, s.organization, id, r.Sequence, digest)
			return s.mapError(ctx, err)
		}
	}
	return s.storeRunnerLifecycle(ctx, id, &next, digest)
}

func lifecycleArgs(organization string, id string, next runnerLifecycle) []any {
	return []any{organization, id, next.ProcessID, next.Sequence, next.State, next.Version, next.BuildRevision, next.TargetVersion, next.Reason, next.ErrorCode, next.ConsecutiveFailedStarts, next.LastTransitionAt}
}

func (s *Store) storeRunnerLifecycle(ctx context.Context, id string, next *runnerLifecycle, digest string) error {
	next.acceptedHistorySlot = (next.acceptedHistorySlot + 1) % 64
	args := append(lifecycleArgs(string(s.organization), id, *next), digest, next.rollout.RequestID, next.acceptedHistorySlot)
	_, err := s.db.Exec(ctx, `INSERT INTO runner_lifecycle_transitions(organization_id,runner_id,`+lifecycleColumns+`,report_digest,rollout_request_id,accepted_slot)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
 ON CONFLICT(organization_id,runner_id,accepted_slot) WHERE accepted_slot IS NOT NULL DO UPDATE SET
 transition_id=DEFAULT,process_id=EXCLUDED.process_id,sequence=EXCLUDED.sequence,state=EXCLUDED.state,version=EXCLUDED.version,
 build_revision=EXCLUDED.build_revision,target_version=EXCLUDED.target_version,reason=EXCLUDED.reason,error_code=EXCLUDED.error_code,
 consecutive_failed_starts=EXCLUDED.consecutive_failed_starts,last_transition_at=EXCLUDED.last_transition_at,
 report_digest=EXCLUDED.report_digest,rollout_request_id=EXCLUDED.rollout_request_id`, args...)
	if err != nil {
		return s.mapError(ctx, err)
	}
	args = append(lifecycleArgs(string(s.organization), id, *next), next.rollout.RequestID, next.rollout.Version, next.rollout.OrganizationRevision, next.refusedThroughSequence, next.processGeneration, next.acceptedHistorySlot, next.lastReportDigest)
	_, err = s.db.Exec(ctx, `INSERT INTO runner_lifecycle(organization_id,runner_id,`+lifecycleColumns+`,rollout_request_id,rollout_version,rollout_revision,refused_through_sequence,process_generation,accepted_history_slot,last_report_digest)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
 ON CONFLICT(organization_id,runner_id) DO UPDATE SET process_id=EXCLUDED.process_id,sequence=EXCLUDED.sequence,state=EXCLUDED.state,version=EXCLUDED.version,
 build_revision=EXCLUDED.build_revision,target_version=EXCLUDED.target_version,reason=EXCLUDED.reason,error_code=EXCLUDED.error_code,
 consecutive_failed_starts=EXCLUDED.consecutive_failed_starts,last_transition_at=EXCLUDED.last_transition_at,rollout_request_id=EXCLUDED.rollout_request_id,
 rollout_version=EXCLUDED.rollout_version,rollout_revision=EXCLUDED.rollout_revision,refused_through_sequence=EXCLUDED.refused_through_sequence,
 process_generation=EXCLUDED.process_generation,accepted_history_slot=EXCLUDED.accepted_history_slot,last_report_digest=EXCLUDED.last_report_digest`, args...)
	if err != nil {
		return s.mapError(ctx, err)
	}
	switch {
	case next.State == runnerauth.LifecycleReady:
		_, err = s.db.Exec(ctx, `DELETE FROM runner_update_pauses WHERE organization_id=$1 AND runner_id=$2 AND version=$3`, s.organization, id, next.Version)
	case next.State == runnerauth.LifecycleStartFailed && next.ConsecutiveFailedStarts >= 3 && next.rollout.RequestID != "" && next.Version == next.rollout.Version:
		if err := s.LockOrganizationUpdateTarget(ctx, true); err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO runner_update_pauses(organization_id,runner_id,version,rollout_revision,machine_id)
 SELECT organization_id,runner_id,$3,$4,machine_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2
 ON CONFLICT(organization_id,runner_id,version) DO UPDATE SET rollout_revision=EXCLUDED.rollout_revision,machine_id=EXCLUDED.machine_id
 WHERE runner_update_pauses.rollout_revision<EXCLUDED.rollout_revision`, s.organization, id, next.Version, next.rollout.OrganizationRevision)
	}
	return s.mapError(ctx, err)
}

func lifecycleTransition(from, to runnerauth.LifecycleState) bool {
	if from == to {
		return to != runnerauth.LifecycleStarting
	}
	switch from {
	case runnerauth.LifecycleStarting, runnerauth.LifecycleStartFailed:
		return to == runnerauth.LifecycleReady || to == runnerauth.LifecycleStartFailed || to == runnerauth.LifecycleShuttingDown
	case runnerauth.LifecycleReady:
		return to == runnerauth.LifecycleDraining || to == runnerauth.LifecycleUpdating || to == runnerauth.LifecycleShuttingDown
	case runnerauth.LifecycleDraining:
		return to == runnerauth.LifecycleUpdating || to == runnerauth.LifecycleShuttingDown || to == runnerauth.LifecycleReady
	case runnerauth.LifecycleUpdating:
		return to == runnerauth.LifecycleShuttingDown || to == runnerauth.LifecycleReady
	case runnerauth.LifecycleShuttingDown:
		return to == runnerauth.LifecycleStopped || to == runnerauth.LifecycleReady
	}
	return false
}

func (s *Store) FillLifecycles(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	rows, err := s.db.Query(ctx, `SELECT runner_id,`+lifecycleColumns+` FROM runner_lifecycle WHERE organization_id=$1 AND runner_id=ANY($2)`, s.organization, ids)
	if err != nil {
		return s.mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var l runnerauth.Lifecycle
		if err := rows.Scan(&id, &l.ProcessID, &l.Sequence, &l.State, &l.Version, &l.BuildRevision, &l.TargetVersion, &l.Reason, &l.ErrorCode, &l.ConsecutiveFailedStarts, &l.LastTransitionAt); err != nil {
			return s.mapError(ctx, err)
		}
		records[indices[id]].Lifecycle = &l
	}
	return s.mapError(ctx, rows.Err())
}

func (s *Store) RunnerStartDeadlines(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, runnerstore.ErrInvalidRecord
	}
	rows, err := s.db.Query(ctx, `SELECT runner_id FROM runner_lifecycle WHERE organization_id=$1 AND state IN ('starting','shutting_down') AND last_transition_at <= $2 ORDER BY last_transition_at,runner_id LIMIT $3`, s.organization, now.Add(-runnerauth.RunnerReadyTimeout), limit)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, s.mapError(ctx, err)
		}
		ids = append(ids, id)
	}
	return ids, s.mapError(ctx, rows.Err())
}

func (s *Store) ExpireRunnerStart(ctx context.Context, id string, now time.Time) error {
	if err := s.LockRunnerUpdate(ctx, id); err != nil {
		return err
	}
	current, err := s.currentLifecycle(ctx, id)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current == nil || current.State != runnerauth.LifecycleStarting && current.State != runnerauth.LifecycleShuttingDown || now.Before(current.LastTransitionAt.Add(runnerauth.RunnerReadyTimeout)) {
		return nil
	}
	if current.State == runnerauth.LifecycleShuttingDown {
		current.State = runnerauth.LifecycleStopped
	} else {
		current.Lifecycle = current.Effective(now)
	}
	current.LastTransitionAt = now
	return s.storeRunnerLifecycle(ctx, id, current, "")
}
