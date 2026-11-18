package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/searchsql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var boardColumns = `b.organization_id,b.project_id,b.work_item_id,b.source_revision,b.source_event_sequence,b.source_project_revision,
b.state,b.archived,b.terminal,b.running,b.dispatchable,b.lane_rank,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at,b.expires_at,b.execution_fence,
b.actor_principal_id,b.issue_type,b.verification_state,b.priority,b.created_at,b.updated_at,b.payload_object_key,b.payload_sha256,b.payload_bytes,b.payload_content_bytes,
ARRAY(SELECT value FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='label' ORDER BY value),
ARRAY(SELECT value FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='assignee' ORDER BY value),` + boardviewsql.DisplayColumns + "," + boardPageColumns + "," + boardListColumns + ",b.page_list_bytes,b.page_window_mask,b.page_refresh_after," + publicActorNameSQL("b.organization_id", "b.page_actor_principal_id")

func scanBoard(row interface{ Scan(...any) error }, extra ...any) (viewstore.BoardRecord, error) {
	var b viewstore.BoardRecord
	b.ActorDisplayName = new(string)
	var key, digest string
	var size, content int64
	targets := []any{&b.OrganizationID, &b.ProjectID, &b.WorkItemID, &b.SourceRevision, &b.SourceEventSequence, &b.SourceProjectRevision,
		&b.Position.State, &b.Archived, &b.Terminal, &b.Running, &b.Dispatchable, &b.Position.Lane, &b.Position.Priority, &b.Position.ActivityMissing, &b.Position.Activity, &b.Position.Identifier, &b.ClosedAt, &b.ExpiresAt, &b.ExecutionFence,
		&b.ActorPrincipalID, &b.IssueType, &b.VerificationState, &b.Priority, &b.CreatedAt, &b.UpdatedAt, &key, &digest, &size, &content, &b.Labels, &b.Assignees}
	display, finish := boardviewsql.DisplayTargets(&b.Display)
	page, finishPage := boardPageTargets(&b)
	targets = append(targets, display...)
	list, finishList := boardListTargets(&b)
	targets = append(targets, page...)
	targets = append(targets, list...)
	targets = append(targets, &b.ListBytes)
	targets = append(targets, &b.WindowMask, &b.RefreshAfter, b.ActorDisplayName)
	targets = append(targets, extra...)
	err := row.Scan(targets...)
	finish()
	finishPage()
	finishList()
	b.Payload = eventstore.Reference(key, digest, size, content)
	return b, err
}

func (s *session) NextBoardCandidate(ctx context.Context, after tracker.NativeWorkItemID, attempted ...tracker.NativeWorkItemID) (viewstore.BoardCandidate, error) {
	skipped := make([]string, len(attempted))
	for index, id := range attempted {
		skipped[index] = string(id)
	}
	var candidate viewstore.BoardCandidate
	err := s.db.QueryRow(ctx, `SELECT coalesce(stale.project_id,candidates.project_id),candidates.work_item_id,dirty.queued_at,candidates.priority<4 FROM (
 (SELECT project_id,work_item_id,0 AS priority FROM native_board_dirty WHERE organization_id=$1 AND work_item_id>$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,1 AS priority FROM native_board_dirty WHERE organization_id=$1 AND work_item_id<=$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,2 AS priority FROM native_board_items WHERE organization_id=$1 AND NOT card_display_ready AND work_item_id>$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,3 AS priority FROM native_board_items WHERE organization_id=$1 AND NOT card_display_ready AND work_item_id<=$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,2 AS priority FROM native_board_items WHERE organization_id=$1 AND NOT page_ready AND work_item_id>$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,3 AS priority FROM native_board_items WHERE organization_id=$1 AND NOT page_ready AND work_item_id<=$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,native_id AS work_item_id,4 AS priority FROM issues WHERE organization_id=$1 AND native_id>$2 AND native_id<>ALL($3::text[]) ORDER BY native_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,4 AS priority FROM native_board_items WHERE organization_id=$1 AND work_item_id>$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,native_id AS work_item_id,5 AS priority FROM issues WHERE organization_id=$1 AND native_id<=$2 AND native_id<>ALL($3::text[]) ORDER BY native_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,5 AS priority FROM native_board_items WHERE organization_id=$1 AND work_item_id<=$2 AND work_item_id<>ALL($3::text[]) ORDER BY work_item_id LIMIT 1)
 ) candidates
 LEFT JOIN native_board_items stale ON stale.organization_id=$1 AND stale.work_item_id=candidates.work_item_id
  AND stale.project_id<>candidates.project_id
  AND EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=candidates.project_id AND i.native_id=candidates.work_item_id)
 LEFT JOIN native_board_dirty dirty ON dirty.organization_id=$1 AND dirty.project_id=coalesce(stale.project_id,candidates.project_id) AND dirty.work_item_id=candidates.work_item_id
 ORDER BY candidates.priority,candidates.work_item_id LIMIT 1`, s.organization, after, skipped).Scan(&candidate.ProjectID, &candidate.WorkItemID, &candidate.QueuedAt, &candidate.Required)
	return candidate, mapError(ctx, err)
}

