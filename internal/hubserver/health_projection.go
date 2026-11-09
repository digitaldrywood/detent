package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthProjectionTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func updateHealthEventProjection(ctx context.Context, tx *sql.Tx, id string, actor tracker.Actor, data tracker.CollaborationData) error {
	var landed bool
	var landingAt string
	if data.Run != nil && data.Run.Runtime != nil && data.Run.Runtime.Landing != nil {
		landed = data.Run.Runtime.Landing.Landed
		landingAt = healthProjectionTime(data.Run.Runtime.Landing.ObservedAt)
	}
	decision := tracker.NativeSchedulerDecision{}
	if data.Decision != nil {
		decision = *data.Decision
	}
	detail := data.ReasonDetail
	if data.Run != nil || data.Decision != nil {
		detail = ""
	}
	_, err := tx.ExecContext(ctx, `UPDATE collaboration_events SET health_projected=1,scheduler_present=?,scheduler_at=?,scheduler_revision=?,actor_kind=?,actor_principal_id=?,transition_from=?,transition_to=?,transition_reason=?,transition_detail=?,transition_revision=?,event_landed=?,event_landing_at=?,scheduler_outcome=?,scheduler_reason=?,scheduler_source=?,scheduler_runner_id=? WHERE id=?`, data.Decision != nil, healthProjectionTime(decision.At), decision.WorkItemRevision, actor.Kind, actor.PrincipalID, data.FromState, data.ToState, data.Reason, detail, data.Revision, landed, landingAt, decision.Outcome, decision.Reason, decision.Source, decision.RunnerID, id)
	return err
}
