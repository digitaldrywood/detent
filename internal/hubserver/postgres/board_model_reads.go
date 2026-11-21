package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/searchsql"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

const boardModelDisplayColumns = `b.card_display_ready,b.card_display_exact,b.card_provenance_provider IS NOT NULL,coalesce(b.card_provenance_provider,''),coalesce(b.card_provenance_external_id,''),coalesce(b.card_provenance_author_id,''),coalesce(b.card_provenance_author_display_name,''),coalesce(b.card_provenance_created_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_updated_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_observed_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_principal_id,''),coalesce(b.card_title,''),coalesce(b.card_actor_kind,''),coalesce(b.card_actor_via_key,''),coalesce(b.card_actor_display_name,''),coalesce(b.card_actor_agent,''),coalesce(b.card_reason,''),coalesce(b.card_status_line,''),coalesce(b.card_attempt_id,''),coalesce(b.card_attempt_count,0),coalesce(b.card_attempt_status,''),coalesce(b.card_attempt_runner_id,''),coalesce(b.card_attempt_machine_id,''),coalesce(b.card_attempt_started_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_attempt_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_execution_id,''),coalesce(b.card_execution_running,false),coalesce(b.card_execution_stage,''),coalesce(b.card_execution_runner_id,''),b.card_execution_started_at,coalesce(b.card_execution_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_id,''),coalesce(b.card_change_title,''),coalesce(b.card_change_status,''),coalesce(b.card_change_external_provider,''),coalesce(b.card_change_external_id,''),coalesce(b.card_change_external_url,''),coalesce(b.card_attempt_identity_role,''),coalesce(b.card_attempt_identity_backend,''),coalesce(b.card_attempt_identity_model,''),coalesce(b.card_attempt_identity_effort,''),coalesce(b.card_execution_identity_role,''),coalesce(b.card_execution_identity_backend,''),coalesce(b.card_execution_identity_model,''),coalesce(b.card_execution_identity_effort,''),coalesce(b.issue_number,0),b.card_change_base_status IS NOT NULL,coalesce(b.card_change_version_id,''),coalesce(b.card_change_policy_id,''),coalesce(b.card_change_review_policy_id,''),coalesce(b.card_change_base_status,'')`

func (s *session) boardModelWhere(f viewstore.BoardSelection) (string, []any, error) {
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
	return where, args, nil
}

