package postgres

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *session) replaceBoardSummary(ctx context.Context, previous, current *viewstore.BoardRecord) error {
	for _, change := range []struct {
		record *viewstore.BoardRecord
		delta  int64
	}{{previous, -1}, {current, 1}} {
		b := change.record
		if b == nil || b.PageIssue == nil {
			continue
		}
		audience := []string{""}
		if b.ActorPrincipalID != "" {
			audience = append(audience, b.ActorPrincipalID)
		}
		states := []string{"", b.Position.State, ""}
		kinds := []string{"total", "lane"}
		if b.Terminal {
			kinds = append(kinds, "closed")
		} else {
			kinds = append(kinds, "open")
		}
		workspace := []bool{false}
		if !slices.Contains(b.Labels, "detent:workspace") {
			workspace = append(workspace, true)
		}
		running := int64(0)
		if b.Running {
			running = change.delta
		}
		archive := []string{"all", strconv.FormatBool(b.Archived)}
		args := []any{s.organization, b.ProjectID, audience, archive, workspace, states, kinds, change.delta, running, b.ProjectedAt}
		statement := `INSERT INTO native_board_page_summaries(organization_id,project_id,audience,archive_scope,exclude_workspace,kind,state,total,running,projected_at)
SELECT $1,$2,audience,archive_scope,exclude_workspace,kind,state,$8,$9,$10 FROM unnest($3::text[]) audience CROSS JOIN unnest($4::text[]) archive_scope CROSS JOIN unnest($5::boolean[]) exclude_workspace CROSS JOIN unnest($6::text[],$7::text[]) scopes(state,kind)
ON CONFLICT(organization_id,project_id,audience,archive_scope,exclude_workspace,kind,state) DO UPDATE SET total=native_board_page_summaries.total+excluded.total,running=native_board_page_summaries.running+excluded.running,projected_at=greatest(native_board_page_summaries.projected_at,excluded.projected_at)`
		if change.delta < 0 {
			legacyShipped, legacyCancelled := 0, 0
			switch b.Position.State {
			case "Done":
				legacyShipped = b.LegacyWindowMask
			case "Cancelled":
				legacyCancelled = b.LegacyWindowMask
			}
			statement = `UPDATE native_board_page_summaries SET total=total+$8,running=running+$9,projected_at=greatest(projected_at,$10),
 shipped_1h=shipped_1h-($11::int&1),shipped_24h=shipped_24h-(($11::int>>1)&1),shipped_7d=shipped_7d-(($11::int>>2)&1),
 cancelled_1h=cancelled_1h-($12::int&1),cancelled_24h=cancelled_24h-(($12::int>>1)&1),cancelled_7d=cancelled_7d-(($12::int>>2)&1)
 WHERE organization_id=$1 AND project_id=$2 AND audience=ANY($3::text[]) AND archive_scope=ANY($4::text[]) AND exclude_workspace=ANY($5::boolean[]) AND (state,kind) IN (SELECT * FROM unnest($6::text[],$7::text[]))`
			args = append(args, legacyShipped, legacyCancelled)
		}
		_, err := s.db.Exec(ctx, statement, args...)
		if err != nil {
			return mapError(ctx, err)
		}
		if change.delta < 0 {
			_, err = s.db.Exec(ctx, `DELETE FROM native_board_page_summaries WHERE organization_id=$1 AND project_id=$2 AND audience=ANY($3::text[]) AND archive_scope=ANY($4::text[]) AND exclude_workspace=ANY($5::boolean[]) AND (state,kind) IN (SELECT * FROM unnest($6::text[],$7::text[])) AND total=0`, args[:7]...)
			if err != nil {
				return mapError(ctx, err)
			}
		}
	}
	return nil
}

