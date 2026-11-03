package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const boardColumns = `b.organization_id,b.project_id,b.work_item_id,b.source_revision,b.source_event_sequence,b.source_project_revision,
b.state,b.archived,b.terminal,b.running,b.dispatchable,b.lane_rank,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at,b.expires_at,b.execution_fence,
b.actor_principal_id,b.issue_type,b.verification_state,b.priority,b.created_at,b.updated_at,b.payload_object_key,b.payload_sha256,b.payload_bytes,b.payload_content_bytes,
ARRAY(SELECT value FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='label' ORDER BY value),
ARRAY(SELECT value FROM native_board_filter_values f WHERE f.organization_id=b.organization_id AND f.project_id=b.project_id AND f.work_item_id=b.work_item_id AND f.kind='assignee' ORDER BY value)`

func scanBoard(row interface{ Scan(...any) error }, extra ...any) (viewstore.BoardRecord, error) {
	var b viewstore.BoardRecord
	var key, digest string
	var size, content int64
	targets := []any{&b.OrganizationID, &b.ProjectID, &b.WorkItemID, &b.SourceRevision, &b.SourceEventSequence, &b.SourceProjectRevision,
		&b.Position.State, &b.Archived, &b.Terminal, &b.Running, &b.Dispatchable, &b.Position.Lane, &b.Position.Priority, &b.Position.ActivityMissing, &b.Position.Activity, &b.Position.Identifier, &b.ClosedAt, &b.ExpiresAt, &b.ExecutionFence,
		&b.ActorPrincipalID, &b.IssueType, &b.VerificationState, &b.Priority, &b.CreatedAt, &b.UpdatedAt, &key, &digest, &size, &content, pq.Array(&b.Labels), pq.Array(&b.Assignees)}
	targets = append(targets, extra...)
	err := row.Scan(targets...)
	b.Payload = eventstore.Reference(key, digest, size, content)
	return b, err
}

func (s *session) NextBoardCandidate(ctx context.Context, after tracker.NativeWorkItemID) (viewstore.BoardCandidate, error) {
	var candidate viewstore.BoardCandidate
	err := s.db.QueryRowContext(ctx, `WITH dirty AS MATERIALIZED (
 (SELECT project_id,work_item_id,0 AS priority FROM native_board_dirty WHERE organization_id=$1 AND work_item_id>$2 ORDER BY work_item_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,1 AS priority FROM native_board_dirty WHERE organization_id=$1 AND work_item_id<=$2 ORDER BY work_item_id LIMIT 1)
 ) SELECT project_id,work_item_id FROM (
 SELECT project_id,work_item_id,priority FROM dirty
 UNION ALL (SELECT project_id,native_id,2 AS priority FROM issues WHERE organization_id=$1 AND native_id>$2 AND NOT EXISTS(SELECT 1 FROM dirty) ORDER BY native_id LIMIT 1)
 UNION ALL (SELECT project_id,work_item_id,2 AS priority FROM native_board_items WHERE organization_id=$1 AND work_item_id>$2 AND NOT EXISTS(SELECT 1 FROM dirty) ORDER BY work_item_id LIMIT 1)
 ) candidates ORDER BY priority,work_item_id LIMIT 1`, s.organization, after).Scan(&candidate.ProjectID, &candidate.WorkItemID)
	return candidate, mapError(ctx, err)
}

func (s *session) BoardItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.BoardRecord, error) {
	row, err := scanBoard(s.db.QueryRowContext(ctx, "SELECT "+boardColumns+" FROM native_board_items b WHERE b.organization_id=$1 AND b.project_id=$2 AND b.work_item_id=$3", s.organization, project, item))
	return row, mapError(ctx, err)
}

