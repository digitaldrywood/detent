package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsEventProjection struct {
	FromState *string                          `json:"from_state"`
	ToState   *string                          `json:"to_state"`
	Decision  *tracker.NativeSchedulerDecision `json:"decision"`
	Run       *struct {
		AttemptID string `json:"attempt_id"`
	} `json:"run"`
}

func previousSchedulingDecision(ctx context.Context, q nativeQueryer, scope nativeScope, item string, decision tracker.NativeSchedulerDecision) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+collaborationPayloadColumn("")+` FROM collaboration_events
WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision'
AND (scheduler_outcome IS NULL OR scheduler_outcome='claimed' OR
 (scheduler_runner_id=? AND ((?='skipped' AND scheduler_outcome='skipped' AND scheduler_reason=?) OR (?<>'skipped' AND scheduler_source=?))))
ORDER BY sequence DESC LIMIT 64`, scope.organization, scope.project, item, decision.RunnerID, decision.Outcome, decision.Reason, decision.Outcome, decision.Source)
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
		if err := decodeRunHistory(ctx, q, raw, &data); err != nil {
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
