package runnerrecords

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const lifecycleColumns = `process_id,sequence,state,version,build_revision,target_version,reason,error_code,consecutive_failed_starts,last_transition_at`

func scanLifecycle(row pgx.Row) (runnerauth.Lifecycle, error) {
	var l runnerauth.Lifecycle
	err := row.Scan(&l.ProcessID, &l.Sequence, &l.State, &l.Version, &l.BuildRevision, &l.TargetVersion, &l.Reason, &l.ErrorCode, &l.ConsecutiveFailedStarts, &l.LastTransitionAt)
	return l, err
}

func (s *Store) RunnerLifecycle(ctx context.Context, id string) (*runnerauth.Lifecycle, error) {
	l, err := scanLifecycle(s.db.QueryRow(ctx, `SELECT `+lifecycleColumns+` FROM runner_lifecycle WHERE organization_id=$1 AND runner_id=$2`, s.organization, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, corestore.ErrNotFound
	}
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return &l, nil
}

func (s *Store) RunnerLifecycleHistory(ctx context.Context, id string, limit int) ([]runnerstore.LifecycleTransition, error) {
	if limit < 1 || limit > 100 {
		return nil, runnerstore.ErrInvalidRecord
	}
	rows, err := s.db.Query(ctx, `SELECT `+lifecycleColumns+`,refusal FROM runner_lifecycle_transitions WHERE organization_id=$1 AND runner_id=$2 ORDER BY last_transition_at DESC,transition_id DESC LIMIT $3`, s.organization, id, limit)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	defer rows.Close()
	result := []runnerstore.LifecycleTransition{}
	for rows.Next() {
		var l runnerstore.LifecycleTransition
		if err := rows.Scan(&l.ProcessID, &l.Sequence, &l.State, &l.Version, &l.BuildRevision, &l.TargetVersion, &l.Reason, &l.ErrorCode, &l.ConsecutiveFailedStarts, &l.LastTransitionAt, &l.Refusal); err != nil {
			return nil, s.mapError(ctx, err)
		}
		result = append(result, l)
	}
	return result, s.mapError(ctx, rows.Err())
}

func (s *Store) RunnerLifecycleReceipt(ctx context.Context, id string, r runnerauth.LifecycleReport, digest string) (runnerstore.LifecycleRefusal, bool, error) {
	var refusal runnerstore.LifecycleRefusal
	err := s.db.QueryRow(ctx, `SELECT refusal FROM runner_lifecycle_transitions WHERE organization_id=$1 AND runner_id=$2 AND process_id=$3 AND sequence=$4 AND report_digest=$5 AND report_digest <> ''`, s.organization, id, r.ProcessID, r.Sequence, digest).Scan(&refusal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return refusal, err == nil, s.mapError(ctx, err)
}

func (s *Store) RefuseRunnerLifecycle(ctx context.Context, id string, report runnerauth.LifecycleReport, digest string, refusal runnerstore.LifecycleRefusal, now time.Time) error {
	return s.appendRunnerLifecycle(ctx, id, runnerauth.Lifecycle{LifecycleReport: report, LastTransitionAt: now}, digest, refusal)
}

func (s *Store) ReportRunnerLifecycle(ctx context.Context, id string, r runnerauth.LifecycleReport, digest string, now time.Time) error {
	if r.Validate() != nil || now.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	old, err := s.RunnerLifecycle(ctx, id)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return err
	}
	next := runnerauth.Lifecycle{LifecycleReport: r, LastTransitionAt: now}
	if old != nil && old.ProcessID == r.ProcessID {
		if r.Sequence <= old.Sequence || old.Version != r.Version || old.BuildRevision != r.BuildRevision || !lifecycleTransition(old.State, r.State) {
			return corestore.ErrRevisionConflict
		}
		next.ConsecutiveFailedStarts = old.ConsecutiveFailedStarts
		if old.State == runnerauth.LifecycleStarting && r.State == runnerauth.LifecycleStartFailed {
			next.ConsecutiveFailedStarts++
		}
	} else {
		if r.State != runnerauth.LifecycleStarting || r.Sequence != 1 {
			return corestore.ErrRevisionConflict
		}
		var seen bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runner_lifecycle_transitions WHERE organization_id=$1 AND runner_id=$2 AND process_id=$3 AND refusal='')`, s.organization, id, r.ProcessID).Scan(&seen); err != nil {
			return s.mapError(ctx, err)
		}
		if seen {
			return corestore.ErrRevisionConflict
		}
		if old != nil && old.State == runnerauth.LifecycleShuttingDown {
			stopped := *old
			stopped.State = runnerauth.LifecycleStopped
			stopped.LastTransitionAt = now
			if err := s.storeRunnerLifecycle(ctx, id, stopped, ""); err != nil {
				return err
			}
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
	}
	return s.storeRunnerLifecycle(ctx, id, next, digest)
}

func lifecycleArgs(organization string, id string, next runnerauth.Lifecycle) []any {
	return []any{organization, id, next.ProcessID, next.Sequence, next.State, next.Version, next.BuildRevision, next.TargetVersion, next.Reason, next.ErrorCode, next.ConsecutiveFailedStarts, next.LastTransitionAt}
}

func (s *Store) appendRunnerLifecycle(ctx context.Context, id string, next runnerauth.Lifecycle, digest string, refusal runnerstore.LifecycleRefusal) error {
	args := append(lifecycleArgs(string(s.organization), id, next), digest, refusal)
	_, err := s.db.Exec(ctx, `INSERT INTO runner_lifecycle_transitions(organization_id,runner_id,`+lifecycleColumns+`,report_digest,refusal) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, args...)
	return s.mapError(ctx, err)
}

func (s *Store) storeRunnerLifecycle(ctx context.Context, id string, next runnerauth.Lifecycle, digest string) error {
	if err := s.appendRunnerLifecycle(ctx, id, next, digest, ""); err != nil {
		return err
	}
	args := lifecycleArgs(string(s.organization), id, next)
	_, err := s.db.Exec(ctx, `INSERT INTO runner_lifecycle(organization_id,runner_id,`+lifecycleColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(organization_id,runner_id) DO UPDATE SET process_id=EXCLUDED.process_id,sequence=EXCLUDED.sequence,state=EXCLUDED.state,version=EXCLUDED.version,build_revision=EXCLUDED.build_revision,target_version=EXCLUDED.target_version,reason=EXCLUDED.reason,error_code=EXCLUDED.error_code,consecutive_failed_starts=EXCLUDED.consecutive_failed_starts,last_transition_at=EXCLUDED.last_transition_at`, args...)
	return s.mapError(ctx, err)
}

func lifecycleTransition(from, to runnerauth.LifecycleState) bool {
	if from == to {
		return true
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
	current, err := s.RunnerLifecycle(ctx, id)
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
		*current = current.Effective(now)
	}
	current.LastTransitionAt = now
	return s.storeRunnerLifecycle(ctx, id, *current, "")
}