func (s *session) boardSelectionWhere(ctx context.Context, selection viewstore.BoardSelection) (string, []any, error) {
	where, args, err := boardWhere(s.organization, selection)
	if err != nil || selection.SearchText == "" && selection.Fingerprint == "" {
		return where, args, err
	}
	var pending bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM issues i LEFT JOIN search_documents d ON d.organization_id=i.organization_id AND d.project_id=i.project_id AND d.work_item_id=i.native_id AND d.kind='issue'
 WHERE i.organization_id=$1 AND i.project_id=$2 AND ($3::boolean IS NULL OR i.archived=$3) AND (NOT $4 OR i.actor_principal_id=$5) AND (d.id IS NULL OR d.placeholder OR d.indexed_revision<>d.revision)
 UNION ALL
 SELECT 1 FROM native_comments c JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id LEFT JOIN search_documents d ON d.organization_id=c.organization_id AND d.project_id=c.project_id AND d.kind='comment' AND d.source_id=c.id
 WHERE i.organization_id=$1 AND i.project_id=$2 AND NOT c.deleted AND ($3::boolean IS NULL OR i.archived=$3) AND (NOT $4 OR i.actor_principal_id=$5) AND (d.id IS NULL OR d.placeholder OR d.indexed_revision<>d.revision)
)`, s.organization, selection.ProjectID, selection.Archived, selection.Reporter, selection.ReporterPrincipalID).Scan(&pending)
	if err != nil {
		return "", nil, mapError(ctx, err)
	}
	if pending {
		return "", nil, searchstore.ErrIndexPending
	}
	return where, args, nil
}

func boardWhere(org tracker.OrganizationID, f viewstore.BoardSelection) (string, []any, error) {
	if f.ProjectID == "" {
		return "", nil, corestore.ErrNotFound
	}
	args := []any{org, f.ProjectID}
	clauses := []string{"b.organization_id=$1", "b.project_id=$2"}
	if f.SearchText != "" {
		query := searchstore.Query{Kind: "work", ProjectID: f.ProjectID, Text: f.SearchText, Limit: 1}
		if err := query.Validate(); err != nil {
			return "", nil, err
		}
		number, name := searchstore.Number(f.SearchText)
		queryTerms := searchstore.LiteralQueryTerms(org, f.ProjectID, f.SearchText)
		args = append(args, strings.Join(searchstore.Expressions(org, query), " | "), searchstore.LiteralHex(f.SearchText), number, searchstore.LiteralHex(name), pq.Array(queryTerms), len([]rune(f.SearchText)) <= 2)
		clauses = append(clauses, `EXISTS(SELECT 1 FROM search_documents d JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE d.organization_id=b.organization_id AND d.project_id=b.project_id AND d.work_item_id=b.work_item_id AND d.kind='issue' AND d.indexed_revision=d.revision AND NOT d.placeholder AND (
d.vector @@ $3::tsquery
OR ($5::bigint>0 AND i.number=$5 AND ($6='' OR $6 IN (encode(convert_to(translate(p.id,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz'),'UTF8'),'hex'),encode(convert_to(translate(p.name,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz'),'UTF8'),'hex'))))
OR `+literalPostingMatch("d", "$7", "$8")+`
OR position(decode($4,'hex') IN convert_to(translate(p.id||'#'||i.number::text,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz'),'UTF8'))>0
OR position(decode($4,'hex') IN convert_to(translate(p.name||'#'||i.number::text,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz'),'UTF8'))>0))`)
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
		args = append(args, pq.Array(ids))
		clauses = append(clauses, fmt.Sprintf("b.work_item_id=ANY($%d::text[])", len(args)))
	}
	if f.Reporter {
		if f.ReporterPrincipalID == "" {
			return "", nil, corestore.ErrNotFound
		}
		args = append(args, f.ReporterPrincipalID)
		clauses = append(clauses, fmt.Sprintf("EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND i.actor_principal_id=$%d)", len(args)))
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
	if f.Fingerprint != "" {
		marker := "\nfingerprint: " + f.Fingerprint + "\n"
		args = append(args, pq.Array(searchstore.LiteralQueryTerms(org, f.ProjectID, marker)))
		terms := fmt.Sprintf("$%d", len(args))
		clauses = append(clauses, `EXISTS(SELECT 1 FROM search_documents d WHERE d.organization_id=b.organization_id AND d.project_id=b.project_id AND d.work_item_id=b.work_item_id AND d.kind='issue' AND d.indexed_revision=d.revision AND NOT d.placeholder AND `+literalPostingMatch("d", terms, "false")+`)`)
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
			args = append(args, pq.Array(field.values))
			clauses = append(clauses, fmt.Sprintf("%s=ANY($%d::text[])", field.column, len(args)))
		}
	}
	if len(f.Priorities) > 4 {
		return "", nil, corestore.ErrInvalidPage
	}
	if len(f.Priorities) > 0 {
		args = append(args, pq.Array(f.Priorities))
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
			args = append(args, field.kind, pq.Array(field.values))
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

func literalPostingMatch(document, terms, short string) string {
	return `(CASE WHEN ` + short + `::boolean THEN EXISTS(SELECT 1 FROM search_postings lp WHERE lp.organization_id=` + document + `.organization_id AND lp.document_id=` + document + `.id AND lp.term=ANY(` + terms + `::text[])) ELSE EXISTS(
SELECT 1 FROM (
 SELECT p.literal_positions,q.ordinality AS query_position FROM unnest(` + terms + `::text[]) WITH ORDINALITY q(term,ordinality)
 JOIN search_postings p ON p.organization_id=` + document + `.organization_id AND p.document_id=` + document + `.id AND p.term=q.term
 ORDER BY cardinality(p.literal_positions),q.ordinality LIMIT 1
) anchor CROSS JOIN LATERAL unnest(anchor.literal_positions) anchor_position(position)
WHERE NOT EXISTS(SELECT 1 FROM unnest(` + terms + `::text[]) WITH ORDINALITY q(term,ordinality)
 WHERE NOT EXISTS(SELECT 1 FROM search_postings p WHERE p.organization_id=` + document + `.organization_id AND p.document_id=` + document + `.id AND p.term=q.term
 AND p.literal_positions @> ARRAY[anchor_position.position+q.ordinality-anchor.query_position]::bigint[]))
) END)`
}

func (s *session) BoardPage(ctx context.Context, f viewstore.BoardSelection) ([]viewstore.BoardRecord, error) {
	if err := checkPage(f.Limit); err != nil {
		return nil, err
	}
	where, args, err := s.boardSelectionWhere(ctx, f)
	if err != nil {
		return nil, err
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
	searchRanked := f.SearchText != "" && f.SearchRanked
	var exactProjection, rankProjection string
	if searchRanked {
		query := searchstore.Query{Kind: "work", ProjectID: f.ProjectID, Text: f.SearchText, Limit: 1}
		expression := strings.Join(searchstore.Expressions(s.organization, query), " | ")
		args = append(args, expression)
		expressionArg := len(args)
		number, name := searchstore.Number(f.SearchText)
		args = append(args, number, name)
		numberArg, nameArg := len(args)-1, len(args)
		exactProjection = fmt.Sprintf("CASE WHEN $%d>0 AND i.number=$%d AND ($%d='' OR lower($%d)=lower(i.project_id) OR lower($%d)=lower(p.name)) THEN true ELSE false END", numberArg, numberArg, nameArg, nameArg, nameArg)
		rankProjection = fmt.Sprintf("coalesce((SELECT max(ts_rank_cd(d.vector,$%d::tsquery,32)) FROM search_documents d WHERE d.organization_id=b.organization_id AND d.project_id=b.project_id AND d.work_item_id=b.work_item_id AND d.kind='issue' AND d.indexed_revision=d.revision AND NOT d.placeholder AND d.vector @@ $%d::tsquery),0)::double precision", expressionArg, expressionArg)
		order = "issue_search_exact DESC,issue_search_rank DESC"
		if f.WorkOrder {
			order += ",b.running DESC,b.dispatchable DESC,i.number DESC"
		} else if f.ClosedOrder {
			order += ",coalesce(b.closed_at,'-infinity'::timestamptz) DESC"
		} else {
			order += ",i.number DESC"
		}
		order += ",b.identifier"
	}
	if f.WorkOrder && !searchRanked {
		order = "b.running DESC,b.dispatchable DESC,i.number DESC,b.identifier"
	}
	if p := f.After; p != nil {
		base := len(args)
		if searchRanked {
			args = append(args, p.SearchExact, p.SearchRank, p.SearchNumber, p.Identifier)
			exactArg, rankArg, numberArg, idArg := base+1, base+2, base+3, base+4
			exactExpr, rankExpr := "("+exactProjection+")", "("+rankProjection+")"
			if f.ClosedOrder {
				args = append(args, p.Activity)
				atArg := len(args)
				where += fmt.Sprintf(" AND (%s<$%d OR (%s=$%d AND (%s<$%d OR (%s=$%d AND (coalesce(b.closed_at,'-infinity'::timestamptz)<coalesce($%d::timestamptz,'-infinity'::timestamptz) OR (coalesce(b.closed_at,'-infinity'::timestamptz)=coalesce($%d::timestamptz,'-infinity'::timestamptz) AND b.identifier>$%d))))))", exactExpr, exactArg, exactExpr, exactArg, rankExpr, rankArg, rankExpr, rankArg, atArg, atArg, idArg)
			} else {
				where += fmt.Sprintf(" AND (%s<$%d OR (%s=$%d AND (%s<$%d OR (%s=$%d AND (i.number<$%d OR (i.number=$%d AND b.identifier>$%d))))))", exactExpr, exactArg, exactExpr, exactArg, rankExpr, rankArg, rankExpr, rankArg, numberArg, numberArg, idArg)
			}
		} else if f.ClosedOrder {
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
	projection := boardColumns
	from := " FROM native_board_items b"
	if searchRanked {
		projection += "," + exactProjection + " AS issue_search_exact," + rankProjection + " AS issue_search_rank,i.number"
		from += " JOIN issues i ON i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id"
	} else if f.WorkOrder {
		from += " JOIN issues i ON i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+projection+from+" WHERE "+where+" ORDER BY "+order+fmt.Sprintf(" LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardRecord{}
	for rows.Next() {
		var row viewstore.BoardRecord
		if searchRanked {
			var exact bool
			var rank float64
			var number int64
			row, err = scanBoard(rows, &exact, &rank, &number)
			row.Position.SearchExact, row.Position.SearchRank, row.Position.SearchNumber = exact, rank, number
		} else {
			row, err = scanBoard(rows)
		}
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BoardLanes(ctx context.Context, f viewstore.BoardSelection, now time.Time) ([]viewstore.BoardLane, error) {
	f.States = nil
	f.Terminal = nil
	where, args, err := s.boardSelectionWhere(ctx, f)
	if err != nil {
		return nil, err
	}
	args = append(args, now)
	current := fmt.Sprintf("b.running AND b.expires_at>$%d AND EXISTS(SELECT 1 FROM issues i JOIN leases l ON l.organization_id=i.organization_id AND l.issue_id=i.id WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND l.fencing_token=b.execution_fence AND l.released_at IS NULL AND l.renewed_at<=$%d AND l.expires_at>$%d)", len(args), len(args), len(args))
	statement := "SELECT b.state,count(*),count(*) FILTER (WHERE " + current + ") FROM native_board_items b WHERE " + where + " GROUP BY b.state ORDER BY b.state LIMIT 201"
	filtered := f.SearchText != "" || f.WorkItemIDs != nil || f.Reporter || f.ActorPrincipalID != "" || f.ExcludeWorkspace || len(f.Types)+len(f.Verification)+len(f.Labels)+len(f.Assignees)+len(f.Priorities) > 0 || f.CreatedAfter != nil || f.CreatedBefore != nil || f.UpdatedAfter != nil || f.NativeCreatedAfter != nil || f.NativeCreatedBefore != nil || f.NativeUpdatedAfter != nil || f.CompletedFrom != nil || f.CompletedTo != nil
	if !filtered {
		counterWhere := "c.organization_id=$1 AND c.project_id=$2 AND c.total>0"
		if f.Archived != nil {
			counterWhere += " AND c.archived=$3"
		}
		statement = "SELECT c.state,sum(c.total)::bigint,sum((SELECT count(*) FROM native_board_items b WHERE b.organization_id=c.organization_id AND b.project_id=c.project_id AND b.archived=c.archived AND b.state=c.state AND " + current + "))::bigint FROM native_board_counts c WHERE " + counterWhere + " GROUP BY c.state ORDER BY c.state LIMIT 201"
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
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
	err := s.db.QueryRowContext(ctx, "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=$1 AND project_id=$2", s.organization, project).Scan(&result.Sequence, &result.ReplayFloor)
	if err == sql.ErrNoRows {
		return result, nil
	}
	return result, mapError(ctx, err)
}

func (s *session) BoardDeltas(ctx context.Context, project tracker.ProjectID, after int64, limit int) ([]viewstore.BoardDelta, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,work_item_id,previous_object_key,previous_sha256,previous_bytes,previous_content_bytes,current_object_key,current_sha256,current_bytes,current_content_bytes FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2 AND sequence>$3 ORDER BY sequence LIMIT $4`, s.organization, project, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.BoardDelta{}
	for rows.Next() {
		var row viewstore.BoardDelta
		var beforeKey, beforeHash, afterKey, afterHash *string
		var beforeSize, beforeContent, afterSize, afterContent *int64
		if err := rows.Scan(&row.Sequence, &row.WorkItemID, &beforeKey, &beforeHash, &beforeSize, &beforeContent, &afterKey, &afterHash, &afterSize, &afterContent); err != nil {
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

func (s *session) BoardClosedWindows(ctx context.Context, selection viewstore.BoardSelection, starts []time.Time, until time.Time) ([]viewstore.BoardClosedCount, error) {
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
	rows, err := s.db.QueryContext(ctx, "SELECT b.state,"+strings.Join(aggregates, ",")+" FROM native_board_items b WHERE "+where+" GROUP BY b.state ORDER BY b.state LIMIT 201", args...)
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
	err := s.db.QueryRowContext(ctx, `SELECT
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='organization' AND subject_id=$1),0),
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='token' AND subject_id=$2),0),
 coalesce((SELECT revision FROM native_board_access_revisions WHERE organization_id=$1 AND subject_kind='member' AND subject_id=$3),0)`, s.organization, credential, user).Scan(&revisions[0], &revisions[1], &revisions[2])
	return revisions, mapError(ctx, err)
}

func (s *session) BoardAttemptCount(ctx context.Context, issueID int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND issue_id=$2`, s.organization, issueID).Scan(&count)
	return count, mapError(ctx, err)
}