func (s *session) boardModelQuery(f viewstore.BoardSelection, overlay bool) (string, []any, error) {
	if f.Limit < 1 || f.Limit > 2001 {
		return "", nil, corestore.ErrInvalidPage
	}
	where, args, err := s.boardModelWhere(f)
	if err != nil {
		return "", nil, err
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
	limit := fmt.Sprintf("$%d", len(args))
	page := "SELECT b.* FROM native_board_items b WHERE " + where + " ORDER BY " + order + " LIMIT " + limit
	cte, running := "", "b.running"
	laneRank, state, dispatchable := "b.lane_rank", "b.state", "b.dispatchable"
	columns := boardPageColumns
	if f.Compact {
		columns = boardListColumns
	}
	if overlay || f.WorkOrder {
		cte = projectBoardDirty + ","
		page, order = boardModelOverlayPage(f.WorkOrder, where, order, limit)
		laneRank, state, dispatchable = "b.effective_lane_rank", "b.effective_state", "b.effective_dispatchable"
		running = "b.effective_running"
		columns = strings.NewReplacer(
			"b.page_state", state, "b.list_state", state,
			"b.page_archived", "b.effective_archived", "b.list_archived", "b.effective_archived",
			"b.page_terminal", "b.effective_terminal", "b.list_terminal", "b.effective_terminal",
		).Replace(columns)
	}
	model := "true AS access_allowed,b.source_revision,b.source_event_sequence,b.source_project_revision," +
		laneRank + " AS lane_rank," + state + " AS state,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at," +
		running + " AS running," + dispatchable + " AS dispatchable,b.issue_number,b.page_list_bytes," + boardModelDisplayColumns + "," +
		columns + "," + publicActorNameSQL("b.organization_id", "b.page_actor_principal_id") + "," + boardviewsql.CurrentCardLease
	query := "WITH " + cte + "page AS MATERIALIZED (" + page + ") SELECT " + model + " FROM page b ORDER BY " + order
	return query, args, nil
}

var projectBoardDirty = boardviewsql.DirtyItems("SELECT $2::text AS id")

const boardModelDirtyMatch = "d.project_id=b.project_id AND d.work_item_id=b.work_item_id"
const boardModelClean = " AND b.work_item_id <> ALL(ARRAY(SELECT work_item_id FROM dirty))"

const boardModelWorkOrder = "effective_running DESC,b.dispatchable DESC,b.issue_number DESC,b.identifier"

var boardModelDirtyFields = strings.NewReplacer(
	"b.lane_rank", "coalesce(d.board_rank,b.lane_rank)",
	"b.state", "coalesce(d.state,b.state)",
	"b.archived", "coalesce(d.archived,b.archived)",
	"b.terminal", "coalesce(d.terminal,b.terminal)",
	"b.dispatchable", "coalesce(d.dispatchable,b.dispatchable)",
)

var boardModelEffectiveFields = strings.NewReplacer(
	"b.lane_rank", "b.effective_lane_rank",
	"b.state", "b.effective_state",
	"b.dispatchable", "b.effective_dispatchable",
)

func boardModelOverlayPage(work bool, where, order, limit string) (string, string) {
	const fields = "b.lane_rank AS effective_lane_rank,b.state AS effective_state,b.archived AS effective_archived,b.terminal AS effective_terminal,b.dispatchable AS effective_dispatchable"
	dirtyWhere := boardModelDirtyFields.Replace(where)
	clean := " FROM native_board_items b WHERE " + where + boardModelClean
	cleanColumns := "SELECT b.*," + fields + ","
	dirtyColumns := "SELECT b.*," + boardModelDirtyFields.Replace(fields) + ","
	tail := " LIMIT " + limit
	running := "b.running"
	prefix := ""
	if work {
		order = boardModelWorkOrder
		running = boardviewsql.RunningCard
	}
	dirty := "(" + dirtyColumns + running + " AS effective_running FROM dirty d JOIN native_board_items b ON " +
		boardModelDirtyMatch + " WHERE " + dirtyWhere + " ORDER BY " + boardModelDirtyFields.Replace(order) + tail + ")"
	pages := "(" + cleanColumns + "b.running AS effective_running" + clean + " ORDER BY " + order + tail + ") UNION ALL " + dirty
	if work {
		const liveMatch = "l.project_id=b.project_id AND l.work_item_id=b.work_item_id"
		const inventoryOrder = " ORDER BY b.dispatchable DESC,b.issue_number DESC,b.identifier"
		prefix = "WITH " + boardviewsql.LiveItems("$1", "$2::text") + ", live_page AS MATERIALIZED (" +
			cleanColumns + "true AS effective_running FROM live_items l JOIN native_board_items b ON " + liveMatch +
			" WHERE " + where + boardModelClean + " AND l.running" + inventoryOrder + tail + ") "
		remaining := " LIMIT (SELECT greatest(" + limit + "-count(*),0) FROM live_page)"
		idle := cleanColumns + "false AS effective_running" + clean
		pages = "SELECT * FROM live_page UNION ALL " + dirty + " UNION ALL (" +
			idle + " AND NOT b.running" + inventoryOrder + remaining + ") UNION ALL (" +
			idle + " AND b.running AND b.work_item_id <> ALL(ARRAY(SELECT work_item_id FROM live_items WHERE running))" + inventoryOrder + remaining + ")"
	}
	order = boardModelEffectiveFields.Replace(order)
	return prefix + "SELECT * FROM (" + pages + ") b ORDER BY " + order + tail, order
}

var boardModelParameter = regexp.MustCompile(`\$[0-9]+`)

func (s *session) BoardModelPages(ctx context.Context, selection viewstore.BoardSelection, work *viewstore.BoardSelection, authority accessstore.Authority) (result viewstore.BoardModelPage, resultErr error) {
	if selection.SearchText != "" || selection.Fingerprint != "" {
		result, resultErr = s.boardSearchModelPages(ctx, selection, work)
		return result, s.boardSearchFailure(ctx, selection, resultErr)
	}
	query, args, err := s.boardModelQuery(selection, work != nil)
	if err != nil {
		return result, err
	}
	query = "SELECT 1 AS page_part, model.* FROM (" + query + ") model"
	if work != nil {
		workSQL, workArgs, err := s.boardModelQuery(*work, true)
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
	if selection.Summary {
		summarySQL, summaryArgs, err := s.boardSummaryQuery(selection, true)
		if err != nil {
			return result, err
		}
		batch.Queue(summarySQL, summaryArgs...).Query(func(rows pgx.Rows) error {
			defer rows.Close()
			for rows.Next() {
				var summary viewstore.BoardSummaryRecord
				targets := append([]any{&result.Sequence, &result.Pending, &result.OldestPendingAt}, boardSummaryTargets(&summary)...)
				if err := rows.Scan(targets...); err != nil {
					return err
				}
				if summary.Kind != "" {
					result.Summaries = append(result.Summaries, summary)
				}
			}
			return rows.Err()
		})
	} else {
		batch.Queue(boardModelMetadataSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error { return row.Scan(&result.Sequence, &result.Pending, &result.OldestPendingAt) })
	}

	if selection.CountLanes {
		where, laneArgs, err := s.boardModelWhere(selection)
		if err != nil {
			return result, err
		}
		laneSQL := "WITH " + projectBoardDirty + " SELECT " + boardModelDirtyFields.Replace("b.state") +
			",count(*),count(*) FILTER (WHERE " + boardviewsql.RunningCard + " AND NOT " + boardModelDirtyFields.Replace("b.terminal") +
			") FROM native_board_items b LEFT JOIN dirty d ON " + boardModelDirtyMatch + " WHERE " +
			boardModelDirtyFields.Replace(where) + " GROUP BY 1 ORDER BY 1 LIMIT 201"
		batch.Queue(laneSQL, laneArgs...).Query(func(rows pgx.Rows) error {
			defer rows.Close()
			result.Lanes = []viewstore.BoardLane{}
			for rows.Next() {
				var lane viewstore.BoardLane
				if err := rows.Scan(&lane.State, &lane.Total, &lane.Running); err != nil {
					return err
				}
				result.Lanes = append(result.Lanes, lane)
			}
			if len(result.Lanes) > 200 {
				return corestore.ErrInvalidPage
			}
			return rows.Err()
		})
	}
	batch.Queue(query, args...).Query(func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var row viewstore.BoardRecord
			row.ActorDisplayName = new(string)
			var part, number int
			var allowed, current bool
			targets := []any{&part, &allowed, &row.SourceRevision, &row.SourceEventSequence, &row.SourceProjectRevision, &row.Position.Lane, &row.Position.State, &row.Position.Priority, &row.Position.ActivityMissing, &row.Position.Activity, &row.Position.Identifier, &row.ClosedAt, &row.Running, &row.Dispatchable, &number, &row.ListBytes}
			display, finishDisplay := boardviewsql.DisplayTargets(&row.Display)
			page, finishPage := boardPageTargets(&row)
			targets = append(targets, display...)
			targets = append(targets, page...)
			targets = append(targets, row.ActorDisplayName, &current)
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			if !allowed {
				return accessstore.ErrDenied
			}
			finishDisplay()
			if !current {
				row.Running = false
				row.Display.StatusLine = viewstore.ExpireLease(row.Display.Attempt, row.Display.Execution, row.Display.StatusLine)
			}
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

const boardSearchTimeoutMargin = 50 * time.Millisecond

var errBoardSearchRefused = errors.Join(searchstore.ErrBusy, context.DeadlineExceeded)

const boardModelMetadataSQL = `WITH sample AS MATERIALIZED (
 SELECT array_agg(d.work_item_id) AS ids,min(d.queued_at) AS oldest FROM (
  SELECT work_item_id,queued_at FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 ORDER BY queued_at LIMIT 257
 ) d
)
SELECT coalesce((SELECT sequence FROM native_board_sequences WHERE organization_id=$1 AND project_id=$2),0) AS sequence,
 CASE WHEN sample.ids IS NULL THEN false WHEN cardinality(sample.ids)>256 THEN true ELSE
  cardinality(sample.ids) <> (SELECT count(matched.work_item_id) FROM (
   SELECT b.work_item_id,b.actor_principal_id FROM native_board_items b
   WHERE b.organization_id=$1 AND b.project_id=$2 AND b.work_item_id=ANY(sample.ids)
   INTERSECT
   SELECT i.native_id,coalesce(i.actor_principal_id,'') FROM issues i
   WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=ANY(sample.ids)
  ) matched)
 END OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT page_ready)
 OR EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT list_ready) AS pending,
 sample.oldest FROM sample`

func (s *session) boardSearchModelPages(ctx context.Context, selection viewstore.BoardSelection, work *viewstore.BoardSelection) (viewstore.BoardModelPage, error) {
	var result viewstore.BoardModelPage
	batch := &pgx.Batch{}
	ctx, cancel, err := s.beginBoardSearch(ctx, selection, batch)
	defer cancel()
	if err != nil {
		return result, err
	}

	if _, _, err := boardWhere(s.organization, selection); err != nil {
		return result, err
	}
	var numberPending bool
	batch.Queue(boardModelMetadataSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&result.Sequence, &result.Pending, &result.OldestPendingAt)
	})
	if selection.SearchRanked && selection.SearchText != "" {
		windowArgs := []any{s.organization, selection.ProjectID}
		batch.Queue("SELECT count(*),min(w.number) FROM "+searchsql.Window(&windowArgs, selection, "number")+" w", windowArgs...).QueryRow(func(row pgx.Row) error {
			var scanned int64
			var lowest *int64
			if err := row.Scan(&scanned, &lowest); err != nil {
				return err
			}
			if scanned == searchstore.WindowRows && lowest != nil {
				result.SearchWindowEnd = lowest
			}
			return nil
		})
	}
	if selection.SearchRanked && selection.SearchText == "" && selection.Fingerprint != "" {
		scanArgs := []any{s.organization, selection.ProjectID}
		batch.Queue("SELECT count(*),max(k.work_item_id) FROM "+searchsql.FingerprintScan(&scanArgs, selection)+" k", scanArgs...).QueryRow(func(row pgx.Row) error {
			var scanned int64
			var last *string
			if err := row.Scan(&scanned, &last); err != nil {
				return err
			}
			if scanned == searchstore.WindowRows && last != nil {
				result.SearchFingerprintEnd = last
			}
			return nil
		})
	}
	if work != nil && !work.SearchRanked {
		batch.Queue(boardWorkOrderPendingSQL, s.organization, selection.ProjectID).QueryRow(func(row pgx.Row) error {
			return row.Scan(&numberPending)
		})
	}
	if err := s.readBoardModelBatch(ctx, batch); err != nil {
		return result, err
	}
	result.Records, err = s.boardPage(ctx, selection, true, nil)
	if err != nil {
		return result, err
	}
	if work != nil {
		result.WorkRecords, err = s.boardPage(ctx, *work, true, new(!numberPending))
	}
	return result, err
}

func (s *session) beginBoardSearch(ctx context.Context, selection viewstore.BoardSelection, batches ...*pgx.Batch) (context.Context, context.CancelFunc, error) {
	if selection.SearchText == "" && selection.Fingerprint == "" {
		return ctx, func() {}, nil
	}
	initial := s.searchDeadline.IsZero()
	if initial {
		budget := s.searchBudget
		if budget <= 0 {
			budget = defaultBoardSearchBudget
		}
		s.searchDeadline, s.searchBudgetLimits = time.Now().Add(budget), true
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(s.searchDeadline) {
			s.searchDeadline, s.searchBudgetLimits = deadline, false
		}
	}
	ctx, cancel := context.WithDeadline(ctx, s.searchDeadline)
	if err := ctx.Err(); err != nil {
		return ctx, cancel, err
	}
	if initial {
		milliseconds := max(1, (time.Until(s.searchDeadline) - boardSearchTimeoutMargin/2).Milliseconds())
		lockKey := fmt.Sprintf("board-search:%q:%q", s.organization, selection.ReporterPrincipalID)
		const statement = `SELECT set_config('statement_timeout',$1,true) IS NOT NULL AND pg_try_advisory_xact_lock(hashtextextended($2,0))`
		if len(batches) > 0 {
			batches[0].Queue(statement, fmt.Sprintf("%dms", milliseconds), lockKey).QueryRow(func(row pgx.Row) error {
				if err := row.Scan(&s.searchAdmitted); err != nil {
					return err
				}
				if !s.searchAdmitted {
					return errBoardSearchRefused
				}
				return nil
			})
			return ctx, cancel, nil
		}
		var admitted bool
		err := s.db.QueryRow(ctx, statement, fmt.Sprintf("%dms", milliseconds), lockKey).Scan(&admitted)
		if err != nil {
			return ctx, cancel, mapError(ctx, err)
		}
		s.searchAdmitted = admitted
	}
	if !s.searchAdmitted {
		return ctx, cancel, errBoardSearchRefused
	}
	return ctx, cancel, nil
}
