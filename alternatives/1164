package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readActivityWorkspaces(ctx context.Context, q nativeQueryer, organization string, a operatortool.ActivityAttempt) (_ []string, resultErr error) {
	rows, err := queryChangeLegacyRows(ctx, q, `SELECT id FROM workspace_sessions WHERE organization_id=? AND project_id=? AND subject_work_item_id=? AND attempt_id=? AND state IN ('requested','starting','ready','idle') ORDER BY id`, organization, a.ProjectID, a.WorkItemID, a.AttemptID)
	if err != nil {
		return nil, err
	}
	defer func(rows *changeLegacyRows) { resultErr = errors.Join(resultErr, rows.Close()) }(rows)
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const activityLatestChangeQuery = `SELECT id,activity_created_at FROM change_requests WHERE organization_id=? AND project_id=? AND work_item_id=? AND activity_current_version_id IS NOT NULL ORDER BY activity_created_at DESC,id DESC LIMIT 1`

func readActivityChangeLink(ctx context.Context, q nativeQueryer, organization string, a *operatortool.ActivityAttempt) (resultErr error) {
	if a.ChangeID == "" {
		var latest string
		var at float64
		err := q.QueryRowContext(ctx, activityLatestChangeQuery, organization, a.ProjectID, a.WorkItemID).Scan(&latest, &at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := queryChangeLegacyRows(ctx, q, `SELECT id,record_json FROM change_requests WHERE organization_id=? AND project_id=? AND work_item_id=? AND activity_current_version_id IS NULL ORDER BY id`, organization, a.ProjectID, a.WorkItemID)
		if err != nil {
			return err
		}
		defer func(rows *changeLegacyRows) { resultErr = errors.Join(resultErr, rows.Close()) }(rows)
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			var c tracker.ChangeRequest
			if err := decodeStorageJSON(ctx, []byte(raw), &c); err != nil {
				return err
			}
			day := float64(c.CreatedAt.Round(time.Millisecond).UnixMilli())/86400000 + 2440587.5
			if latest == "" || day > at || day == at && id > latest {
				latest, at = id, day
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		a.ChangeID = latest
	}
	if a.ChangeID == "" || a.PullRequestURL != "" {
		return nil
	}
	var version sql.NullString
	var raw string
	err := q.QueryRowContext(ctx, `SELECT activity_current_version_id,CASE WHEN activity_current_version_id IS NULL THEN record_json ELSE '' END FROM change_requests WHERE id=? AND organization_id=? AND project_id=?`, a.ChangeID, organization, a.ProjectID).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !version.Valid {
		var c tracker.ChangeRequest
		if err := decodeStorageJSON(ctx, []byte(raw), &c); err != nil {
			return err
		}
		version.String = c.CurrentVersion
	}
	var url sql.NullString
	err = q.QueryRowContext(ctx, `SELECT activity_external_url,CASE WHEN activity_external_url IS NULL THEN COALESCE(NULLIF(payload_object_key,''),record_json) ELSE '' END FROM change_versions WHERE id=? AND change_id=?`, version.String, a.ChangeID).Scan(&url, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if url.Valid {
		a.PullRequestURL = url.String
		return nil
	}
	var v tracker.ChangeVersion
	if err := decodeStorageJSON(ctx, []byte(raw), &v); err != nil {
		return err
	}
	if v.External != nil {
		a.PullRequestURL = v.External.URL
	}
	return nil
}
