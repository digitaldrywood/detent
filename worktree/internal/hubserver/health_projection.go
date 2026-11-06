package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func readHealthFindingLinks(ctx context.Context, q nativeQueryer, scope nativeScope, id string) ([]healthFindingIssue, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.project_id,l.work_item_id,i.actor_principal_id,CASE WHEN i.actor_principal_id IS NULL THEN i.actor_json ELSE '{}' END FROM health_finding_issues l JOIN issues i ON i.native_id=l.work_item_id AND i.project_id=l.project_id WHERE l.finding_id=? AND l.project_id=? AND i.organization_id=? ORDER BY l.work_item_id LIMIT ?`, id, scope.project, scope.organization, healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []healthFindingIssue{}
	count := 0
	for rows.Next() {
		count++
		if count > healthReadLimit {
			return nil, errHealthReadLimit
		}
		var link healthFindingIssue
		var principal sql.NullString
		var raw string
		if err := rows.Scan(&link.ProjectID, &link.WorkItemID, &principal, &raw); err != nil {
			return nil, err
		}
		if !principal.Valid {
			var actor tracker.Actor
			if err := json.Unmarshal([]byte(raw), &actor); err != nil {
				return nil, err
			}
			principal.String = actor.PrincipalID
		}
		if scope.credential.HostedRole == "reporter" && (principal.String == "" || principal.String != scope.actor().PrincipalID) {
			continue
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

type healthCancellation struct {
	actorRaw, dataRaw string
	automatic         bool
}

func readHealthCancellations(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID) (map[string]healthCancellation, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,actor_json,data_json FROM collaboration_events
 WHERE organization_id=? AND type='workflow.transitioned' AND health_projected IS NULL
 ORDER BY id LIMIT ?`, organization, healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make(map[string]healthCancellation)
	for rows.Next() {
		if len(facts) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		var id string
		var fact healthCancellation
		if err := rows.Scan(&id, &fact.actorRaw, &fact.dataRaw); err != nil {
			return nil, err
		}
		var actor tracker.Actor
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(fact.actorRaw), &actor); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fact.dataRaw), &data); err != nil {
			return nil, err
		}
		fact.automatic = actor.Kind == "integration" && actor.PrincipalID == "health_detector" && data.FromState == "Backlog" && data.ToState == "Cancelled"
		facts[id] = fact
	}
	return facts, errors.Join(rows.Err(), rows.Close())
}