const boardIssueVersion = `COALESCE((SELECT xmin FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3),'0'::xid)`

func (s *session) BoardProjectionVersion(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.BoardProjectionVersion, error) {
	var version viewstore.BoardProjectionVersion
	err := s.db.QueryRow(ctx, `SELECT `+boardIssueVersion+`,
 COALESCE((SELECT xmin FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3),'0'::xid),
 COALESCE((SELECT xmin FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3),'0'::xid)`, s.organization, project, item).Scan(&version.Issue, &version.Published, &version.Dirty)
	return version, mapError(ctx, err)
}

func (s *session) BoardItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.BoardRecord, error) {
	row, _, err := s.boardItemVersion(ctx, project, item)
	return row, err
}

func (s *session) boardItemVersion(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.BoardRecord, uint32, error) {
	var version uint32
	row, err := scanBoard(s.db.QueryRow(ctx, "SELECT "+boardColumns+",b.xmin FROM native_board_items b WHERE b.organization_id=$1 AND b.project_id=$2 AND b.work_item_id=$3", s.organization, project, item), &version)
	return row, version, mapError(ctx, err)
}

func (s *session) boardSelectionWhere(ctx context.Context, selection viewstore.BoardSelection) (string, []any, error) {
	_, cancel, budgetErr := s.beginBoardSearch(ctx, selection)
	defer cancel()
	if budgetErr != nil {
		return "", nil, budgetErr
	}
	return boardWhere(s.organization, selection)
}

