package hubhealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadFindingLinks(ctx context.Context, q hubsql.Queryer, scope Scope, id string) ([]FindingIssue, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.project_id,l.work_item_id,i.actor_principal_id,CASE WHEN i.actor_principal_id IS NULL THEN i.actor_json ELSE '{}' END FROM health_finding_issues l JOIN issues i ON i.native_id=l.work_item_id AND i.project_id=l.project_id WHERE l.finding_id=? AND l.project_id=? AND i.organization_id=? ORDER BY l.work_item_id LIMIT ?`, id, scope.Project, scope.Organization, ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []FindingIssue{}
	count := 0
	for rows.Next() {
		count++
		if count > ReadLimit {
			return nil, ErrReadLimit
		}
		var link FindingIssue
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
		if scope.Reporter && (principal.String == "" || principal.String != scope.Principal) {
			continue
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func healthProjectionTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func UpdateEventProjection(ctx context.Context, tx *sql.Tx, id string, actor tracker.Actor, data tracker.CollaborationData) error {
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

type ProjectionFamily string

func ProjectionQueries(family ProjectionFamily) (string, string, error) {
	switch family {
	case "collaboration_events":
		return `SELECT rowid,id,data_json,actor_json FROM collaboration_events WHERE health_projected IS NULL ORDER BY rowid LIMIT ?`, `UPDATE collaboration_events SET health_projected=health_projected WHERE rowid=? AND health_projected IS NULL AND data_json=?`, nil
	case "issues":
		return `SELECT rowid,id,actor_json,'' FROM issues WHERE actor_kind IS NULL ORDER BY rowid LIMIT ?`, `UPDATE issues SET actor_kind=actor_kind WHERE rowid=? AND actor_kind IS NULL AND actor_json=?`, nil
	case "native_comments":
		return `SELECT rowid,id,actor_json,'' FROM native_comments WHERE actor_kind IS NULL ORDER BY rowid LIMIT ?`, `UPDATE native_comments SET actor_kind=actor_kind WHERE rowid=? AND actor_kind IS NULL AND actor_json=?`, nil
	case "scheduled_validation_reports":
		return `SELECT rowid,CAST(rowid AS TEXT),record_json,'' FROM scheduled_validation_reports WHERE conclusion IS NULL ORDER BY rowid LIMIT ?`, `UPDATE scheduled_validation_reports SET conclusion=conclusion WHERE rowid=? AND conclusion IS NULL AND record_json=?`, nil
	case "health_findings":
		return `SELECT rowid,id,projects_json,organization_id FROM health_findings WHERE projects_projected IS NULL ORDER BY rowid LIMIT ?`, `UPDATE health_findings SET projects_projected=projects_projected WHERE rowid=? AND projects_projected IS NULL AND projects_json=?`, nil
	}
	return "", "", fmt.Errorf("unknown health projection %s", family)
}

func (d *Store) DecodeProjection(f ProjectionFamily, id, raw, actorRaw string) (func(context.Context, *sql.Tx, int64) error, error) {
	switch f {
	case "collaboration_events":
		var data tracker.CollaborationData
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(actorRaw), &actor); err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx *sql.Tx, row int64) error {
			return UpdateEventProjection(ctx, tx, id, actor, data)
		}, nil
	case "issues", "native_comments":
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx *sql.Tx, row int64) error {
			query := `UPDATE native_comments SET actor_kind=? WHERE rowid=?`
			args := []any{actor.Kind, row}
			if f == "issues" {
				query = `UPDATE issues SET actor_kind=?,actor_principal_id=? WHERE rowid=?`
				args = []any{actor.Kind, actor.PrincipalID, row}
			}
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		}, nil
	case "scheduled_validation_reports":
		var report gate.ScheduledValidationReport
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx *sql.Tx, row int64) error {
			_, err := tx.ExecContext(ctx, `UPDATE scheduled_validation_reports SET conclusion=? WHERE rowid=?`, report.Conclusion, row)
			return err
		}, nil
	case "health_findings":
		var projects []string
		if err := json.Unmarshal([]byte(raw), &projects); err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx *sql.Tx, row int64) error {
			if err := d.hooks.SyncProjects(ctx, tx, tracker.OrganizationID(actorRaw), id, projects); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE health_findings SET projects_projected=1 WHERE id=?`, id)
			return err
		}, nil
	}
	return nil, fmt.Errorf("unknown health projection %s", f)
}

func (s *Store) ApplyProjection(ctx context.Context, f ProjectionFamily, row int64, raw string, apply func(context.Context, *sql.Tx, int64) error) error {
	_, fence, err := ProjectionQueries(f)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, fence, row, raw)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if err := apply(ctx, tx, row); err != nil {
		return err
	}
	return tx.Commit()
}

type Cancellation struct {
	ActorRaw, DataRaw string
	Automatic         bool
}

func ReadCancellations(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID) (map[string]Cancellation, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,actor_json,data_json FROM collaboration_events
 WHERE organization_id=? AND type='workflow.transitioned' AND health_projected IS NULL
 ORDER BY id LIMIT ?`, organization, ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make(map[string]Cancellation)
	for rows.Next() {
		if len(facts) == ReadLimit {
			return nil, ErrReadLimit
		}
		var id string
		var fact Cancellation
		if err := rows.Scan(&id, &fact.ActorRaw, &fact.DataRaw); err != nil {
			return nil, err
		}
		var actor tracker.Actor
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(fact.ActorRaw), &actor); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fact.DataRaw), &data); err != nil {
			return nil, err
		}
		fact.Automatic = actor.Kind == "integration" && actor.PrincipalID == "health_detector" && data.FromState == "Backlog" && data.ToState == "Cancelled"
		facts[id] = fact
	}
	return facts, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) ConvertProjections(ctx context.Context, remaining, start int) error {
	families := []ProjectionFamily{"collaboration_events", "issues", "native_comments", "scheduled_validation_reports", "health_findings"}
	families = append(families[start:], families[:start]...)
	for _, family := range families {
		if remaining == 0 {
			break
		}
		n, err := s.ConvertProjectionFamily(ctx, family, remaining)
		if err != nil {
			return fmt.Errorf("convert health %s: %w", family, err)
		}
		remaining -= n
	}
	return nil
}

func (s *Store) ConvertProjectionFamily(ctx context.Context, family ProjectionFamily, limit int) (int, error) {
	query, _, err := ProjectionQueries(family)
	if err != nil {
		return 0, err
	}
	base, _, _ := strings.Cut(query, " ORDER BY")
	keys := "rowid"
	table := string(family)
	_, pending, _ := strings.Cut(base, " WHERE ")
	selected, err := s.hooks.ProjectionPage(ctx, "health-"+table, "SELECT rowid FROM "+table+" INDEXED BY "+table+"_health_conversion_idx WHERE "+pending, keys, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		row            int64
		id, raw, actor string
	}
	var candidates []candidate
	for _, position := range selected {
		var c candidate
		if err := s.reader.QueryRowContext(ctx, base+" AND rowid=?", position).Scan(&c.row, &c.id, &c.raw, &c.actor); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return 0, err
		}
		candidates = append(candidates, c)
	}
	for _, c := range candidates {
		if err := s.hooks.ChargeConversion(ctx, len(c.raw)+len(c.actor)); err != nil {
			return 0, err
		}
		apply, err := s.DecodeProjection(family, c.id, c.raw, c.actor)
		if err != nil {
			return 0, err
		}
		if err := s.ApplyProjection(ctx, family, c.row, c.raw, apply); err != nil {
			return 0, err
		}
	}
	return len(candidates), nil
}
