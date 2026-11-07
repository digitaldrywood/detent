package postgres

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

const boardModelDisplayColumns = `b.card_display_ready,b.card_display_exact,b.card_provenance_provider IS NOT NULL,coalesce(b.card_provenance_provider,''),coalesce(b.card_provenance_external_id,''),coalesce(b.card_provenance_author_id,''),coalesce(b.card_provenance_author_display_name,''),coalesce(b.card_provenance_created_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_updated_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_observed_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_principal_id,''),coalesce(b.card_title,''),coalesce(b.card_actor_kind,''),coalesce(b.card_actor_via_key,''),coalesce(b.card_actor_display_name,''),coalesce(b.card_actor_agent,''),coalesce(b.card_reason,''),coalesce(b.card_status_line,''),coalesce(b.card_attempt_id,''),coalesce(b.card_attempt_count,0),coalesce(b.card_attempt_status,''),coalesce(b.card_attempt_runner_id,''),coalesce(b.card_attempt_machine_id,''),coalesce(b.card_attempt_started_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_attempt_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_execution_id,''),coalesce(b.card_execution_running,false),coalesce(b.card_execution_stage,''),coalesce(b.card_execution_runner_id,''),b.card_execution_started_at,coalesce(b.card_execution_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_id,''),coalesce(b.card_change_title,''),coalesce(b.card_change_status,''),coalesce(b.card_change_external_provider,''),coalesce(b.card_change_external_id,''),coalesce(b.card_change_external_url,''),coalesce(b.card_attempt_identity_role,''),coalesce(b.card_attempt_identity_backend,''),coalesce(b.card_attempt_identity_model,''),coalesce(b.card_attempt_identity_effort,''),coalesce(b.card_execution_identity_role,''),coalesce(b.card_execution_identity_backend,''),coalesce(b.card_execution_identity_model,''),coalesce(b.card_execution_identity_effort,''),coalesce(b.issue_number,0)`

func (s *session) boardModelQuery(f viewstore.BoardSelection, authority accessstore.Authority) (string, []any, error) {
	if f.Limit < 1 || f.Limit > 2001 {
		return "", nil, corestore.ErrInvalidPage
	}
	if f.SearchText != "" || f.Fingerprint != "" {
		return "", nil, searchstore.ErrIndexPending
	}
	base := f
	base.Reporter = false
	base.NativeCreatedAfter, base.NativeCreatedBefore, base.NativeUpdatedAfter = nil, nil, nil
	where, args, err := boardWhere(s.organization, base)
	if err != nil {
		return "", nil, err
	}
	where += " AND b.page_ready"
	if f.Compact {
		where += " AND b.list_ready"
	}
	add := func(expression string, value any) {
		args = append(args, value)
		where += fmt.Sprintf(" AND "+expression, len(args))
	}
	if f.Reporter {
		if f.ReporterPrincipalID == "" {
			return "", nil, corestore.ErrNotFound
		}
		add("b.actor_principal_id=$%d", f.ReporterPrincipalID)
	}
	for _, filter := range []struct {
		column, operator string
		at               *time.Time
	}{{"b.created_at", ">=", f.NativeCreatedAfter}, {"b.created_at", "<", f.NativeCreatedBefore}, {"b.updated_at", ">=", f.NativeUpdatedAfter}} {
		if filter.at != nil {
			add(filter.column+filter.operator+"$%d", *filter.at)
		}
	}
	order := "b.lane_rank,b.state,b.priority_rank,b.activity_missing,coalesce(b.activity_at,'-infinity'::timestamptz) DESC,b.identifier"
	prefix := "b.lane_rank,b.state,b.priority_rank,b.activity_missing"
	if len(f.States) == 1 {
		order = "b.priority_rank,b.activity_missing,coalesce(b.activity_at,'-infinity'::timestamptz) DESC,b.identifier"
		prefix = "b.priority_rank,b.activity_missing"
	}
	if f.ClosedOrder {
		order = "coalesce(b.closed_at,'-infinity'::timestamptz) DESC,b.identifier"
	}
	if f.WorkOrder {
		order = "b.running DESC,b.dispatchable DESC,b.issue_number DESC,b.identifier"
	}
	if p := f.After; p != nil {
		if f.ClosedOrder {
			args = append(args, p.Activity, p.Identifier)
			at, id := len(args)-1, len(args)
			where += fmt.Sprintf(" AND (coalesce(b.closed_at,'-infinity'::timestamptz)<coalesce($%d::timestamptz,'-infinity'::timestamptz) OR (coalesce(b.closed_at,'-infinity'::timestamptz)=coalesce($%d::timestamptz,'-infinity'::timestamptz) AND b.identifier>$%d))", at, at, id)
		} else {
			values := []any{p.Lane, p.State, p.Priority, p.ActivityMissing}
			if len(f.States) == 1 {
				values = values[2:]
			}
			holders := make([]string, len(values))
			for i, value := range values {
				args = append(args, value)
				holders[i] = fmt.Sprintf("$%d", len(args))
			}
			tuple := "(" + strings.Join(holders, ",") + ")"
			args = append(args, p.Activity, p.Identifier)
			at, id := len(args)-1, len(args)
			where += fmt.Sprintf(" AND ((%s)>%s OR ((%s)=%s AND (coalesce(b.activity_at,'-infinity'::timestamptz)<coalesce($%d::timestamptz,'-infinity'::timestamptz) OR (coalesce(b.activity_at,'-infinity'::timestamptz)=coalesce($%d::timestamptz,'-infinity'::timestamptz) AND b.identifier>$%d))))", prefix, tuple, prefix, tuple, at, at, id)
		}
	}
	args = append(args, f.Limit)
	limitArg := len(args)
	allowed := "true AS access_allowed"
	columns := boardPageColumns
	if f.Compact {
		columns = boardListColumns
	}
	model := allowed + ",b.source_revision,b.source_event_sequence,b.source_project_revision,b.lane_rank,b.state,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at,b.running,b.dispatchable,b.issue_number,b.page_list_bytes," + boardModelDisplayColumns + "," + columns
	query := "WITH page AS MATERIALIZED (SELECT b.* FROM native_board_items b WHERE " + where + " ORDER BY " + order + fmt.Sprintf(" LIMIT $%d", limitArg) + ") SELECT " + model + " FROM page b ORDER BY " + order

	return query, args, nil
}

