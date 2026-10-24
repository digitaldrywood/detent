package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) convertSchedulerDecisions(ctx context.Context) error {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id,data_json FROM collaboration_events WHERE type='scheduler.decision' AND scheduler_outcome IS NULL ORDER BY id LIMIT ?`, storageConversionRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		id   string
		data tracker.CollaborationData
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var raw string
		if err := rows.Scan(&c.id, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &c.data); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, c := range candidates {
		d := tracker.NativeSchedulerDecision{}
		if c.data.Decision != nil {
			d = *c.data.Decision
		}
		if _, err := s.database.db.ExecContext(ctx, `UPDATE collaboration_events SET scheduler_outcome=?,scheduler_reason=?,scheduler_source=?,scheduler_runner_id=? WHERE id=? AND scheduler_outcome IS NULL`, d.Outcome, d.Reason, d.Source, d.RunnerID, c.id); err != nil {
			return err
		}
	}
	return nil
}

func previousSchedulingDecision(ctx context.Context, q nativeQueryer, scope nativeScope, item string, decision tracker.NativeSchedulerDecision) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT data_json FROM collaboration_events
WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision'
AND (scheduler_outcome IS NULL OR scheduler_outcome='claimed' OR
 (scheduler_runner_id=? AND ((?='skipped' AND scheduler_outcome='skipped' AND scheduler_reason=?) OR (?<>'skipped' AND scheduler_source=?))))
ORDER BY sequence DESC`, scope.organization, scope.project, item, decision.RunnerID, decision.Outcome, decision.Reason, decision.Outcome, decision.Source)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return "", err
		}
		d := data.Decision
		if d == nil {
			continue
		}
		if d.Outcome == "claimed" {
			return "", sql.ErrNoRows
		}
		if d.RunnerID != decision.RunnerID {
			continue
		}
		if decision.Outcome == "skipped" {
			if d.Outcome != "skipped" || d.Reason != decision.Reason {
				continue
			}
		} else if d.Source != decision.Source {
			continue
		}
		return raw, nil
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", sql.ErrNoRows
}

func hasNewerRunnerDecision(ctx context.Context, q nativeQueryer, scope nativeScope, item, runner string, at time.Time) (bool, error) {
	var found bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision' AND scheduler_runner_id=? AND recorded_at>? AND scheduler_outcome IS NOT NULL)`, scope.organization, scope.project, item, runner, formatHubTime(at)).Scan(&found); err != nil || found {
		return found, err
	}
	rows, err := q.QueryContext(ctx, `SELECT data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision' AND scheduler_outcome IS NULL AND recorded_at>?`, scope.organization, scope.project, item, formatHubTime(at))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return false, err
		}
		if data.Decision != nil && data.Decision.RunnerID == runner {
			return true, nil
		}
	}
	return false, rows.Err()
}
