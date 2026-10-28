package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const issuePagePullRequestActivity = `CASE WHEN rtrim(pr.synchronized_at,'Z')>rtrim(pr.updated_at,'Z') THEN pr.synchronized_at ELSE pr.updated_at END`

const issuePageVersionActivityQuery = `SELECT COALESCE((SELECT ` + issuePagePullRequestActivity + `
 FROM change_versions v JOIN change_requests c ON c.id=v.change_id
 JOIN projects p ON p.organization_id=c.organization_id AND p.id=c.project_id
 JOIN pull_requests pr ON pr.repository_id=p.repository_id AND pr.url=?
 WHERE v.id=? ORDER BY rtrim(` + issuePagePullRequestActivity + `,'Z') DESC,pr.id DESC LIMIT 1),'')`

const issuePageVersionActivityItemQuery = `SELECT i.native_id FROM change_versions v
 JOIN change_issue_links l ON l.change_id=v.change_id
 JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id
 WHERE v.id=? AND rtrim(COALESCE(i.last_activity_at,''),'Z')<rtrim(?,'Z') ORDER BY l.work_item_id LIMIT 1`

type issuePageVersionActivity struct {
	at, item string
}

func readIssuePageVersionActivity(ctx context.Context, q nativeQueryer, id, url string) (issuePageVersionActivity, error) {
	var activity issuePageVersionActivity
	if url == "" {
		return activity, nil
	}
	if err := q.QueryRowContext(ctx, issuePageVersionActivityQuery, url, id).Scan(&activity.at); err != nil {
		return activity, err
	}
	if activity.at == "" {
		return activity, nil
	}
	err := q.QueryRowContext(ctx, issuePageVersionActivityItemQuery, id, activity.at).Scan(&activity.item)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return activity, err
}

func changeVersionExternalURL(version tracker.ChangeVersion) string {
	if version.External != nil {
		return version.External.URL
	}
	return ""
}

func (s *Service) convertIssuePageFacts(ctx context.Context) error {
	for range storageConversionRows {
		more, err := s.convertIssuePageFact(ctx)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (s *Service) convertIssuePageFact(ctx context.Context) (bool, error) {
	var family, id, raw, actorRaw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT * FROM (
 SELECT 'issue',CAST(id AS TEXT),actor_json,'' FROM (SELECT id,actor_json FROM issues WHERE actor_principal_id IS NULL ORDER BY organization_id,project_id,id LIMIT 1)
 UNION ALL SELECT 'issue-filters',CAST(id AS TEXT),labels_json,assignees_json FROM (SELECT id,labels_json,assignees_json FROM issues WHERE page_filters_ready IS NULL ORDER BY organization_id,project_id,id LIMIT 1)
 UNION ALL SELECT 'event',id,data_json,actor_json FROM (SELECT id,data_json,actor_json FROM collaboration_events WHERE page_projection_ready IS NULL ORDER BY organization_id,project_id,work_item_id,sequence LIMIT 1)
 UNION ALL SELECT 'board',work_item_id,record_json,'' FROM (SELECT work_item_id,record_json FROM native_board_items WHERE actor_principal_id IS NULL ORDER BY organization_id,project_id,work_item_id LIMIT 1)
 UNION ALL SELECT 'board-filters',work_item_id,record_json,'' FROM (SELECT work_item_id,record_json FROM native_board_items WHERE page_filters_ready IS NULL ORDER BY organization_id,project_id,work_item_id LIMIT 1)
 UNION ALL SELECT 'version',id,record_json,'' FROM (SELECT id,record_json FROM change_versions WHERE external_url IS NULL ORDER BY id LIMIT 1)
 ) LIMIT 1`).Scan(&family, &id, &raw, &actorRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var actor tracker.Actor
	var labels, assignees []string
	var data tracker.CollaborationData
	var card tracker.NativeBoardCard
	var version tracker.ChangeVersion
	switch family {
	case "issue":
		err = json.Unmarshal([]byte(raw), &actor)
	case "issue-filters":
		if err = json.Unmarshal([]byte(raw), &labels); err == nil {
			err = json.Unmarshal([]byte(actorRaw), &assignees)
		}
	case "event":
		if err = json.Unmarshal([]byte(actorRaw), &actor); err == nil {
			err = decodePageEvent(raw, &data)
		}
	case "board", "board-filters":
		err = json.Unmarshal([]byte(raw), &card)
	case "version":
		err = json.Unmarshal([]byte(raw), &version)
	}
	if err != nil {
		return false, err
	}
	var activity issuePageVersionActivity
	if family == "version" {
		activity, err = readIssuePageVersionActivity(ctx, s.database.reader, id, changeVersionExternalURL(version))
		if err != nil {
			return false, err
		}
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var unchanged bool
	switch family {
	case "issue":
		err = tx.QueryRowContext(ctx, "SELECT actor_json=? AND actor_principal_id IS NULL FROM issues WHERE id=?", raw, id).Scan(&unchanged)
	case "issue-filters":
		err = tx.QueryRowContext(ctx, "SELECT labels_json=? AND assignees_json=? AND page_filters_ready IS NULL FROM issues WHERE id=?", raw, actorRaw, id).Scan(&unchanged)
	case "event":
		err = tx.QueryRowContext(ctx, "SELECT data_json=? AND actor_json=? AND page_projection_ready IS NULL FROM collaboration_events WHERE id=?", raw, actorRaw, id).Scan(&unchanged)
	case "board":
		err = tx.QueryRowContext(ctx, "SELECT record_json=? AND actor_principal_id IS NULL FROM native_board_items WHERE work_item_id=?", raw, id).Scan(&unchanged)
	case "board-filters":
		err = tx.QueryRowContext(ctx, "SELECT record_json=? AND page_filters_ready IS NULL FROM native_board_items WHERE work_item_id=?", raw, id).Scan(&unchanged)
	case "version":
		err = tx.QueryRowContext(ctx, "SELECT record_json=? AND external_url IS NULL FROM change_versions WHERE id=?", raw, id).Scan(&unchanged)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !unchanged {
		return true, nil
	}
	switch family {
	case "issue":
		var numeric int64
		numeric, err = strconv.ParseInt(id, 10, 64)
		if err == nil {
			err = updateIssueActor(ctx, tx, numeric, actor)
		}
	case "event":
		err = updateEventPageFacts(ctx, tx, id, actor, data)
	case "issue-filters":
		var numeric int64
		numeric, err = strconv.ParseInt(id, 10, 64)
		if err == nil {
			err = replaceIssuePageFilterValues(ctx, tx, numeric, labels, assignees)
		}
	case "board":
		err = updateBoardFilterFacts(ctx, tx, card)
	case "board-filters":
		err = replaceBoardPageFilterValues(ctx, tx, card)
	case "version":
		var current issuePageVersionActivity
		current, err = readIssuePageVersionActivity(ctx, tx, id, changeVersionExternalURL(version))
		if err != nil {
			return false, err
		}
		if current != activity {
			return true, nil
		}
		if activity.item != "" {
			_, err = tx.ExecContext(ctx, "UPDATE issues SET last_activity_at=? WHERE native_id=?", activity.at, activity.item)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE change_versions SET external_url=? WHERE id=?", changeVersionExternalURL(version), id)
		}
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}