func (s *session) boardSummaryQuery(selection viewstore.BoardSelection, includeMetadata bool) (string, []any, error) {
	if !selection.SupportsSummary() {
		return "", nil, corestore.ErrInvalidPage
	}
	archive := "all"
	if selection.Archived != nil {
		archive = strconv.FormatBool(*selection.Archived)
	}
	audience := ""
	if selection.Reporter {
		audience = selection.ReporterPrincipalID
		if audience == "" {
			return "", nil, corestore.ErrNotFound
		}
	}
	args := []any{s.organization, selection.ProjectID, audience, archive, selection.ExcludeWorkspace, selection.WindowAsOf}
	prefixSQL, running := "", "c.running"
	if selection.CardLiveness {
		args = append(args, int64(runnerauth.HeartbeatTimeout/time.Second))
		prefixSQL = boardviewsql.Liveness("$1", "$2::text", "$6", "$7") + `,
 running_counts AS MATERIALIZED (
 SELECT bl.state,count(*) AS running FROM board_liveness bl
 WHERE bl.page_ready AND NOT bl.terminal AND ($4='all' OR bl.archived::text=$4)
 AND (NOT $5::boolean OR NOT bl.workspace)
 AND ($3::text='' OR bl.actor_principal_id=$3 AND bl.current_actor=$3)
 AND ` + boardviewsql.Running("bl.card_attempt_status", "bl.card_execution_id", "bl.card_execution_running", "bl", "$6") + `
 GROUP BY bl.state),`
		running = "CASE WHEN c.kind='lane' THEN coalesce((SELECT running FROM running_counts rc WHERE rc.state=c.state),0) WHEN c.kind='closed' THEN 0 ELSE coalesce((SELECT sum(running) FROM running_counts),0) END"
	}
	query := `WITH ` + prefixSQL + `metadata AS MATERIALIZED (` + boardModelMetadataSQL + `),
counts AS MATERIALIZED (
 SELECT c.kind,c.state,c.total,` + running + ` AS running,c.projected_at
 FROM native_board_page_summaries c
 WHERE c.organization_id=$1 AND c.project_id=$2 AND c.audience=$3 AND c.archive_scope=$4 AND c.exclude_workspace=$5 AND NOT (SELECT pending FROM metadata)
 ORDER BY c.kind,c.state LIMIT 204
), completion AS MATERIALIZED (
 SELECT c.kind,c.state,c.projected_at
 FROM native_board_page_summaries c
 WHERE c.organization_id=$1 AND c.project_id=$2 AND c.audience=$3 AND c.archive_scope='all' AND c.exclude_workspace=$5 AND c.kind IN ('total','lane') AND NOT (SELECT pending FROM metadata)
 ORDER BY c.kind,c.state LIMIT 204
),
windows AS MATERIALIZED (` + boardviewsql.CompletionCountsSQL("(SELECT $2::text FROM metadata WHERE NOT pending)", "$3", "$5", "$6") + `), summary(kind,state,total,running,shipped_1h,shipped_24h,shipped_7d,cancelled_1h,cancelled_24h,cancelled_7d,projected_at,sequence) AS (
 SELECT coalesce(c.kind,a.kind,'total'),coalesce(c.state,a.state,''),coalesce(c.total,0),coalesce(c.running,0),
 coalesce(w.shipped_1h,0),
 coalesce(w.shipped_24h,0),
 coalesce(w.shipped_7d,0),
 coalesce(w.cancelled_1h,0),
 coalesce(w.cancelled_24h,0),
 coalesce(w.cancelled_7d,0),
 greatest(c.projected_at,a.projected_at),(SELECT sequence FROM metadata)
 FROM counts c FULL JOIN completion a ON c.kind=a.kind AND c.state=a.state
 LEFT JOIN windows w ON w.kind=CASE WHEN c.kind='closed' THEN 'total' ELSE coalesce(c.kind,a.kind) END AND w.state=coalesce(c.state,a.state)
 AND w.archive_scope=CASE WHEN c.kind='closed' THEN $4 ELSE 'all' END
 ORDER BY 1,2 LIMIT 204)
`
	if includeMetadata {
		query += `SELECT m.sequence,m.pending,m.oldest,coalesce(c.kind,''),coalesce(c.state,''),coalesce(c.total,0),coalesce(c.running,0),
 coalesce(c.shipped_1h,0),coalesce(c.shipped_24h,0),coalesce(c.shipped_7d,0),coalesce(c.cancelled_1h,0),coalesce(c.cancelled_24h,0),coalesce(c.cancelled_7d,0),
 coalesce(c.projected_at,'0001-01-01'::timestamptz),coalesce(c.sequence,0) FROM metadata m LEFT JOIN summary c ON true ORDER BY c.kind,c.state`
	} else {
		query += `SELECT * FROM summary
UNION ALL SELECT '','',0,0,0,0,0,0,0,0,'0001-01-01'::timestamptz,-1 WHERE (SELECT pending FROM metadata)`
	}
	return query, args, nil
}

func (s *session) BoardSummaryModel(ctx context.Context, selection viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error) {
	query, args, err := s.boardSummaryQuery(selection, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardSummaryRecord{}
	for rows.Next() {
		var row viewstore.BoardSummaryRecord
		if err := rows.Scan(boardSummaryTargets(&row)...); err != nil {
			return nil, mapError(ctx, err)
		}
		if row.Sequence == -1 {
			return nil, searchstore.ErrIndexPending
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardStatusModel(ctx context.Context, selection viewstore.BoardSelection) ([]viewstore.BoardStatusRecord, error) {
	if len(selection.WorkItemIDs) == 0 || len(selection.WorkItemIDs) > 2000 {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(selection.WorkItemIDs))
	for index, id := range selection.WorkItemIDs {
		ids[index] = string(id)
	}
	rows, err := s.db.Query(ctx, "SELECT b.work_item_id,b.page_projected_at,coalesce(seq.sequence,0),b.card_change_principal_id,"+boardModelDisplayColumns+" FROM native_board_items b LEFT JOIN native_board_sequences seq ON seq.organization_id=b.organization_id AND seq.project_id=b.project_id WHERE b.organization_id=$1 AND b.project_id=$2 AND b.work_item_id=ANY($3::text[]) AND b.page_ready AND (NOT $4::boolean OR (b.actor_principal_id=$5 AND EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND i.actor_principal_id=$5))) ORDER BY b.work_item_id", s.organization, selection.ProjectID, ids, selection.Reporter, selection.ReporterPrincipalID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardStatusRecord{}
	for rows.Next() {
		var row viewstore.BoardStatusRecord
		var display viewstore.BoardDisplay
		var principal *string
		targets, finish := boardviewsql.DisplayTargets(&display)
		targets = append([]any{&row.WorkItemID, &row.ProjectedAt, &row.Sequence, &principal}, targets...)
		if err := rows.Scan(targets...); err != nil {
			return nil, mapError(ctx, err)
		}
		finish()
		row.StatusLine, row.Attempt, row.Execution, row.Change = display.StatusLine, display.Attempt, display.Execution, display.Change
		if selection.Reporter && (principal == nil || *principal != selection.ReporterPrincipalID) {
			row.Change = nil
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func boardSummaryTargets(row *viewstore.BoardSummaryRecord) []any {
	return []any{&row.Kind, &row.State, &row.Total, &row.Running, &row.Shipped1h, &row.Shipped24h, &row.Shipped7d, &row.Cancelled1h, &row.Cancelled24h, &row.Cancelled7d, &row.ProjectedAt, &row.Sequence}
}
