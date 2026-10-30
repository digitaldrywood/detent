package hubhealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *Store) WriteEvaluation(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, findings []Finding) ([]string, error) {
	fingerprints := make([]string, 0, len(findings))
	transitions := []string{}
	for _, f := range findings {
		fingerprints = append(fingerprints, f.Fingerprint)
		var id string
		err := hubsql.QueryRow(ctx, tx, "SELECT id FROM health_findings WHERE organization_id=? AND fingerprint=? AND resolved_at IS NULL", organization, f.Fingerprint).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			cutoff := now.Add(-ReopenWindow)
			err = hubsql.QueryRow(ctx, tx, `SELECT id FROM health_findings WHERE organization_id=? AND fingerprint=? AND resolved_at>=? AND julianday(resolved_at)>=julianday(?) ORDER BY resolved_at DESC LIMIT 1`, organization, f.Fingerprint, cutoff.UTC().Format("2006-01-02T15:04:05"), formatHubTime(cutoff)).Scan(&id)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		subject, err := json.Marshal(f.Subject)
		if err != nil {
			return nil, err
		}
		projects, err := json.Marshal(f.Projects)
		if err != nil {
			return nil, err
		}
		evidence, err := json.Marshal(f.Evidence)
		if err != nil {
			return nil, err
		}
		if id == "" {
			id = d.hooks.NewID("finding")
			transitions = append(transitions, id)
			openedAt := now
			if !f.ConditionSince.IsZero() && f.ConditionSince.Before(now) {
				openedAt = f.ConditionSince
			}
			_, err = hubsql.Exec(ctx, tx, `INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json,projects_projected) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, id, organization, f.Fingerprint, f.Signal, f.Class, string(subject), string(projects), formatHubTime(openedAt), formatHubTime(now), f.Severity, f.Summary, f.NextAction, string(evidence))
			if err == nil {
				err = d.hooks.SyncProjects(ctx, tx, organization, id, f.Projects)
			}
			if err == nil {
				err = d.hooks.AppendDashboard(ctx, tx, id)
			}
			if err == nil && f.Severity == "attention" {
				err = d.hooks.EnqueueOpened(ctx, tx, organization, id, now)
			}
		} else {
			if err := d.hooks.SyncProjects(ctx, tx, organization, id, f.Projects); err != nil {
				return nil, err
			}
			_, err = hubsql.Exec(ctx, tx, `UPDATE health_findings SET last_seen_at=?,resolved_at=NULL,projects_json=?,severity=?,summary=?,next_action=?,evidence_json=?,projects_projected=1 WHERE id=? AND organization_id=?`, formatHubTime(now), string(projects), f.Severity, f.Summary, f.NextAction, string(evidence), id, organization)
		}
		if err != nil {
			return nil, err
		}
	}
	raw, err := hubsql.List(fingerprints)
	if err != nil {
		return nil, err
	}
	if err := d.hooks.EnqueueResolved(ctx, tx, organization, raw, now); err != nil {
		return nil, err
	}
	rows, err := hubsql.Query(ctx, tx, `UPDATE health_findings SET resolved_at=? WHERE organization_id=? AND resolved_at IS NULL AND fingerprint NOT IN (SELECT value FROM sql_rows(?)) RETURNING id`, formatHubTime(now), organization, raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		transitions = append(transitions, id)
		if len(transitions) > ReadLimit {
			return nil, ErrReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	_, err = hubsql.Exec(ctx, tx, `INSERT INTO health_detector_ticks(organization_id,last_tick_at) VALUES(?,?) ON CONFLICT(organization_id) DO UPDATE SET last_tick_at=excluded.last_tick_at`, organization, formatHubTime(now))
	return transitions, err
}
