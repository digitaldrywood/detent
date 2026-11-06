package postgres

import (
	"context"
	"slices"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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
		windows := [6]int64{}
		for index := range 3 {
			if b.WindowMask&(1<<index) == 0 {
				continue
			}
			if b.Position.State == "Done" {
				windows[index] = change.delta
			}
			if b.Position.State == "Cancelled" {
				windows[index+3] = change.delta
			}
		}
		running := int64(0)
		if b.Running {
			running = change.delta
		}
		statement := `INSERT INTO native_board_page_summaries(organization_id,project_id,audience,archive_scope,exclude_workspace,kind,state,total,running,shipped_1h,shipped_24h,shipped_7d,cancelled_1h,cancelled_24h,cancelled_7d,projected_at)
SELECT $1,$2,audience,archive_scope,exclude_workspace,kind,state,$7,$8,$9,$10,$11,$12,$13,$14,$15 FROM unnest($3::text[]) audience CROSS JOIN unnest($4::text[]) archive_scope CROSS JOIN unnest($5::boolean[]) exclude_workspace CROSS JOIN unnest($6::text[],$16::text[]) scopes(state,kind)
ON CONFLICT(organization_id,project_id,audience,archive_scope,exclude_workspace,kind,state) DO UPDATE SET total=native_board_page_summaries.total+excluded.total,running=native_board_page_summaries.running+excluded.running,shipped_1h=native_board_page_summaries.shipped_1h+excluded.shipped_1h,shipped_24h=native_board_page_summaries.shipped_24h+excluded.shipped_24h,shipped_7d=native_board_page_summaries.shipped_7d+excluded.shipped_7d,cancelled_1h=native_board_page_summaries.cancelled_1h+excluded.cancelled_1h,cancelled_24h=native_board_page_summaries.cancelled_24h+excluded.cancelled_24h,cancelled_7d=native_board_page_summaries.cancelled_7d+excluded.cancelled_7d,projected_at=greatest(native_board_page_summaries.projected_at,excluded.projected_at)`
		if change.delta < 0 {
			statement = `UPDATE native_board_page_summaries SET total=total+$7,running=running+$8,shipped_1h=shipped_1h+$9,shipped_24h=shipped_24h+$10,shipped_7d=shipped_7d+$11,cancelled_1h=cancelled_1h+$12,cancelled_24h=cancelled_24h+$13,cancelled_7d=cancelled_7d+$14,projected_at=greatest(projected_at,$15) WHERE organization_id=$1 AND project_id=$2 AND audience=ANY($3::text[]) AND archive_scope=ANY($4::text[]) AND exclude_workspace=ANY($5::boolean[]) AND (state,kind) IN (SELECT * FROM unnest($6::text[],$16::text[]))`
		}
		_, err := s.db.Exec(ctx, statement, s.organization, b.ProjectID, audience, []string{"all", strconv.FormatBool(b.Archived)}, workspace, states, change.delta, running, windows[0], windows[1], windows[2], windows[3], windows[4], windows[5], b.ProjectedAt, kinds)
		if err != nil {
			return mapError(ctx, err)
		}
		if change.delta < 0 {
			_, err = s.db.Exec(ctx, `DELETE FROM native_board_page_summaries WHERE organization_id=$1 AND project_id=$2 AND audience=ANY($3::text[]) AND archive_scope=ANY($4::text[]) AND exclude_workspace=ANY($5::boolean[]) AND (state,kind) IN (SELECT * FROM unnest($6::text[],$7::text[])) AND total=0`, s.organization, b.ProjectID, audience, []string{"all", strconv.FormatBool(b.Archived)}, workspace, states, kinds)
			if err != nil {
				return mapError(ctx, err)
			}
		}
	}
	return nil
}

func (s *session) BoardSummaryModel(ctx context.Context, selection viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error) {
	if selection.SearchText != "" || selection.Fingerprint != "" || selection.ActorPrincipalID != "" || len(selection.Types)+len(selection.Verification)+len(selection.Labels)+len(selection.Assignees)+len(selection.Priorities) > 0 || selection.NativeCreatedAfter != nil || selection.NativeCreatedBefore != nil || selection.NativeUpdatedAfter != nil || selection.CompletedFrom != nil || selection.CompletedTo != nil {
		return nil, corestore.ErrInvalidPage
	}
	archive := "all"
	if selection.Archived != nil {
		archive = strconv.FormatBool(*selection.Archived)
	}
	audience := ""
	if selection.Reporter {
		audience = selection.ReporterPrincipalID
		if audience == "" {
			return nil, corestore.ErrNotFound
		}
	}
	rows, err := s.db.Query(ctx, `WITH readiness AS MATERIALIZED (SELECT EXISTS(SELECT 1 FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2) OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT page_ready) OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT list_ready) AS pending)
(SELECT c.kind,c.state,c.total,c.running,c.shipped_1h,c.shipped_24h,c.shipped_7d,c.cancelled_1h,c.cancelled_24h,c.cancelled_7d,c.projected_at,coalesce(s.sequence,0) FROM native_board_page_summaries c LEFT JOIN native_board_sequences s ON s.organization_id=c.organization_id AND s.project_id=c.project_id WHERE c.organization_id=$1 AND c.project_id=$2 AND c.audience=$3 AND c.archive_scope=$4 AND c.exclude_workspace=$5 AND NOT (SELECT pending FROM readiness) ORDER BY c.kind,c.state LIMIT 204)
UNION ALL SELECT '','',0,0,0,0,0,0,0,0,'0001-01-01'::timestamptz,-1 WHERE (SELECT pending FROM readiness)`, s.organization, selection.ProjectID, audience, archive, selection.ExcludeWorkspace)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardSummaryRecord{}
	for rows.Next() {
		var row viewstore.BoardSummaryRecord
		if err := rows.Scan(&row.Kind, &row.State, &row.Total, &row.Running, &row.Shipped1h, &row.Shipped24h, &row.Shipped7d, &row.Cancelled1h, &row.Cancelled24h, &row.Cancelled7d, &row.ProjectedAt, &row.Sequence); err != nil {
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
		targets, finish := boardDisplayTargets(&display)
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