func boardWhere(org tracker.OrganizationID, f viewstore.BoardSelection) (string, []any, error) {
	if f.ProjectID == "" {
		return "", nil, corestore.ErrNotFound
	}
	args := []any{org, f.ProjectID}
	clauses := []string{"b.organization_id=$1", "b.project_id=$2"}
	var reporterPredicate string
	if f.SearchText != "" || f.Fingerprint != "" {
		owned, ownedNumber := func(string) string { return "true" }, ""
		if f.Reporter {
			if f.ReporterPrincipalID == "" {
				return "", nil, corestore.ErrNotFound
			}
			args = append(args, f.ReporterPrincipalID)
			principal := len(args)
			owned = func(alias string) string {
				return fmt.Sprintf("EXISTS(SELECT 1 FROM issues ri WHERE ri.organization_id=$1 AND ri.project_id=$2 AND ri.native_id=%s.work_item_id AND ri.actor_principal_id=$%d OFFSET 0)", alias, principal)
			}
			ownedNumber = fmt.Sprintf(" AND i.actor_principal_id=$%d", principal)
		}
		var matches []string
		if f.SearchText != "" {
			if err := (searchstore.Query{Kind: "work", ProjectID: f.ProjectID, Text: f.SearchText, Limit: 1}).Validate(); err != nil {
				return "", nil, err
			}
			window := searchsql.Window(&args, f, "work_item_id,terms")
			args = append(args, searchstore.WindowQuery(f.SearchText))
			match := fmt.Sprintf("w.terms @@ to_tsquery('pg_catalog.english',$%d)", len(args))
			if number, name := searchstore.Number(f.SearchText); number > 0 {
				args = append(args, number, name)
				matches = append(matches, fmt.Sprintf("SELECT i.native_id FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.number=$%d AND ($%d='' OR lower(i.project_id)=lower($%d) OR lower(p.name)=lower($%d))", len(args)-1, len(args), len(args), len(args))+ownedNumber)
			}
			matches = append(matches, "SELECT w.work_item_id FROM "+window+" w WHERE "+match+" AND "+owned("w"))
		}
		if f.Fingerprint != "" && len(matches) > 0 {
			args = append(args, searchstore.FingerprintDigest(f.Fingerprint))
			matches = []string{fmt.Sprintf("SELECT m.work_item_id FROM (%s) m(work_item_id) WHERE EXISTS(SELECT 1 FROM native_work_item_fingerprints f WHERE f.organization_id=$1 AND f.project_id=$2 AND f.fingerprint_digest=$%d AND f.work_item_id=m.work_item_id)", strings.Join(matches, " UNION "), len(args))}
		} else if f.Fingerprint != "" {
			matches = []string{"SELECT k.work_item_id FROM " + searchsql.FingerprintScan(&args, f) + " k WHERE " + owned("k")}
		}
		clauses = append(clauses, "b.work_item_id IN ("+strings.Join(matches, " UNION ")+")")
	}
	add := func(column, operator string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s %s $%d", column, operator, len(args)))
	}
	if f.WorkItemIDs != nil {
		if len(f.WorkItemIDs) > corestore.MaxPageSize {
			return "", nil, corestore.ErrInvalidPage
		}
		ids := make([]string, len(f.WorkItemIDs))
		for index, id := range f.WorkItemIDs {
			if id == "" {
				return "", nil, corestore.ErrInvalidPage
			}
			ids[index] = string(id)
		}
		args = append(args, ids)
		clauses = append(clauses, fmt.Sprintf("b.work_item_id=ANY($%d::text[])", len(args)))
	}
	if f.Reporter && f.SearchText == "" && f.Fingerprint == "" {
		if f.ReporterPrincipalID == "" {
			return "", nil, corestore.ErrNotFound
		}
		args = append(args, f.ReporterPrincipalID)
		reporterPredicate = fmt.Sprintf("EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND i.actor_principal_id=$%d)", len(args))
		clauses = append(clauses, reporterPredicate)
	}
	if f.ActorPrincipalID != "" {
		add("b.actor_principal_id", "=", f.ActorPrincipalID)
	}
	if f.Archived != nil {
		add("b.archived", "=", *f.Archived)
	}
	if f.ExcludeWorkspace {
		clauses = append(clauses, `NOT EXISTS(SELECT 1 FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='label' AND f.value='detent:workspace')`)
	}
	for _, filter := range []struct {
		column   string
		operator string
		at       *time.Time
	}{{"i.native_created_at", ">=", f.NativeCreatedAfter}, {"i.native_created_at", "<", f.NativeCreatedBefore}, {"i.native_updated_at", ">=", f.NativeUpdatedAfter}} {
		if filter.at != nil {
			args = append(args, *filter.at)
			clauses = append(clauses, fmt.Sprintf("EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND %s %s $%d)", filter.column, filter.operator, len(args)))
		}
	}
	if f.Terminal != nil {
		add("b.terminal", "=", *f.Terminal)
	}
	for _, field := range []struct {
		column string
		values []string
	}{{"b.state", f.States}, {"b.issue_type", f.Types}, {"b.verification_state", f.Verification}} {
		if len(field.values) > 200 {
			return "", nil, corestore.ErrInvalidPage
		}
		if len(field.values) == 1 {
			add(field.column, "=", field.values[0])
		} else if len(field.values) > 1 {
			args = append(args, field.values)
			clauses = append(clauses, fmt.Sprintf("%s=ANY($%d::text[])", field.column, len(args)))
		}
	}
	if len(f.Priorities) > 4 {
		return "", nil, corestore.ErrInvalidPage
	}
	if len(f.Priorities) > 0 {
		args = append(args, f.Priorities)
		clauses = append(clauses, fmt.Sprintf("b.priority=ANY($%d::integer[])", len(args)))
	}
	for _, field := range []struct {
		kind   string
		values []string
	}{{"label", f.Labels}, {"assignee", f.Assignees}} {
		if len(field.values) > 200 {
			return "", nil, corestore.ErrInvalidPage
		}
		if len(field.values) > 0 {
			args = append(args, field.kind, field.values)
			clauses = append(clauses, fmt.Sprintf("EXISTS(SELECT 1 FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind=$%d AND f.value=ANY($%d::text[]))", len(args)-1, len(args)))
		}
	}
	for _, field := range []struct {
		column, operator string
		at               *time.Time
	}{{"b.created_at", ">=", f.CreatedAfter}, {"b.created_at", "<", f.CreatedBefore}, {"b.updated_at", ">=", f.UpdatedAfter}, {"b.closed_at", ">=", f.CompletedFrom}, {"b.closed_at", "<", f.CompletedTo}} {
		if field.at != nil {
			add(field.column, field.operator, *field.at)
		}
	}
	return strings.Join(clauses, " AND "), args, nil
}