var boardModelParameter = regexp.MustCompile(`\$[0-9]+`)

func (s *session) BoardModelPages(ctx context.Context, selection viewstore.BoardSelection, work *viewstore.BoardSelection, authority accessstore.Authority) (result viewstore.BoardModelPage, resultErr error) {
	if selection.SearchText != "" || selection.Fingerprint != "" {
		return s.boardSearchModelPages(ctx, selection, work)
	}
	query, args, err := s.boardModelQuery(selection, authority)
	if err != nil {
		return result, err
	}
	query = "SELECT 1 AS page_part, model.* FROM (" + query + ") model"
	if work != nil {
		workSQL, workArgs, err := s.boardModelQuery(*work, authority)
		if err != nil {
			return result, err
		}
		offset := len(args)
		workSQL = boardModelParameter.ReplaceAllStringFunc(workSQL, func(value string) string {
			number, parseErr := strconv.Atoi(value[1:])
			if parseErr != nil {
				return value
			}
			return fmt.Sprintf("$%d", number+offset)
		})
		args = append(args, workArgs...)
		query = "(" + query + ") UNION ALL (SELECT 2 AS page_part, model.* FROM (" + workSQL + ") model)"
	}
	mainOrder := "CASE WHEN page_part=1 THEN lane_rank END,CASE WHEN page_part=1 THEN state END,CASE WHEN page_part=1 THEN priority_rank END,CASE WHEN page_part=1 THEN activity_missing END,CASE WHEN page_part=1 THEN activity_at END DESC NULLS LAST"
	if selection.ClosedOrder {
		mainOrder = "CASE WHEN page_part=1 THEN closed_at END DESC NULLS LAST"
	}
	query = "SELECT * FROM (" + query + ") ordered ORDER BY page_part," + mainOrder + ",CASE WHEN page_part=2 THEN running END DESC,CASE WHEN page_part=2 THEN dispatchable END DESC,CASE WHEN page_part=2 THEN issue_number END DESC,identifier"
	batch := &pgx.Batch{}
	metaSQL := boardModelMetadataSQL
	if selection.Summary {
		summarySQL, summaryArgs, err := s.boardSummaryQuery(selection)
		if err != nil {
			return result, err
		}
		summarySQL = "SELECT metadata.*,coalesce(c.kind,''),coalesce(c.state,''),coalesce(c.total,0),coalesce(c.running,0),coalesce(c.shipped_1h,0),coalesce(c.shipped_24h,0),coalesce(c.shipped_7d,0),coalesce(c.cancelled_1h,0),coalesce(c.cancelled_24h,0),coalesce(c.cancelled_7d,0),coalesce(c.projected_at,'0001-01-01'::timestamptz),coalesce(c.summary_sequence,0) FROM (" + metaSQL + ") metadata LEFT JOIN (" + summarySQL + ") c(kind,state,total,running,shipped_1h,shipped_24h,shipped_7d,cancelled_1h,cancelled_24h,cancelled_7d,projected_at,summary_sequence) ON true"
		batch.Queue(summarySQL, summaryArgs...).Query(func(rows pgx.Rows) error {
			defer rows.Close()
			for rows.Next() {
				var summary viewstore.BoardSummaryRecord
				targets := append([]any{&result.Sequence, &result.Pending, &result.OldestPendingAt}, boardSummaryTargets(&summary)...)
				if err := rows.Scan(targets...); err != nil {
					return err
				}
				if summary.Sequence == -1 {
					result.Pending = true
				}
				if summary.Kind != "" {
					result.Summaries = append(result.Summaries, summary)
				}
			}
			return rows.Err()
		})
	} else {
		batch.Queue(metaSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error { return row.Scan(&result.Sequence, &result.Pending, &result.OldestPendingAt) })
	}

	batch.Queue(query, args...).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var row viewstore.BoardRecord
			var part, number int
			var allowed bool
			targets := []any{&part, &allowed, &row.SourceRevision, &row.SourceEventSequence, &row.SourceProjectRevision, &row.Position.Lane, &row.Position.State, &row.Position.Priority, &row.Position.ActivityMissing, &row.Position.Activity, &row.Position.Identifier, &row.ClosedAt, &row.Running, &row.Dispatchable, &number, &row.ListBytes}
			display, finishDisplay := boardDisplayTargets(&row.Display)
			page, finishPage := boardPageTargets(&row)
			targets = append(targets, display...)
			targets = append(targets, page...)
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			if !allowed {
				return accessstore.ErrDenied
			}
			finishDisplay()
			finishPage()
			if row.PageIssue == nil {
				return viewstore.ErrInvalidView
			}
			row.OrganizationID, row.ProjectID, row.WorkItemID = row.PageIssue.OrganizationID, row.PageIssue.ProjectID, row.PageIssue.WorkItemID
			if part == 1 {
				result.Records = append(result.Records, row)
			} else {
				result.WorkRecords = append(result.WorkRecords, row)
			}
		}
		return rows.Err()
	})
	resultErr = s.readBoardModelBatch(ctx, batch)
	return result, resultErr
}

