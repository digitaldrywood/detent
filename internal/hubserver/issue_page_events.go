package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeEditedOwnership(ctx context.Context, q nativeQueryer, item tracker.NativeWorkItemID) (bool, bool, error) {
	var title, body bool
	if err := q.QueryRowContext(ctx, `SELECT title_supplied OR EXISTS(SELECT 1 FROM collaboration_events WHERE work_item_id=s.work_item_id AND type='issue.edited' AND edited_title=1),body_supplied OR EXISTS(SELECT 1 FROM collaboration_events WHERE work_item_id=s.work_item_id AND type='issue.edited' AND edited_body=1) FROM linked_issue_sources s WHERE work_item_id=?`, item).Scan(&title, &body); err != nil {
		return false, false, err
	}
	if title && body {
		return title, body, nil
	}
	rows, err := q.QueryContext(ctx, "SELECT data_json FROM collaboration_events WHERE work_item_id=? AND type='issue.edited' AND page_projection_ready IS NULL ORDER BY sequence LIMIT ?", item, storageConversionRows+1)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	count, bytes := 0, 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, false, err
		}
		count++
		bytes += len(raw)
		if count > storageConversionRows || bytes > storageConversionBytes {
			return false, false, operatortool.ErrReadUnavailable
		}
		var data tracker.CollaborationData
		if err := decodePageEvent(raw, &data); err != nil {
			return false, false, err
		}
		for _, field := range data.Fields {
			title = title || field == "title"
			body = body || field == "body"
		}
	}
	return title, body, rows.Err()
}

func readIssueEmailParticipants(ctx context.Context, q nativeQueryer, event issueEmailEvent, item, organization, after string, limit int) (map[string]bool, string, error) {
	var project tracker.ProjectID
	var kind, principal sql.NullString
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT project_id,actor_kind,actor_principal_id,CASE WHEN actor_principal_id IS NULL THEN actor_json ELSE '' END FROM issues WHERE organization_id=? AND native_id=?", organization, item).Scan(&project, &kind, &principal, &raw); err != nil {
		return nil, "", err
	}
	if !principal.Valid {
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			return nil, "", err
		}
		kind.String, principal.String = actor.Kind, actor.PrincipalID
	}
	participants := map[string]bool{}
	if kind.String == "human" && principal.String > after {
		participants[principal.String] = true
	}
	rows, err := q.QueryContext(ctx, `SELECT actor_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='comment.created' AND page_projection_ready IS NULL AND julianday(recorded_at)<=julianday(?) AND (?<>? OR sequence<=?) LIMIT ?`, organization, project, item, formatHubTime(event.at), item, event.item, event.sequence, storageConversionRows+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	count, bytes := 0, 0
	for rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, "", err
		}
		count++
		bytes += len(raw)
		if count > storageConversionRows || bytes > storageConversionBytes {
			rows.Close()
			return nil, "", operatortool.ErrReadUnavailable
		}
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(raw), &actor); err != nil {
			rows.Close()
			return nil, "", err
		}
		if actor.Kind == "human" && actor.PrincipalID > after {
			if _, exists := participants[actor.PrincipalID]; !exists {
				participants[actor.PrincipalID] = false
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, "", err
	}
	rows, err = q.QueryContext(ctx, `SELECT actor_principal_id FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='comment.created' AND actor_kind='human' AND actor_principal_id>? AND page_projection_ready=1 AND julianday(recorded_at)<=julianday(?) AND (?<>? OR sequence<=?) GROUP BY actor_principal_id ORDER BY actor_principal_id LIMIT ?`, organization, project, item, after, formatHubTime(event.at), item, event.item, event.sequence, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		if _, exists := participants[id]; !exists {
			participants[id] = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	principals := make([]string, 0, len(participants))
	for id := range participants {
		principals = append(principals, id)
	}
	slices.Sort(principals)
	next := ""
	if len(principals) > limit {
		next = principals[limit-1]
		for _, id := range principals[limit:] {
			delete(participants, id)
		}
	}
	return participants, next, nil
}

func appendIssueEmailNotice(ctx context.Context, tx *sql.Tx, item, user, due string, notice issueEmailNotice) error {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT notices_json FROM issue_notification_batches WHERE work_item_id=? AND user_id=?", item, user).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var notices []issueEmailNotice
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &notices); err != nil {
			return err
		}
	}
	notices = append(notices, notice)
	encoded, err := json.Marshal(notices)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO issue_notification_batches(work_item_id,user_id,due_at,notices_json) SELECT work_item_id,user_id,?,? FROM issue_subscriptions WHERE work_item_id=? AND user_id=? AND unsubscribed=0 ON CONFLICT(work_item_id,user_id) DO UPDATE SET notices_json=excluded.notices_json`, due, string(encoded), item, user)
	return err
}
