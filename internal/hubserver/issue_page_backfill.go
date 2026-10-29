package hubserver

import (
	"context"
	"database/sql"
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
	ctx = s.runDetailContext(ctx)
	remaining := storageConversionRows
	var failures error
	families := []struct{ name, table, key, order, pending string }{
		{"issue", "issues", "id", "organization_id,project_id,id", "actor_principal_id IS NULL"},
		{"issue-filters", "issues", "id", "organization_id,project_id,id", "page_filters_ready IS NULL"},
		{"event", "collaboration_events", "id", "organization_id,project_id,work_item_id,sequence", "page_projection_ready IS NULL"},
		{"board", "native_board_items", "work_item_id", "organization_id,project_id,work_item_id", "actor_principal_id IS NULL"},
		{"board-filters", "native_board_items", "work_item_id", "organization_id,project_id,work_item_id", "page_filters_ready IS NULL"},
		{"version", "change_versions", "id", "id", "external_url IS NULL"},
	}
	start := int((s.pageConversionFamily.Add(1) - 1) % uint64(len(families)))
	families = append(families[start:], families[:start]...)
	for _, family := range families {
		if remaining == 0 {
			break
		}
		cursor := "page-" + family.name
		candidates, err := s.storageProjectionPage(ctx, cursor, "SELECT "+family.key+" FROM "+family.table+" WHERE "+family.pending, family.order, remaining)
		if err != nil {
			return errors.Join(failures, err)
		}
		for _, candidate := range candidates {
			for {
				remaining--
				s.projectionCursors.Store(cursor, candidate.cursor)
				more, err := s.convertIssuePageFact(ctx, family.name, candidate.id)
				if err != nil {
					failures = errors.Join(failures, err)
					if ctx.Err() != nil {
						return failures
					}
					break
				}
				if !more || family.name != "version" || remaining == 0 {
					break
				}
			}
			if remaining == 0 {
				break
			}
		}
	}
	return failures
}

func (s *Service) convertIssuePageFact(ctx context.Context, family, id string) (bool, error) {
	var raw, actorRaw string
	queries := map[string]string{
		"issue":         "SELECT actor_json,'' FROM issues WHERE id=? AND actor_principal_id IS NULL",
		"issue-filters": "SELECT labels_json,assignees_json FROM issues WHERE id=? AND page_filters_ready IS NULL",
		"event":         "SELECT data_json,actor_json FROM collaboration_events WHERE id=? AND page_projection_ready IS NULL",
		"board":         "SELECT record_json,'' FROM native_board_items WHERE work_item_id=? AND actor_principal_id IS NULL",
		"board-filters": "SELECT record_json,'' FROM native_board_items WHERE work_item_id=? AND page_filters_ready IS NULL",
		"version":       "SELECT COALESCE(NULLIF(payload_object_key,''),record_json),'' FROM change_versions WHERE id=? AND external_url IS NULL",
	}
	err := s.database.reader.QueryRowContext(ctx, queries[family], id).Scan(&raw, &actorRaw)
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
		err = decodeStorageJSON(ctx, []byte(raw), &actor)
	case "issue-filters":
		if err = decodeStorageJSON(ctx, []byte(raw), &labels); err == nil {
			err = decodeStorageJSON(ctx, []byte(actorRaw), &assignees)
		}
	case "event":
		if err = decodeStorageJSON(ctx, []byte(actorRaw), &actor); err == nil {
			err = decodePageEvent(raw, &data)
		}
	case "board", "board-filters":
		err = decodeStorageJSON(ctx, []byte(raw), &card)
	case "version":
		err = decodeStorageJSON(ctx, []byte(raw), &version)
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
		err = tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json)=? AND external_url IS NULL FROM change_versions WHERE id=?", raw, id).Scan(&unchanged)
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
	return family == "version" && activity.item != "", tx.Commit()
}