func (s *session) readBoardModelBatch(ctx context.Context, batch *pgx.Batch) error {
	timed, ok := s.db.(timedQueries)
	if !ok {
		return corestore.ErrInvalidPage
	}
	sender, ok := timed.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return corestore.ErrInvalidPage
	}
	started := time.Now()
	err := sender.SendBatch(ctx, batch).Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	return mapError(ctx, err)
}

const boardModelMetadataSQL = `SELECT coalesce((SELECT sequence FROM native_board_sequences WHERE organization_id=$1 AND project_id=$2),0),EXISTS(SELECT 1 FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2) OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT page_ready) OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT list_ready),(SELECT queued_at FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 ORDER BY queued_at LIMIT 1)`

func (s *session) boardSearchModelPages(ctx context.Context, selection viewstore.BoardSelection, work *viewstore.BoardSelection) (viewstore.BoardModelPage, error) {
	var result viewstore.BoardModelPage
	if _, _, err := boardWhere(s.organization, selection); err != nil {
		return result, err
	}
	var pending, numberPending bool
	batch := &pgx.Batch{}
	batch.Queue(boardModelMetadataSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&result.Sequence, &result.Pending, &result.OldestPendingAt)
	})
	batch.Queue(boardSearchPendingSQL, s.organization, selection.ProjectID, selection.Archived, selection.Reporter, selection.ReporterPrincipalID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&pending)
	})
	if work != nil && !work.SearchRanked {
		batch.Queue(boardWorkOrderPendingSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error {
			return row.Scan(&numberPending)
		})
	}
	if err := s.readBoardModelBatch(ctx, batch); err != nil {
		return result, err
	}
	if pending {
		return result, searchstore.ErrIndexPending
	}
	var err error
	result.Records, err = s.boardPage(ctx, selection, true, nil)
	if err != nil {
		return result, err
	}
	if work != nil {
		result.WorkRecords, err = s.boardPage(ctx, *work, true, new(!numberPending))
	}
	return result, err
}