func (s *session) BoardPage(ctx context.Context, f viewstore.BoardSelection) ([]viewstore.BoardRecord, error) {
	rows, err := s.boardPage(ctx, f, false, nil)
	return rows, s.boardSearchFailure(ctx, f, err)
}

func (s *session) boardPage(ctx context.Context, f viewstore.BoardSelection, typed bool, workOrderReady *bool) ([]viewstore.BoardRecord, error) {
	ctx, cancel, budgetErr := s.beginBoardSearch(ctx, f)
	defer cancel()
	if budgetErr != nil {
		return nil, budgetErr
	}

	if typed {
		if f.Limit < 1 || f.Limit > 2001 {
			return nil, corestore.ErrInvalidPage
		}
	} else if err := checkPage(f.Limit); err != nil {
		return nil, err
	}
	if f.SearchRanked && (f.SearchText != "" || f.Fingerprint != "") {
		return s.boardSearchPage(ctx, f, typed)
	}
	where, args, err := boardWhere(s.organization, f)
	if !typed {
		where, args, err = s.boardSelectionWhere(ctx, f)
	}
	if err != nil {
		return nil, err
	}
	if typed {
		where += " AND b.page_ready"
		if f.Compact {
			where += " AND b.list_ready"
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
	projectedWorkOrder := false
	if f.WorkOrder {
		if workOrderReady != nil {
			projectedWorkOrder = *workOrderReady
		} else {
			var pending bool
			if err := s.db.QueryRow(ctx, boardWorkOrderPendingSQL, s.organization, f.ProjectID).Scan(&pending); err != nil {
				return nil, mapError(ctx, err)
			}
			projectedWorkOrder = !pending
		}
		order = "b.running DESC,b.dispatchable DESC,i.number DESC,b.identifier"
		if projectedWorkOrder {
			order = "b.running DESC,b.dispatchable DESC,b.issue_number DESC,b.identifier"
			where += " AND (SELECT true FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id LIMIT 1)"
		}
	}
	candidateSQL := ""
	if f.SearchText != "" || f.Fingerprint != "" {
		candidateSQL = "WITH search_candidates AS MATERIALIZED (SELECT b.organization_id,b.project_id,b.work_item_id FROM native_board_items b WHERE " + where + ") "
		where = "b.organization_id=$1 AND b.project_id=$2"
	}
	if p := f.After; p != nil {
		base := len(args)
		if f.ClosedOrder {
			args = append(args, p.Activity, p.Identifier)
			where += fmt.Sprintf(" AND (coalesce(b.closed_at,'-infinity'::timestamptz)<coalesce($%d::timestamptz,'-infinity'::timestamptz) OR (coalesce(b.closed_at,'-infinity'::timestamptz)=coalesce($%d::timestamptz,'-infinity'::timestamptz) AND b.identifier>$%d))", base+1, base+1, base+2)
		} else {
			values := []any{p.Lane, p.State, p.Priority, p.ActivityMissing}
			if len(f.States) == 1 {
				values = values[2:]
			}
			holders := make([]string, len(values))
			for i, v := range values {
				args = append(args, v)
				holders[i] = fmt.Sprintf("$%d", len(args))
			}
			tuple := "(" + strings.Join(holders, ",") + ")"
			args = append(args, p.Activity, p.Identifier)
			at, id := len(args)-1, len(args)
			where += fmt.Sprintf(" AND ((%s)>%s OR ((%s)=%s AND (coalesce(b.activity_at,'-infinity'::timestamptz)<coalesce($%d::timestamptz,'-infinity'::timestamptz) OR (coalesce(b.activity_at,'-infinity'::timestamptz)=coalesce($%d::timestamptz,'-infinity'::timestamptz) AND b.identifier>$%d))))", prefix, tuple, prefix, tuple, at, at, id)
		}
	}
	args = append(args, f.Limit)
	from := " FROM native_board_items b"
	if candidateSQL != "" {
		from += " JOIN search_candidates candidate ON candidate.organization_id=b.organization_id AND candidate.project_id=b.project_id AND candidate.work_item_id=b.work_item_id"
	}
	if f.WorkOrder && !projectedWorkOrder {
		from += " JOIN issues i ON i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id"
	}
	rows, err := s.db.Query(ctx, candidateSQL+"SELECT "+boardColumns+from+" WHERE "+where+" ORDER BY "+order+fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardRecord{}
	for rows.Next() {
		row, err := scanBoard(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardLanes(ctx context.Context, f viewstore.BoardSelection, now time.Time) (lanes []viewstore.BoardLane, err error) {
	defer func(parent context.Context, selection viewstore.BoardSelection) {
		err = s.boardSearchFailure(parent, selection, err)
	}(ctx, f)
	ctx, cancel, budgetErr := s.beginBoardSearch(ctx, f)
	defer cancel()
	if budgetErr != nil {
		return nil, budgetErr
	}

	f.States = nil
	f.Terminal = nil
	where, args, err := s.boardSelectionWhere(ctx, f)
	if err != nil {
		return nil, err
	}
	args = append(args, now)
	current := fmt.Sprintf("b.running AND b.expires_at>$%d AND EXISTS(SELECT 1 FROM issues i JOIN leases l ON l.organization_id=i.organization_id AND l.issue_id=i.id WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND l.fencing_token=b.execution_fence AND l.released_at IS NULL AND l.renewed_at<=$%d AND l.expires_at>$%d)", len(args), len(args), len(args))
	statement := "SELECT b.state,count(*),count(*) FILTER (WHERE " + current + ") FROM native_board_items b WHERE " + where + " GROUP BY b.state ORDER BY b.state LIMIT 201"
	filtered := f.SearchText != "" || f.WorkItemIDs != nil || f.Reporter || f.ActorPrincipalID != "" || f.Fingerprint != "" || len(f.Types)+len(f.Verification)+len(f.Labels)+len(f.Assignees)+len(f.Priorities) > 0 || f.CreatedAfter != nil || f.CreatedBefore != nil || f.UpdatedAfter != nil || f.NativeCreatedAfter != nil || f.NativeCreatedBefore != nil || f.NativeUpdatedAfter != nil || f.CompletedFrom != nil || f.CompletedTo != nil
	if !filtered && f.ExcludeWorkspace {
		if err := s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND NOT workspace_counted)", s.organization, f.ProjectID).Scan(&filtered); err != nil {
			return nil, mapError(ctx, err)
		}
	}
	if !filtered {
		total := "c.total"
		if f.ExcludeWorkspace {
			total = "(c.total-c.workspace_total)"
			current += " AND NOT EXISTS(SELECT 1 FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='label' AND f.value='detent:workspace')"
		}
		counterWhere := "c.organization_id=$1 AND c.project_id=$2 AND " + total + ">0"
		if f.Archived != nil {
			counterWhere += " AND c.archived=$3"
		}
		statement = "SELECT c.state,sum(" + total + ")::bigint,sum((SELECT count(*) FROM native_board_items b WHERE b.organization_id=c.organization_id AND b.project_id=c.project_id AND b.archived=c.archived AND b.state=c.state AND " + current + "))::bigint FROM native_board_counts c WHERE " + counterWhere + " GROUP BY c.state ORDER BY c.state LIMIT 201"
	}
	rows, err := s.db.Query(ctx, statement, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardLane{}
	for rows.Next() {
		var row viewstore.BoardLane
		if err := rows.Scan(&row.State, &row.Total, &row.Running); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if len(result) > 200 {
		return nil, corestore.ErrInvalidPage
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardSequence(ctx context.Context, project tracker.ProjectID) (viewstore.BoardSequence, error) {
	var result viewstore.BoardSequence
	err := s.db.QueryRow(ctx, "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=$1 AND project_id=$2", s.organization, project).Scan(&result.Sequence, &result.ReplayFloor)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	return result, mapError(ctx, err)
}

func (s *session) BoardDeliveryState(ctx context.Context, project tracker.ProjectID, subjects []viewstore.BoardDeliverySubject, now time.Time) (viewstore.BoardDeliveryState, error) {
	var state viewstore.BoardDeliveryState
	if err := checkPage(len(subjects)); err != nil {
		return state, err
	}
	ids, hashes, users, memberships, sessions := make([]string, len(subjects)), make([]string, len(subjects)), make([]string, len(subjects)), make([]string, len(subjects)), make([]string, len(subjects))
	for i, subject := range subjects {
		ids[i], hashes[i], users[i], memberships[i], sessions[i] = subject.CredentialID, subject.CredentialHash, subject.UserID, subject.MembershipID, subject.SessionHash
	}
	rows, err := s.db.Query(ctx, `WITH project_state AS MATERIALIZED (
 SELECT coalesce(b.sequence,0) AS head,coalesce(b.replay_floor,0) AS floor,
        EXISTS(SELECT 1 FROM projects WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL) AS live,
        coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='organization' AND subject_id=$1),0) AS access_revision
 FROM (VALUES (1)) seed(value)
 LEFT JOIN native_board_sequences b ON b.organization_id=$1 AND b.project_id=$2
)
SELECT p.head,p.floor,p.live,p.access_revision,
       coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='token' AND subject_id=q.credential_id),0),
       coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='member' AND subject_id=q.user_id),0),
       coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='token' AND subject_id=m.principal_id),0),
       coalesce(t.token_hash=q.credential_hash AND t.revoked_at IS NULL AND t.created_at<=$8
         AND (t.expires_at IS NULL OR t.expires_at>$8)
         AND (q.user_id='' OR (m.active AND m.membership_id=q.membership_id AND principal.id IS NOT NULL AND principal.revoked_at IS NULL
           AND (t.id=m.principal_id OR (t.hosted_user_id=q.user_id AND t.hosted_membership_id=m.membership_id))))
         AND (q.session_hash='' OR (h.identity_subject=q.user_id AND h.revoked_at IS NULL AND h.expires_at>$8 AND h.identity_expires_at>$8
           AND (COALESCE(m.sessions_revoked_before,CASE WHEN NOT m.active THEN m.updated_at END) IS NULL
             OR h.identity_created_at>COALESCE(m.sessions_revoked_before,CASE WHEN NOT m.active THEN m.updated_at END)))),false),
       least(t.expires_at,h.expires_at,h.identity_expires_at),
       coalesce(t.scope,''),coalesce(t.operator_key_scope,''),coalesce(t.operator_project_access,''),coalesce(t.native_only,false),
       coalesce(m.role,''),coalesce(m.principal_id,'')
 FROM project_state p
 CROSS JOIN unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[]) WITH ORDINALITY
 AS q(credential_id,credential_hash,user_id,membership_id,session_hash,ordinal)
 LEFT JOIN LATERAL (SELECT id,token_hash,revoked_at,created_at,expires_at,hosted_user_id,hosted_membership_id,scope,operator_key_scope,operator_project_access,native_only
   FROM api_tokens WHERE organization_id=$1 AND id=q.credential_id OFFSET 0) t ON true
 LEFT JOIN LATERAL (SELECT active,membership_id,principal_id,role,sessions_revoked_before,updated_at
   FROM hosted_members WHERE organization_id=$1 AND user_id=q.user_id OFFSET 0) m ON true
 LEFT JOIN LATERAL (SELECT id,revoked_at FROM api_tokens WHERE organization_id=$1 AND id=m.principal_id OFFSET 0) principal ON true
 LEFT JOIN LATERAL (SELECT identity_subject,revoked_at,expires_at,identity_created_at,identity_expires_at
   FROM hosted_sessions WHERE organization_id=$1 AND token_hash=q.session_hash OFFSET 0) h ON true
 ORDER BY q.ordinal`, s.organization, project, ids, hashes, users, memberships, sessions, now)
	if err != nil {
		return state, mapError(ctx, err)
	}
	defer rows.Close()
	state.Access = make([]viewstore.BoardDeliveryAccess, 0, len(subjects))
	for rows.Next() {
		var access viewstore.BoardDeliveryAccess
		var expires *time.Time
		if err := rows.Scan(&state.Head.Sequence, &state.Head.ReplayFloor, &state.ProjectLive,
			&access.Revisions[0], &access.Revisions[1], &access.Revisions[2], &access.Revisions[3], &access.Live, &expires,
			&access.CredentialScope, &access.CredentialKeyScope, &access.CredentialProjectAccess, &access.CredentialNativeOnly,
			&access.MemberRole, &access.MemberPrincipalID); err != nil {
			return viewstore.BoardDeliveryState{}, mapError(ctx, err)
		}
		if expires != nil {
			access.ExpiresAt = *expires
		}
		state.Access = append(state.Access, access)
	}
	if err := rows.Err(); err != nil {
		return viewstore.BoardDeliveryState{}, mapError(ctx, err)
	}
	if len(state.Access) != len(subjects) {
		return viewstore.BoardDeliveryState{}, viewstore.ErrInvalidView
	}
	return state, nil
}

func (s *session) BoardDeltas(ctx context.Context, project tracker.ProjectID, after int64, limit int) ([]viewstore.BoardDelta, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT sequence,work_item_id,previous_object_key,previous_sha256,previous_bytes,previous_content_bytes,current_object_key,current_sha256,current_bytes,current_content_bytes,topic,entity_id,entity_revision,kind,status,related_id FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2 AND sequence>$3 ORDER BY sequence LIMIT $4`, s.organization, project, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardDelta{}
	for rows.Next() {
		var row viewstore.BoardDelta
		var beforeKey, beforeHash, afterKey, afterHash *string
		var beforeSize, beforeContent, afterSize, afterContent *int64
		if err := rows.Scan(&row.Sequence, &row.WorkItemID, &beforeKey, &beforeHash, &beforeSize, &beforeContent, &afterKey, &afterHash, &afterSize, &afterContent, &row.Topic, &row.EntityID, &row.EntityRevision, &row.Kind, &row.Status, &row.RelatedID); err != nil {
			return nil, mapError(ctx, err)
		}
		if beforeKey != nil {
			if beforeHash == nil || beforeSize == nil || beforeContent == nil {
				return nil, eventstore.ErrInvalidRecord
			}
			ref := eventstore.Reference(*beforeKey, *beforeHash, *beforeSize, *beforeContent)
			row.Previous = &ref
		}
		if afterKey != nil {
			if afterHash == nil || afterSize == nil || afterContent == nil {
				return nil, eventstore.ErrInvalidRecord
			}
			ref := eventstore.Reference(*afterKey, *afterHash, *afterSize, *afterContent)
			row.Current = &ref
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardClosedWindows(ctx context.Context, selection viewstore.BoardSelection, starts []time.Time, until time.Time) (counts []viewstore.BoardClosedCount, err error) {
	defer func(parent context.Context, original viewstore.BoardSelection) {
		err = s.boardSearchFailure(parent, original, err)
	}(ctx, selection)
	ctx, cancel, budgetErr := s.beginBoardSearch(ctx, selection)
	defer cancel()
	if budgetErr != nil {
		return nil, budgetErr
	}

	if len(starts) == 0 || len(starts) > 3 || until.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	earliest := starts[0]
	for _, start := range starts {
		if start.IsZero() || !start.Before(until) {
			return nil, corestore.ErrInvalidPage
		}
		if start.Before(earliest) {
			earliest = start
		}
	}
	selection.Archived = nil
	selection.States = nil
	selection.Terminal = new(true)
	where, args, err := s.boardSelectionWhere(ctx, selection)
	if err != nil {
		return nil, err
	}
	args = append(args, earliest, until)
	where += fmt.Sprintf(" AND b.closed_at >= $%d AND b.closed_at < $%d", len(args)-1, len(args))
	var aggregates []string
	for _, start := range starts {
		args = append(args, start)
		aggregates = append(aggregates, fmt.Sprintf("count(*) FILTER (WHERE b.closed_at >= $%d)", len(args)))
	}
	rows, err := s.db.Query(ctx, "SELECT b.state,"+strings.Join(aggregates, ",")+" FROM native_board_items b WHERE "+where+" GROUP BY b.state ORDER BY b.state LIMIT 201", args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardClosedCount{}
	states := 0
	for rows.Next() {
		states++
		if states > 200 {
			return nil, corestore.ErrInvalidPage
		}
		var state string
		counts := make([]int64, len(starts))
		dest := []any{&state}
		for index := range counts {
			dest = append(dest, &counts[index])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, mapError(ctx, err)
		}
		for index, total := range counts {
			result = append(result, viewstore.BoardClosedCount{Window: index, State: state, Total: total})
		}
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardAccessRevisions(ctx context.Context, credential, user string) ([3]int64, error) {
	var revisions [3]int64
	err := s.db.QueryRow(ctx, `SELECT
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='organization' AND subject_id=$1),0),
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='token' AND subject_id=$2),0),
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='member' AND subject_id=$3),0)`, s.organization, credential, user).Scan(&revisions[0], &revisions[1], &revisions[2])
	return revisions, mapError(ctx, err)
}

func (s *session) BoardAttemptCount(ctx context.Context, issueID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND issue_id=$2`, s.organization, issueID).Scan(&count)
	return count, mapError(ctx, err)
}

const boardWorkOrderPendingSQL = `SELECT EXISTS(SELECT 1 FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND issue_number IS NULL)`

func (s *session) BoardView(ctx context.Context, selection viewstore.BoardViewSelection) (viewstore.BoardViewRows, error) {
	var result viewstore.BoardViewRows
	if len(selection.Projects) > viewstore.BoardViewMaxProjects || len(selection.Items) > viewstore.BoardViewMaxItems || selection.ClosedPage < 1 || selection.ClosedPage > viewstore.BoardViewMaxPage || !viewstore.ValidLanePreset(selection.LanePreset) {
		return result, corestore.ErrInvalidPage
	}
	var after *time.Time
	identifier := ""
	if selection.ClosedAfter != nil {
		after, identifier = selection.ClosedAfter.ClosedAt, selection.ClosedAfter.Identifier
	}
	rows, err := s.db.Query(ctx, boardviewsql.Statement(selection.Reporter != ""), s.organization, selection.ProjectStrings(), selection.AllowedStrings(), selection.Reporter, selection.Archived,
		selection.CredentialID, selection.PrincipalID, selection.UserID, selection.ClosedPage+1, selection.ItemStrings(), len(selection.Items) > 0,
		selection.LanePreset, after, identifier, selection.ClosedAfter != nil, append([]string{}, selection.IncludeLanes...), append([]string{}, selection.ExcludeLanes...))
	if err != nil {
		return result, mapError(ctx, err)
	}
	result, err = boardviewsql.Scan(rows)
	return result, mapError(ctx, err)
}

func (s *session) boardSearchPage(ctx context.Context, f viewstore.BoardSelection, typed bool) ([]viewstore.BoardRecord, error) {
	filter := f
	filter.SearchText, filter.Fingerprint = "", ""
	where, args, err := boardWhere(s.organization, filter)
	if err != nil {
		return nil, err
	}
	if typed {
		where += " AND b.page_ready"
		if f.Compact {
			where += " AND b.list_ready"
		}
	}
	query, args, err := searchsql.Ranked(f, where, args, boardColumns)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardRecord{}
	for rows.Next() {
		var exact bool
		var rank float64
		var number int64
		row, err := scanBoard(rows, &exact, &rank, &number)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		row.Position.SearchExact, row.Position.SearchRank, row.Position.SearchNumber = exact, rank, number
		if f.After != nil && !f.WorkOrder {
			row.Position.SearchWindow, row.Position.SearchFingerprintAfter = f.After.SearchWindow, f.After.SearchFingerprintAfter
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) boardSearchFailure(parent context.Context, selection viewstore.BoardSelection, err error) error {
	if err == nil || errors.Is(err, searchstore.ErrBusy) || selection.SearchText == "" && selection.Fingerprint == "" || parent.Err() != nil || !s.searchBudgetLimits {
		return err
	}
	var statement *pgconn.PgError
	timedOut := errors.As(err, &statement) && statement.Code == "57014" && strings.Contains(statement.Message, "statement timeout")
	if errors.Is(err, context.DeadlineExceeded) || timedOut {
		return fmt.Errorf("board search: %w", errors.Join(searchstore.ErrBusy, err))
	}
	return err
}
